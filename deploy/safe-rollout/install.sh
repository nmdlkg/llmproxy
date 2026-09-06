#!/usr/bin/env bash
set -euo pipefail
[[ $EUID == 0 ]] || { echo 'Run with sudo.' >&2; exit 1; }
source_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
python3 -c 'import yaml'
install -d -m 755 /usr/local/lib/cliproxy-deploy /opt/cliproxyapi/releases
install -d -m 700 /var/lib/cliproxy-deploy
install -d -o root -g nogroup -m 750 /var/lib/cliproxyapi-staging
install -m 755 "$source_dir/rollout.py" "$source_dir/smoke.py" /usr/local/lib/cliproxy-deploy/
install -m 644 "$source_dir/"*.service /etc/systemd/system/
python3 /usr/local/lib/cliproxy-deploy/rollout.py init
python3 - <<'PY'
import json,os
from pathlib import Path
import yaml
state=Path('/var/lib/cliproxy-deploy')
cfg=yaml.safe_load(Path('/etc/cliproxyapi/config.yaml').read_text())
keyfile=state/'probe.key'
keys=cfg.get('api-keys') or []
if not keyfile.exists() and keys and isinstance(keys[0],str):
    fd=os.open(keyfile,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(fd,'w') as f: f.write(keys[0])
settings=state/'settings.json'
if not settings.exists():
    host=cfg.get('host') or '127.0.0.1'
    if host in ['0.0.0.0','::']: host='127.0.0.1'
    if ':' in host: host='['+host+']'
    scheme='https' if (cfg.get('tls') or {}).get('enable') else 'http'
    settings.write_text(json.dumps({'url':f"{scheme}://{host}:{cfg.get('port',8317)}",'key_file':str(keyfile),'observe_seconds':180,'interval_seconds':5},indent=2))
    settings.chmod(0o600)
if not keyfile.exists():
    print('Before promotion: provision /var/lib/cliproxy-deploy/probe.key with a valid production API key (mode 600).')
PY
install -d /etc/systemd/system/cliproxyapi.service.d
cat > /etc/systemd/system/cliproxyapi.service.d/deploy-recovery.conf <<'UNIT'
[Unit]
Requires=cliproxyapi-deploy-boot.service
After=cliproxyapi-deploy-boot.service
UNIT
systemctl daemon-reload
systemctl enable cliproxyapi-deploy-boot.service
systemctl start cliproxyapi-deploy-boot.service
printf '%s\n' 'Installed. Production was not restarted. Stage a bundle before promotion.'
