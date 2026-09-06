#!/usr/bin/env python3
"""Small systemd release controller. Requires root and python3-yaml on the host."""
import argparse
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import time
import urllib.request

import yaml

ROOT = Path('/opt/cliproxyapi/releases')
STATE = Path('/var/lib/cliproxy-deploy')
CONFIG = Path('/etc/cliproxyapi/config.yaml')
BINARY = Path('/opt/cliproxyapi/bin/cliproxyapi')
SERVICE = 'cliproxyapi.service'
LIB = Path('/usr/local/lib/cliproxy-deploy')
STAGING = Path('/var/lib/cliproxyapi-staging')


def run(*args):
    return subprocess.run(args, check=True, text=True, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE).stdout.strip()


def atomic(path, data, mode=0o600):
    path = Path(path)
    tmp = path.with_name(path.name + '.new')
    with open(tmp, 'w') as f:
        os.fchmod(f.fileno(), mode)
        f.write(data)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)
    fd = os.open(path.parent, os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def link(path, target):
    tmp = path.with_name(path.name + '.new')
    tmp.unlink(missing_ok=True)
    tmp.symlink_to(target)
    os.replace(tmp, path)
    fd = os.open(path.parent, os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def digest(path):
    with open(path, 'rb') as f:
        return hashlib.file_digest(f, 'sha256').hexdigest()


def manifest(path):
    result = {}
    for f in sorted(path.rglob('*')):
        if f.is_symlink():
            raise ValueError('Release symlinks are not supported')
        if f.is_file():
            result[str(f.relative_to(path))] = digest(f)
    return result


def release(name):
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._-]{0,100}', name):
        raise ValueError('Invalid release ID')
    return ROOT / name


def config_data():
    return yaml.safe_load(CONFIG.read_text()) or {}


def snapshot_config(target):
    cfg = config_data()
    plugins = cfg.setdefault('plugins', {})
    plugin_dir = Path(plugins.get('dir') or 'plugins')
    if str(plugin_dir).startswith('~'):
        raise ValueError('Use an absolute plugins.dir before installing rollout tooling')
    if not plugin_dir.is_absolute():
        # The reference service's WorkingDirectory is /var/lib/cliproxyapi.
        plugin_dir = Path('/var/lib/cliproxyapi') / plugin_dir
    if plugin_dir.exists():
        shutil.copytree(plugin_dir, target / 'plugins', dirs_exist_ok=True)
    else:
        (target / 'plugins').mkdir(exist_ok=True)
    plugins['dir'] = str(target / 'plugins')
    atomic(target / 'config.yaml', yaml.safe_dump(cfg, sort_keys=False), 0o640)
    shutil.chown(target / 'config.yaml', group='nogroup')


def init():
    ROOT.mkdir(parents=True, exist_ok=True)
    STATE.mkdir(parents=True, exist_ok=True, mode=0o700)
    if (STATE / 'current').exists():
        return
    name = 'baseline-' + time.strftime('%Y%m%dT%H%M%S')
    base = release(name)
    base.mkdir()
    shutil.copy2(BINARY, base / 'cliproxyapi')
    snapshot_config(base)
    seal(base)
    atomic(STATE / 'current', name)
    # Do not change the running service or its paths during setup.
    print('Captured baseline:', name)


def seal(path):
    # Service can read/execute artifacts but cannot replace deployment records.
    for p in [path, *path.rglob('*')]:
        if p.is_symlink():
            raise ValueError('Release symlinks are not supported')
        shutil.chown(p, user='root', group='nogroup')
        p.chmod(0o750 if p.is_dir() or p.name == 'cliproxyapi' else 0o640)


def stage(bundle):
    src = Path(bundle).resolve()
    name = src.name
    target = release(name)
    if target.exists():
        raise ValueError('Release already exists; use a fresh bundle ID')
    expected = json.loads((src / 'manifest.json').read_text())
    actual = manifest(src)
    actual.pop('manifest.json', None)
    if expected != actual or 'cliproxyapi' not in actual:
        raise ValueError('Bundle checksum mismatch')
    if any(k not in {'cliproxyapi', 'plugins/tenancy-scheduler.so'} for k in actual):
        raise ValueError('Unexpected bundle member')
    # Copy production plugins/config for rollback consistency; overlay new libraries.
    source_config_hash = digest(CONFIG)
    target.mkdir()
    snapshot_config(target)
    shutil.copy2(src / 'cliproxyapi', target / 'cliproxyapi')
    if (src / 'plugins').exists():
        # Platform-specific discovery must not shadow the newly supplied library.
        for incoming in (src / 'plugins').glob('*.so'):
            for previous in (target / 'plugins').rglob(incoming.name):
                previous.unlink()
        shutil.copytree(src / 'plugins', target / 'plugins', dirs_exist_ok=True)
    if digest(CONFIG) != source_config_hash:
        raise ValueError('Production config changed while taking staging snapshot')
    if any(digest(target / member) != checksum for member, checksum in expected.items()):
        raise ValueError('Bundle changed during import')
    seal(target)
    # Staging gets only executable artifacts, never production configuration.
    stage_root = STAGING / name
    stage_root.mkdir()
    artifacts = stage_root / 'artifacts'
    artifacts.mkdir()
    shutil.copy2(target / 'cliproxyapi', artifacts / 'cliproxyapi')
    (artifacts / 'plugins').mkdir()
    library = target / 'plugins/tenancy-scheduler.so'
    if library.exists():
        shutil.copy2(library, artifacts / 'plugins/tenancy-scheduler.so')
    seal(stage_root)
    work = stage_root / 'work'
    work.mkdir(mode=0o700)
    shutil.chown(work, user='nobody', group='nogroup')
    artifact_hashes = manifest(target)
    atomic(STATE / (name + '.source-config'), source_config_hash)
    try:
        run('systemctl', 'start', 'cliproxyapi-stage@' + name + '.service')
        result = run('systemctl', 'show', 'cliproxyapi-stage@' + name + '.service', '-p', 'Result', '--value')
        if result != 'success':
            raise RuntimeError('Staging service failed')
        if manifest(target) != artifact_hashes:
            raise RuntimeError('Release changed during staging')
        atomic(STATE / (name + '.staged.json'), json.dumps(artifact_hashes))
        print('Staging passed:', name)
    except Exception:
        print('Staging failed; production unchanged. See staging unit journal.', file=sys.stderr)
        raise


def switch(name):
    target = release(name)
    data = (target / 'config.yaml').read_text()
    # Call only while production is stopped: config watcher must not see a mixed release.
    atomic(CONFIG, data)
    shutil.chown(CONFIG, user='nobody', group='nogroup')
    link(BINARY, target / 'cliproxyapi')


def properties():
    output = run('systemctl', 'show', SERVICE, '-p', 'ActiveState', '-p', 'MainPID', '-p', 'NRestarts')
    return dict(line.split('=', 1) for line in output.splitlines())


def probe(url, key_file, target, initial_restarts):
    props = properties()
    if props['ActiveState'] != 'active' or int(props['MainPID']) <= 0:
        return False
    if int(props['NRestarts']) != initial_restarts:
        return False
    if digest(Path('/proc') / props['MainPID'] / 'exe') != digest(target / 'cliproxyapi'):
        return False
    # Deployment probes have bounded deadlines, not production upstream requests.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(url + '/healthz', timeout=5) as response:
        if json.load(response).get('status') != 'ok':
            return False
    key = Path(key_file).read_text().strip()
    request = urllib.request.Request(url + '/v1/models', headers={'Authorization': 'Bearer ' + key})
    with opener.open(request, timeout=5) as response:
        return bool(json.load(response).get('data'))


def monitor(name, settings):
    initial_restarts = int(properties()['NRestarts'])
    if initial_restarts:
        raise RuntimeError('Candidate restarted during startup')
    deadline = time.monotonic() + settings.get('observe_seconds', 180)
    failures = 0
    successes = 0
    while time.monotonic() < deadline:
        try:
            ok = probe(settings['url'], settings['key_file'], release(name), initial_restarts)
        except Exception:
            ok = False
        failures = 0 if ok else failures + 1
        successes += int(ok)
        if failures >= 3:
            raise RuntimeError('Deployment checks failed three times consecutively')
        time.sleep(settings.get('interval_seconds', 5))
    if failures or successes < 3:
        raise RuntimeError('Insufficient successful deployment checks')


def recover(boot=False):
    pending = STATE / 'pending.json'
    if not pending.exists():
        return
    record = json.loads(pending.read_text())
    previous = record['previous']
    # Never retry the rejected candidate. Keep pending on any recovery failure.
    if not boot:
        run('systemctl', 'stop', SERVICE)
    switch(previous)
    atomic(STATE / 'current', previous)
    if not boot:
        run('systemctl', 'reset-failed', SERVICE)
        run('systemctl', 'start', SERVICE)
        settings = json.loads((STATE / 'settings.json').read_text())
        restarts = int(properties()['NRestarts'])
        for _ in range(12):
            try:
                if probe(settings['url'], settings['key_file'], release(previous), restarts):
                    break
            except Exception:
                pass
            time.sleep(5)
        else:
            raise RuntimeError('Rollback restored files but recovery checks failed; operator action required')
    atomic(STATE / 'last-result.json', json.dumps({**record, 'status': 'rolled-back', 'boot': boot}))
    pending.unlink()
    print('Rolled back to:', previous, flush=True)


def rollback():
    if (STATE / 'pending.json').exists():
        return recover()
    last = json.loads((STATE / 'last-result.json').read_text())
    if last['status'] != 'accepted' or (STATE / 'current').read_text().strip() != last['candidate']:
        raise ValueError('No accepted deployment available for manual rollback')
    atomic(STATE / 'pending.json', json.dumps(last))
    recover()


def promote(name):
    target = release(name)
    if (STATE / 'pending.json').exists():
        raise RuntimeError('Unresolved deployment exists; recover it first')
    expected = json.loads((STATE / (name + '.staged.json')).read_text())
    if manifest(target) != expected:
        raise ValueError('Staged release changed')
    if digest(CONFIG) != (STATE / (name + '.source-config')).read_text():
        raise ValueError('Production config changed after staging; stage a fresh release')
    settings = json.loads((STATE / 'settings.json').read_text())
    if not 30 <= settings.get('observe_seconds', 180) <= 300 or not 1 <= settings.get('interval_seconds', 5) <= 10:
        raise ValueError('Observation must be 30..300 seconds; interval must be 1..10 seconds')
    if not Path(settings['key_file']).read_text().strip():
        raise ValueError('Missing production probe API key')
    # Verify the old service before any disruptive operation.
    previous = (STATE / 'current').read_text().strip()
    if not probe(settings['url'], settings['key_file'], release(previous), int(properties()['NRestarts'])):
        raise RuntimeError('Existing production checks failed; refusing rollout')
    # Capture the live pre-deployment config and plugins, including management edits.
    rollback = release('rollback-' + name)
    if rollback.exists():
        raise ValueError('Rollback ID already exists')
    rollback.mkdir()
    shutil.copy2(BINARY, rollback / 'cliproxyapi')
    snapshot_config(rollback)
    seal(rollback)
    record = {'candidate': name, 'previous': rollback.name, 'started_at': time.time()}
    atomic(STATE / 'pending.json', json.dumps(record))
    try:
        run('systemctl', 'stop', SERVICE)
        switch(name)
        run('systemctl', 'reset-failed', SERVICE)
        run('systemctl', 'start', SERVICE)
        monitor(name, settings)
        atomic(STATE / 'current', name)
        atomic(STATE / 'last-result.json', json.dumps({**record, 'status': 'accepted'}))
        (STATE / 'pending.json').unlink()
        print('Accepted:', name, flush=True)
    except BaseException:
        recover()
        raise


@contextlib.contextmanager
def locked():
    STATE.mkdir(parents=True, exist_ok=True, mode=0o700)
    with open(STATE / 'lock', 'w') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['init', 'stage', 'promote', 'recover', 'recover-boot', 'rollback', 'status'])
    parser.add_argument('argument', nargs='?')
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error('Run the installed controller with sudo')
    if args.action == 'promote' and not os.environ.get('INVOCATION_ID'):
        parser.error('Promote via systemctl start cliproxyapi-deploy@<release>.service')
    def interrupted(_signum, _frame):
        raise RuntimeError('Deployment interrupted')
    signal.signal(signal.SIGTERM, interrupted)
    with locked():
        if args.action == 'init': init()
        elif args.action == 'stage': stage(args.argument)
        elif args.action == 'promote': promote(args.argument)
        elif args.action == 'recover': recover()
        elif args.action == 'rollback': rollback()
        elif args.action == 'recover-boot': recover(boot=True)
        else:
            for name in ['current', 'pending.json', 'last-result.json']:
                path = STATE / name
                print(name + ':', path.read_text() if path.exists() else 'none')


if __name__ == '__main__':
    try:
        main()
    except Exception as exc:
        # Do not dump subprocess output or configuration that may contain credentials.
        print('Rollout failed:', type(exc).__name__, str(exc) if not isinstance(exc, subprocess.CalledProcessError) else 'systemd command failed; inspect unit journal', file=sys.stderr)
        sys.exit(1)
