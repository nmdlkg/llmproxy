#!/usr/bin/env bash
set -euo pipefail
repo=$(git rev-parse --show-toplevel)
cd "$repo"
if [[ -n $(git status --porcelain) ]]; then
  echo 'Commit or stash changes before building a deployable release.' >&2
  exit 1
fi
export PATH="/usr/local/go/bin:$PATH"
release_id="$(date -u +%Y%m%dT%H%M%SZ)-$(git rev-parse --short=12 HEAD)"
bundle_parent=$(realpath -m -- "${1:-/tmp/cliproxy-bundles}")
bundle="$bundle_parent/$release_id"
mkdir -p "$bundle_parent"
mkdir "$bundle"
mkdir "$bundle/plugins"
[[ -z $(gofmt -l .) ]]
go test ./...
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s deploy/safe-rollout
(
  cd custom-plugins/tenancy-scheduler
  go test ./...
  go build -trimpath -buildmode=c-shared -o "$bundle/plugins/tenancy-scheduler.so" .
)
rm -f "$bundle/plugins/tenancy-scheduler.h"
go build -trimpath -ldflags "-X main.Version=$release_id -X main.Commit=$(git rev-parse HEAD) -X main.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o "$bundle/cliproxyapi" ./cmd/server
python3 - "$bundle" <<'PY'
import hashlib,json,sys
from pathlib import Path
root=Path(sys.argv[1])
values={}
for path in sorted(root.rglob('*')):
    if path.is_file():
        with path.open('rb') as f: values[str(path.relative_to(root))]=hashlib.file_digest(f,'sha256').hexdigest()
(root/'manifest.json').write_text(json.dumps(values,indent=2))
PY
printf 'Verified bundle: %s\n' "$bundle"
