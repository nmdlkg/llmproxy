#!/usr/bin/env python3
"""Check whether each Codex credential is actually entitled to a model.

The proxy's model catalog is plan-derived and advertises a model as soon as the
plan section lists it. That is a local claim, not proof that the upstream
account may call the model. This script bypasses the proxy and sends one
minimal request per credential directly to the Codex backend, so an
entitlement rejection (401/403/404, model_not_found) can be told apart from a
capacity failure (429/5xx, server_is_overloaded).

Read-only: it never writes credential files and never mutates proxy state.

Usage:
  sudo python3 check_codex_model_entitlement.py --auth-dir /var/lib/cliproxyapi/auths
  sudo python3 check_codex_model_entitlement.py --model gpt-6-astra --retries 3

Access tokens are never printed. Output is a per-credential verdict table.
"""

from __future__ import annotations

import argparse
import base64
import json
import ssl
import sys
import time
import urllib.error
import urllib.request
import uuid
from dataclasses import dataclass
from pathlib import Path

DEFAULT_AUTH_DIR = "/var/lib/cliproxyapi/auths"
DEFAULT_MODEL = "gpt-6-astra"
BASE_URL = "https://chatgpt.com/backend-api/codex"
# Mirrors internal/runtime/executor/codex_executor_request.go.
USER_AGENT = (
    "codex-tui/0.135.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 "
    "(codex-tui; 0.135.0)"
)
ORIGINATOR = "codex-tui"

ENTITLEMENT_CODES = {
    "model_not_found",
    "model_not_supported",
    "insufficient_quota",
    "invalid_api_key",
    "permission_denied",
    "unsupported_model",
}
CAPACITY_CODES = {
    "server_is_overloaded",
    "rate_limit_exceeded",
    "usage_limit_reached",
    "slow_down",
}


@dataclass
class Credential:
    path: Path
    plan_type: str
    account_id: str
    email: str
    access_token: str


def decode_jwt_claims(token: str) -> dict:
    parts = token.split(".")
    if len(parts) != 3:
        return {}
    payload = parts[1]
    payload += "=" * (-len(payload) % 4)
    try:
        return json.loads(base64.urlsafe_b64decode(payload))
    except Exception:
        return {}


def load_credentials(auth_dir: Path) -> tuple[list[Credential], list[str]]:
    creds: list[Credential] = []
    skipped: list[str] = []
    if not auth_dir.is_dir():
        raise SystemExit(f"auth dir not found or not readable: {auth_dir}")

    for path in sorted(auth_dir.glob("*.json")):
        try:
            data = json.loads(path.read_text())
        except Exception as exc:
            skipped.append(f"{path.name}: unreadable ({exc.__class__.__name__})")
            continue
        if not isinstance(data, dict):
            skipped.append(f"{path.name}: not a JSON object")
            continue
        if str(data.get("type", "")).strip().lower() != "codex":
            continue

        access_token = str(data.get("access_token") or "").strip()
        if not access_token:
            skipped.append(f"{path.name}: no access_token (api-key or partial file)")
            continue

        # plan_type and account id come from the id_token, matching
        # internal/watcher/synthesizer/file.go.
        claims = decode_jwt_claims(str(data.get("id_token") or ""))
        auth_info = claims.get("https://api.openai.com/auth", {}) or {}
        plan_type = str(auth_info.get("chatgpt_plan_type") or "").strip() or "unknown"
        account_id = str(
            auth_info.get("chatgpt_account_id") or data.get("account_id") or ""
        ).strip()

        creds.append(
            Credential(
                path=path,
                plan_type=plan_type,
                account_id=account_id,
                email=str(data.get("email") or "").strip(),
                access_token=access_token,
            )
        )
    return creds, skipped


def build_request(cred: Credential, model: str) -> urllib.request.Request:
    body = {
        "model": model,
        "instructions": "",
        "input": [
            {
                "role": "user",
                "content": [{"type": "input_text", "text": "say ok"}],
            }
        ],
        "stream": True,
        "store": False,
    }
    req = urllib.request.Request(
        f"{BASE_URL}/responses",
        data=json.dumps(body).encode(),
        method="POST",
    )
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", f"Bearer {cred.access_token}")
    req.add_header("Accept", "text/event-stream")
    req.add_header("User-Agent", USER_AGENT)
    req.add_header("Originator", ORIGINATOR)
    req.add_header("Session_id", str(uuid.uuid4()))
    if cred.account_id:
        req.add_header("Chatgpt-Account-Id", cred.account_id)
    return req


def extract_error(payload: str) -> tuple[str, str]:
    try:
        parsed = json.loads(payload)
    except Exception:
        return "", payload.strip()[:200]
    err = parsed.get("error")
    if isinstance(err, dict):
        code = str(err.get("code") or err.get("type") or "").strip()
        message = str(err.get("message") or "").strip()
        return code, message[:200]
    if isinstance(parsed.get("detail"), str):
        return "", parsed["detail"][:200]
    return "", payload.strip()[:200]


def classify(status: int, code: str, message: str) -> str:
    lowered = f"{code} {message}".lower()
    if code in CAPACITY_CODES or "overload" in lowered or "capacity" in lowered:
        return "CAPACITY"
    if code in ENTITLEMENT_CODES:
        return "NOT_ENTITLED"
    if status in (401, 403, 404):
        return "NOT_ENTITLED"
    if status in (408, 429) or status >= 500:
        return "CAPACITY"
    if 200 <= status < 300:
        return "ENTITLED"
    return "UNKNOWN"


def probe(cred: Credential, model: str, timeout: float, ctx: ssl.SSLContext):
    req = build_request(cred, model)
    try:
        with urllib.request.urlopen(req, timeout=timeout, context=ctx) as resp:
            # A streaming 2xx proves the model was accepted. Do not drain the
            # whole response; the first bytes are enough.
            resp.read(2048)
            return resp.status, "", ""
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8", "replace")
        code, message = extract_error(raw)
        return exc.code, code, message
    except urllib.error.URLError as exc:
        return 0, "network_error", str(exc.reason)[:200]
    except Exception as exc:  # noqa: BLE001
        return 0, "client_error", f"{exc.__class__.__name__}: {exc}"[:200]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--auth-dir", default=DEFAULT_AUTH_DIR)
    parser.add_argument("--model", default=DEFAULT_MODEL)
    parser.add_argument(
        "--retries",
        type=int,
        default=3,
        help="attempts per credential; capacity errors are retried, "
        "entitlement errors are not",
    )
    parser.add_argument("--delay", type=float, default=5.0)
    parser.add_argument("--timeout", type=float, default=45.0)
    parser.add_argument("--json", action="store_true", help="emit JSON")
    args = parser.parse_args()

    creds, skipped = load_credentials(Path(args.auth_dir))
    if not creds:
        print(f"No usable codex OAuth credentials in {args.auth_dir}", file=sys.stderr)
        for note in skipped:
            print(f"  skipped {note}", file=sys.stderr)
        return 1

    ctx = ssl.create_default_context()
    results = []
    for cred in creds:
        verdict = "UNKNOWN"
        status = 0
        code = ""
        message = ""
        attempts = 0
        for attempt in range(1, max(1, args.retries) + 1):
            attempts = attempt
            status, code, message = probe(cred, args.model, args.timeout, ctx)
            verdict = classify(status, code, message)
            # Only a capacity failure is worth retrying. An entitlement
            # rejection is stable and retrying it just adds load.
            if verdict != "CAPACITY":
                break
            if attempt < args.retries:
                time.sleep(args.delay)
        results.append(
            {
                "file": cred.path.name,
                "plan_type": cred.plan_type,
                "email": cred.email,
                "verdict": verdict,
                "http_status": status,
                "error_code": code,
                "error_message": message,
                "attempts": attempts,
            }
        )

    if args.json:
        print(json.dumps({"model": args.model, "results": results}, indent=2))
        return 0

    print(f"model: {args.model}")
    print(f"auth dir: {args.auth_dir}")
    print()
    width = max((len(r["file"]) for r in results), default=4)
    print(f"{'FILE'.ljust(width)}  {'PLAN':<10} {'VERDICT':<13} {'HTTP':<5} ERROR")
    print("-" * (width + 45))
    for r in results:
        detail = r["error_code"] or ""
        if r["error_message"] and not detail:
            detail = r["error_message"][:60]
        elif r["error_message"]:
            detail = f"{detail}: {r['error_message'][:50]}"
        print(
            f"{r['file'].ljust(width)}  {r['plan_type']:<10} "
            f"{r['verdict']:<13} {r['http_status']:<5} {detail}"
        )

    print()
    entitled = sorted({r["plan_type"] for r in results if r["verdict"] == "ENTITLED"})
    denied = sorted({r["plan_type"] for r in results if r["verdict"] == "NOT_ENTITLED"})
    capacity = sorted({r["plan_type"] for r in results if r["verdict"] == "CAPACITY"})
    print(f"entitled plans     : {', '.join(entitled) or '(none)'}")
    print(f"not-entitled plans : {', '.join(denied) or '(none)'}")
    if capacity:
        print(
            f"inconclusive plans : {', '.join(capacity)} "
            "(capacity errors only; rerun later)"
        )
    for note in skipped:
        print(f"skipped {note}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
