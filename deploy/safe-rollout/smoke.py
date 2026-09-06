#!/usr/bin/env python3
"""Isolated staging: real proxy, local mock upstream, ordinary and SSE requests."""
import ctypes as c
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import sys
import threading
import time
import urllib.request

import yaml


class Upstream(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        self.send_response(200)
        if body.get('stream'):
            self.send_header('Content-Type', 'text/event-stream')
            self.end_headers()
            chunk = {'id': 'staging', 'object': 'chat.completion.chunk', 'model': 'staging-model',
                     'choices': [{'index': 0, 'delta': {'role': 'assistant', 'content': 'staging-ok'}, 'finish_reason': None}]}
            self.wfile.write(('data: ' + json.dumps(chunk) + '\n\ndata: [DONE]\n\n').encode())
        else:
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(json.dumps({'id': 'staging', 'object': 'chat.completion', 'model': 'staging-model',
                'choices': [{'index': 0, 'message': {'role': 'assistant', 'content': 'staging-ok'}, 'finish_reason': 'stop'}],
                'usage': {'prompt_tokens': 1, 'completion_tokens': 1, 'total_tokens': 2}}).encode())


def check_plugin(path):
    class Buffer(c.Structure):
        _fields_ = [('ptr', c.c_void_p), ('length', c.c_size_t)]
    call_type = c.CFUNCTYPE(c.c_int, c.c_char_p, c.c_void_p, c.c_size_t, c.POINTER(Buffer))
    free_type = c.CFUNCTYPE(None, c.c_void_p, c.c_size_t)
    class API(c.Structure):
        _fields_ = [('version', c.c_uint32), ('call', call_type), ('free', free_type), ('shutdown', c.CFUNCTYPE(None))]
    lib = c.CDLL(str(path))
    lib.cliproxy_plugin_init.argtypes = [c.c_void_p, c.POINTER(API)]
    api = API()
    assert lib.cliproxy_plugin_init(None, c.byref(api)) == 0
    assert api.version == 1
    def invoke(method, payload):
        raw = json.dumps(payload).encode()
        buf = Buffer()
        assert api.call(method.encode(), raw, len(raw), c.byref(buf)) == 0
        try:
            result = json.loads(c.string_at(buf.ptr, buf.length))
            assert result['ok']
            return result['result']
        finally:
            api.free(buf.ptr, buf.length)
    assert invoke('plugin.register', {})['capabilities']['scheduler']
    assert invoke('scheduler.pick', {'Candidates': [
        {'ID': 'private', 'Metadata': {'shared': False}},
        {'ID': 'shared', 'Metadata': {'shared': True}}]})['AuthID'] == 'shared'
    assert not invoke('scheduler.pick', {'Candidates': [
        {'ID': 'private', 'Metadata': {'shared': False}}]})['Handled']
    api.shutdown()


def smoke(release, work):
    work.mkdir(parents=True, exist_ok=True)
    (work / 'auths').mkdir(exist_ok=True)
    plugins = work / 'plugins'
    plugins.mkdir(exist_ok=True)
    library = release / 'plugins' / 'tenancy-scheduler.so'
    if library.exists():
        shutil.copy2(library, plugins / library.name)
        check_plugin(plugins / library.name)
    upstream = ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1', 0))
        port = reservation.getsockname()[1]
    key = secrets.token_urlsafe(32)
    cfg = {'host': '127.0.0.1', 'port': port, 'auth-dir': str(work / 'auths'), 'api-keys': [key],
           'remote-management': {'disable-control-panel': True, 'disable-auto-update-panel': True},
           'tenancy': {'enabled': True, 'db-path': str(work / 'tenancy.db'), 'user-panel': {'disable-auto-update': True}},
           'plugins': {'enabled': library.exists(), 'dir': str(plugins), 'configs': {'tenancy-scheduler': {'enabled': True, 'priority': 100}}},
           'openai-compatibility': [{'name': 'staging', 'base-url': 'http://127.0.0.1:%d/v1' % upstream.server_port,
                                     'api-key-entries': [{'api-key': 'mock-only'}],
                                     'models': [{'name': 'staging-model', 'alias': 'staging-model'}]}]}
    config = work / 'config.yaml'
    config.write_text(yaml.safe_dump(cfg))
    config.chmod(0o600)
    # Inherit no production .env, auth-store backend, or panel environment.
    env = {'PATH': '/usr/local/bin:/usr/bin:/bin', 'HOME': str(work), 'LANG': 'C.UTF-8'}
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    def request(path, body=None):
        raw = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request('http://127.0.0.1:%d%s' % (port, path), data=raw,
                                     headers={'Authorization': 'Bearer ' + key, 'Content-Type': 'application/json'})
        with opener.open(req, timeout=10) as response:
            return response.read()
    with open(work / 'server.log', 'w') as log:
        process = subprocess.Popen([str(release / 'cliproxyapi'), '-config', str(config), '-local-model'],
                                   cwd=work, env=env, stdout=log, stderr=log)
        try:
            for _ in range(100):
                if process.poll() is not None:
                    raise RuntimeError('Staging proxy exited; inspect protected server.log')
                try:
                    if json.loads(request('/healthz'))['status'] == 'ok': break
                except (OSError, ValueError): pass
                time.sleep(0.1)
            else: raise RuntimeError('Staging proxy did not start')
            models = json.loads(request('/v1/models'))['data']
            assert any(m['id'] == 'staging-model' for m in models)
            body = {'model': 'staging-model', 'messages': [{'role': 'user', 'content': 'test'}]}
            reply = json.loads(request('/v1/chat/completions', body))
            assert reply['choices'][0]['message']['content'] == 'staging-ok'
            stream = request('/v1/chat/completions', {**body, 'stream': True})
            assert b'staging-ok' in stream and b'[DONE]' in stream
            print('PASS: native plugin, startup, authenticated models, completion, SSE', flush=True)
        finally:
            process.terminate()
            try: process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
            upstream.shutdown()


if __name__ == '__main__':
    smoke(Path(sys.argv[1]).resolve(), Path(sys.argv[2]).resolve())
