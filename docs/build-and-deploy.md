# Build and deploy CLIProxyAPI

This covers building the fork and rolling it onto the systemd deployment.
Administration of a running instance is in
[admin-level-runbook.md](admin-level-runbook.md); that runbook's section 5 is
the short form of this document for a routine model-driven rebuild.

Reference deployment:

- service: system-level `cliproxyapi.service`, runs as `nobody:nogroup`
- binary: `/opt/cliproxyapi/bin/cliproxyapi`
- config: `/etc/cliproxyapi/config.yaml`
- source: `/home/minis/workspace/llmproxy`

The binary and service data are owned by `nobody:nogroup` and only executed by
the service user, so preserve that ownership on install rather than inventing
a new one.

## 1. Install the staging and rollback tooling once

The reference deployment uses systemd; no container orchestrator or traffic
splitting is required. The rollout controller stages the new executable and
plugin, promotes the same files, and observes production for three minutes.
Production still restarts during promotion, so active SSE/WebSocket connections
may disconnect. This is not a zero-downtime deployment.

Requirements: Python 3.11+, `python3-yaml`, systemd, a CGO-enabled Go 1.26+
toolchain, and a C compiler. Run installation from a normal operator shell:

~~~bash
cd /home/minis/workspace/llmproxy
sudo bash deploy/safe-rollout/install.sh
~~~

Installation captures the currently deployed binary, configuration, and plugin
directory as a baseline under `/opt/cliproxyapi/releases`. It installs:

- `cliproxyapi-stage@.service`: isolated staging, `nobody:nogroup`, private network.
- `cliproxyapi-deploy@.service`: terminal-independent promotion and monitoring.
- `cliproxyapi-deploy-recover.service`: recovery if the deployment worker fails.
- `cliproxyapi-deploy-boot.service`: interrupted-deployment recovery before proxy startup.

Installation does not restart production. The scripts target the documented
reference paths, service identity, and `/var/lib/cliproxyapi` working directory.
Review those constants before reusing on another host. Relative plugin paths are
resolved against that working directory; use an absolute path for `plugins.dir`
when migrating from a tilde-based setting.

The installer creates root-only monitoring settings at
`/var/lib/cliproxy-deploy/settings.json`. It uses the first configured service API
key for authenticated model-list checks without printing it. If there are no
service API keys, provision a valid production key in the configured `key_file`
(mode 600) before promotion. Check the URL, especially with TLS or a Tailscale-only
listener. TLS verification is enabled; configure a trusted certificate/hostname.

Release files are root-owned and readable by the service group; live service
configuration retains `nobody:nogroup` ownership. This prevents the staging
process from rewriting release artifacts or deployment approval records.

## 2. Build and stage

Commit the worktree first. The build script refuses dirty source, runs formatting
checks, all root Go tests, rollout failure tests, and the nested scheduler module's
tests, then builds a version-stamped server and native scheduler plugin:

~~~bash
cd /home/minis/workspace/llmproxy
GOCACHE=/tmp/gocache GOPROXY=off bash deploy/safe-rollout/build.sh
~~~

The printed bundle is `/tmp/cliproxy-bundles/<timestamp>-<commit>`. Its manifest
binds the executable and plugin checksums. The ldflags target `main.Version`,
`main.Commit`, and `main.BuildDate`; the entrypoint copies them into `buildinfo`.
For a fork seam change, also run the focused commands in
[fork-patches.md](fork-patches.md).

Import and stage the bundle, substituting its exact path:

~~~bash
sudo python3 /usr/local/lib/cliproxy-deploy/rollout.py stage /tmp/cliproxy-bundles/<release>
sudo journalctl -u cliproxyapi-stage@<release>.service --no-pager
~~~

Staging uses the new real proxy with a local mock OpenAI-compatible upstream.
It verifies the native sharing plugin ABI, `/healthz`, authenticated `/v1/models`,
a normal completion, and SSE through the real server. It has an independent auth
directory and SQLite DB, no inherited production environment, and no external
network access. Production configuration, credentials, and release snapshots are
inaccessible inside the staging service. Only executable artifacts are copied in.

This is a deterministic smoke gate, not full production parity. It does not prove
real OAuth refresh, provider availability, every tenant UI workflow, or unrelated
plugins. A dedicated real test account can be added in a separate integration
stage later; never run a copied production OAuth identity concurrently. Staging
currently exercises the tenancy scheduler; other pre-existing plugins are
preserved for production but are not exercised by the mock test.

A failed stage leaves production untouched and does not create a promotion
approval. Use a new bundle ID after fixing a failed stage. Promotion rejects
changed release files or a production config changed since staging.

## 3. Promote and observe

Only promote changes whose database writes/schema remain readable by the previous
version. The controller does not establish migration compatibility automatically.
The current additive API-key column/table migrations should still be checked
against the specific deployed version. Back up SQLite with a consistent SQLite
backup mechanism before a schema change; copying a live `.db` file alone is not
sufficient. A destructive/incompatible migration needs a separate maintenance plan.

Start the worker through systemd, so closing the terminal cannot cancel monitoring:

~~~bash
sudo systemctl start --no-block cliproxyapi-deploy@<release>.service
sudo journalctl -fu cliproxyapi-deploy@<release>.service
~~~

Before stopping production, the worker checks its current executable hash,
`/healthz`, and authenticated model listing. It snapshots the immediately previous
binary/config/plugins, writes a durable pending record, stops the service, installs
the candidate configuration and executable link, and starts the service again.
The config watcher therefore cannot observe a half-switched running release.

For 180 seconds (configurable from 30 to 300), it checks the running executable,
process state, restart count, health JSON, and a nonempty authenticated model list.
Three consecutive failed checks trigger one rollback attempt. Upstream 429/5xx
responses are not automatic rollback triggers. These checks detect startup and
local API regressions, not every possible inference or policy failure.

On acceptance, the worker records the current release and clears pending state.
On failure, it restores the previous binary, plugin paths, and configuration,
restarts, and checks recovery. A killed/timed-out worker invokes recovery through
`OnFailure`; a reboot with pending state restores the previous files before the
proxy starts. Failed recovery leaves a pending record and a failed unit for
operator attention. There is no infinite new/old retry loop and no automatic
redeployment of a rejected version.

~~~bash
sudo python3 /usr/local/lib/cliproxy-deploy/rollout.py status
sudo systemctl --failed
sudo journalctl -u cliproxyapi-deploy-recover.service -n 50 --no-pager
~~~

The deployment journal and persisted `last-result.json` are the initial alert
surfaces; no email/Slack notification transport is installed. Wire failed units
into an existing monitoring system if unattended push alerts are required.

The bundled scheduler `.so` is installed but existing production enable/disable
settings are preserved. Merely deploying a bundle does not enable the scheduler.
The external user-panel development mount/release channel also remains independent
of this backend rollout; frontend changes are not rolled back by these scripts.

## 4. Configuration changes

Edit `/etc/cliproxyapi/config.yaml` separately from the binary roll when you
can, so a failure has one obvious cause. Back it up first, merge lists instead
of duplicating top-level keys, and restart afterwards.

~~~bash
sudo cp -a /etc/cliproxyapi/config.yaml \
  /etc/cliproxyapi/config.yaml.before-$(date +%F)
sudo "$EDITOR" /etc/cliproxyapi/config.yaml
sudo chown nobody:nogroup /etc/cliproxyapi/config.yaml
sudo chmod 600 /etc/cliproxyapi/config.yaml
sudo systemctl restart cliproxyapi.service
~~~

`config.example.yaml` in the repository root is the reference for available
keys. It is documentation, not the deployed file; nothing reads it at runtime.

### Expose Codex Auto Review with Luna-class pricing

`codex-auto-review` is present in the Codex model catalog for every supported
plan. To broadcast it from the reference deployment, first remove any exact or
glob entry under `oauth-excluded-models.codex` that matches
`codex-auto-review`. Do not create an alias to another model: requests must keep
the real upstream model ID.

OpenAI's [Auto-review documentation](https://learn.chatgpt.com/docs/sandboxing/auto-review)
describes this as a separate reviewer agent with a narrower task than the main
agent. It does not identify an exact backing model in the current page text.
For local cost estimation, treat it as a Luna-class model by merging this entry
into the existing `openrouter.model-map` mapping:

~~~yaml
openrouter:
  model-map:
    "codex-auto-review": "openai/gpt-5.6-luna"
~~~

Remove any exact `codex-auto-review` entry from
`tenancy.quota.model-price-overrides`; otherwise its fallback-only fields can
mix with Luna catalog prices. This mapping affects accounting and benchmark
lookup only. It does not rewrite the requested model or route Auto Review
requests to Luna.

After merging the changes, preserve ownership, restart, and verify that the
running server broadcasts the exact model ID:

~~~bash
sudo chown nobody:nogroup /etc/cliproxyapi/config.yaml
sudo chmod 600 /etc/cliproxyapi/config.yaml
sudo systemctl restart cliproxyapi.service
sudo systemctl is-active cliproxyapi.service
curl -fsS -H "Authorization: Bearer $CLIPROXY_API_KEY" \
  http://100.110.30.57:8317/v1/models |
  jq -e '.data[] | select(.id == "codex-auto-review")'
~~~

A successful model-list check proves the model is broadcast. Confirm that its
accounted token rates match `gpt-5.6-luna` separately in the user or admin usage
view after a request. The Luna relationship is an explicit local pricing
assumption, not a claim that both model IDs route to the same backend.

## 5. Manual rollback

To roll back the most recently accepted deployment (including its config snapshot):

~~~bash
sudo python3 /usr/local/lib/cliproxy-deploy/rollout.py rollback
~~~

To retry recovery of an interrupted/failed deployment:

~~~bash
sudo systemctl start cliproxyapi-deploy-recover.service
~~~

Rollback never restores or deletes the production DB or auth files. Usage accrued
and credential refreshes after deployment must survive. Configuration edits made
after the captured snapshot will be replaced by manual rollback; review them first.
Retain baseline and previous releases. There is no automatic garbage collection.

The original `/opt/cliproxyapi/bin/cliproxyapi` path becomes a symlink on first
promotion. Do not use the old `install`/`cp` commands to overwrite that link target:
that would mutate an immutable release and bypass staging. Use this controller for
subsequent binary rolls. The live config remains `/etc/cliproxyapi/config.yaml`.

## 6. Building from a restricted sandbox

An agent or CI runner may see the home directory mounted read-only. Go then
fails on its own caches before compiling anything:

~~~
open /home/<user>/.cache/go-build/...: read-only file system
~~~

This is an environment restriction, not a repository or Go configuration
problem. Redirect the build cache to a writable path. The module cache is
usually already populated and only needs to be readable, so resolution can run
offline.

~~~bash
cd /home/minis/workspace/llmproxy
PATH=$PATH:/usr/local/go/bin GOCACHE=/tmp/gocache GOPROXY=off \
  go build -o /tmp/cliproxyapi-new ./cmd/server

PATH=$PATH:/usr/local/go/bin GOCACHE=/tmp/gocache GOPROXY=off \
  go test ./...
~~~

Notes for that environment:

- `go` is often absent from a non-login shell `PATH`; call it through
  `/usr/local/go/bin` or prepend that directory.
- `GOPROXY=off` fails loudly on a missing module instead of hanging on a
  blocked network. Drop it if a dependency genuinely needs fetching, and expect
  that to require network access.
- A warning like `writing stat cache: ... read-only file system` is harmless.
  The build still succeeds; only the module cache metadata write is refused.
- Sandboxes that block `listen(2)` will fail every test that starts an
  `httptest` server, with `socket: operation not permitted`. Those failures are
  environmental. Confirm the suite on a normal shell before concluding anything
  about a regression, and do not "fix" them.
- Do not install from a sandbox. Build and verify there, then hand the binary
  to an operator who can run the `sudo` steps in section 3.
