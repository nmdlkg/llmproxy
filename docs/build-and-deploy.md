# Build and deploy CLIProxyAPI

This runbook builds a clean release, stages it with smoke tests, and promotes
it to the systemd deployment. Routine administration is covered in
[admin-level-runbook.md](admin-level-runbook.md).

Reference paths:

- Service: `cliproxyapi.service` (`nobody:nogroup`)
- Binary: `/opt/cliproxyapi/bin/cliproxyapi`
- Config: `/etc/cliproxyapi/config.yaml`
- Source: `/home/minis/workspace/llmproxy`

The rollout restarts production; existing SSE/WebSocket connections can drop.
It is not zero-downtime.

## Install rollout tooling (once)

Requirements: Python 3.11+, `python3-yaml`, systemd, CGO-enabled Go 1.26+, and
a C compiler.

```bash
cd /home/minis/workspace/llmproxy
sudo bash deploy/safe-rollout/install.sh
```

The installer records a baseline under `/opt/cliproxyapi/releases` and installs
staging, deployment, and recovery units. It does not restart production.
Review the reference paths and service identity before using it on another host.
The monitor uses the first service API key from `key_file`; ensure that key
exists (mode 600) and that the configured URL/TLS certificate is reachable.

## Build and stage

Commit the worktree first. The build script runs formatting checks, Go tests,
rollout tests, scheduler tests, and produces a versioned server plus scheduler
plugin.

```bash
cd /home/minis/workspace/llmproxy
GOCACHE=/tmp/gocache GOPROXY=off bash deploy/safe-rollout/build.sh
```

Stage the printed bundle path:

```bash
sudo python3 /usr/local/lib/cliproxy-deploy/rollout.py stage /tmp/cliproxy-bundles/<release>
sudo journalctl -u cliproxyapi-stage@<release>.service --no-pager
```

Staging runs the real proxy against a local mock OpenAI-compatible upstream and
checks the plugin ABI, `/healthz`, authenticated `/v1/models`, completion, and
SSE. It uses isolated auth/SQLite data and no external network access. A failed
stage leaves production unchanged; build a new bundle after fixing it.

## Promote and monitor

Check schema compatibility before promotion. Back up SQLite with a consistent
SQLite backup method before any schema change; copying a live database file is
not sufficient. Destructive migrations need a separate maintenance plan.

Start the worker through systemd:

```bash
sudo systemctl start --no-block cliproxyapi-deploy@<release>.service
sudo journalctl -fu cliproxyapi-deploy@<release>.service
```

The worker verifies the current release, health, and model list, snapshots the
previous binary/config/plugins, switches the release atomically, and restarts
the service. It then monitors process state, restart count, health JSON, and a
nonempty authenticated model list for 180 seconds (30–300 seconds configurable).
Three consecutive failures trigger one rollback. Upstream 429/5xx responses do
not trigger automatic rollback.

Inspect the result:

```bash
sudo python3 /usr/local/lib/cliproxy-deploy/rollout.py status
sudo systemctl --failed
sudo journalctl -u cliproxyapi-deploy-recover.service -n 50 --no-pager
```

Recovery also runs on worker failure or reboot with pending state. Failed
recovery leaves a pending record and failed unit for operator attention. The
scheduler plugin is installed, but existing enable/disable settings are kept.

## Change configuration

Prefer a separate config rollout so failures have one obvious cause. Preserve
unrelated keys and merge lists instead of duplicating top-level keys.

```bash
sudo cp -a /etc/cliproxyapi/config.yaml /etc/cliproxyapi/config.yaml.before-$(date +%F)
sudo "$EDITOR" /etc/cliproxyapi/config.yaml
sudo chown nobody:nogroup /etc/cliproxyapi/config.yaml
sudo chmod 600 /etc/cliproxyapi/config.yaml
sudo systemctl restart cliproxyapi.service
```

`config.example.yaml` documents available keys; it is not read at runtime.

### Model pricing mappings

Merge mappings into the existing `openrouter.model-map` when needed for quota
accounting:

```yaml
openrouter:
  model-map:
    "codex-auto-review": "openai/gpt-5.6-luna"
    "claude-fable-5-1": "anthropic/claude-fable-5.1"
```

Remove exact entries for these models from
`tenancy.quota.model-price-overrides`; the mappings affect accounting only and
do not rewrite or reroute requests. Keep `openrouter.enabled: true` and
`openrouter.cost-basis: openrouter` for catalog pricing.

After restarting, verify the exact model ID when applicable:

```bash
curl -fsS -H "Authorization: Bearer $CLIPROXY_API_KEY" \
  http://100.110.30.57:8317/v1/models |
  jq -e '.data[] | select(.id == "codex-auto-review")'
```

### Weekly contribution tiers

`tenancy.quota.window: "168h"` is a rolling seven-day window. Contribution
values are twice the actual monthly subscription cost (the listed Codex values
are 40, 200, and 400 for Plus, Pro Lite, and Pro). Use runtime tier names
`prolite` and `education`; provider keys are `codex`, `claude`, `xai`, and
`antigravity` (`xai`, not `grok`). Claude/xAI/Antigravity use
`contribution_tier`, then `default`; Codex uses the ID-token `plan_type`.
Only owned, shared, enabled credentials contribute, and an absent `shared`
field is not true. Review named `base-usd` overrides and effective usage after
enabling enforcement. Set `auto-routing.quota-fallback: false` when exhaustion
must deny requests.

## Roll back

Roll back the latest accepted release, including its config snapshot:

```bash
sudo python3 /usr/local/lib/cliproxy-deploy/rollout.py rollback
```

Retry interrupted recovery:

```bash
sudo systemctl start cliproxyapi-deploy-recover.service
```

Rollback does not restore production DB or auth files, but it does replace the
captured config snapshot. Review later config edits first. After the first
promotion, `/opt/cliproxyapi/bin/cliproxyapi` is a symlink; do not overwrite its
release target with `cp` or `install`.

## Restricted build environments

Redirect Go's build cache when the home directory is read-only:

```bash
cd /home/minis/workspace/llmproxy
PATH=$PATH:/usr/local/go/bin GOCACHE=/tmp/gocache GOPROXY=off \
  go build -o /tmp/cliproxyapi-new ./cmd/server
PATH=$PATH:/usr/local/go/bin GOCACHE=/tmp/gocache GOPROXY=off \
  go test ./...
```

`GOPROXY=off` requires cached modules. Tests that bind sockets fail in sandboxes
blocking `listen(2)`; verify them on a normal host. Do not run the installer
from a restricted sandbox.
