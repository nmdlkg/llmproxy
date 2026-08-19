# Build and deploy CLIProxyAPI

This covers building the fork and rolling it onto the systemd deployment.
Administration of a running instance is in
[admin-level-runbook.md](admin-level-runbook.md); that runbook's section 5 is
the short form of this document for a routine model-driven rebuild.

Reference deployment:

- service: system-level `cliproxyapi.service`, runs as `cliproxy:cliproxy`
- binary: `/opt/cliproxyapi/bin/cliproxyapi`
- config: `/etc/cliproxyapi/config.yaml`
- source: `/home/minis/workspace/llmproxy`

The binary is owned by `nobody:nogroup` and only executed by the service user,
so preserve that ownership on install rather than inventing a new one.

## 1. Build

The default caches are writable on this host, so no environment overrides are
needed.

~~~bash
cd /home/minis/workspace/llmproxy
git status --short
/usr/local/go/bin/go build -trimpath -o /tmp/cliproxyapi-new ./cmd/server
~~~

Stamp the build so a deployed binary can be identified later. Without this the
binary reports `dev / none / unknown`, which makes it impossible to tell which
build is running.

~~~bash
cd /home/minis/workspace/llmproxy
/usr/local/go/bin/go build -trimpath \
  -ldflags "-X main.Version=$(git rev-parse --abbrev-ref HEAD) \
            -X main.Commit=$(git rev-parse --short HEAD) \
            -X main.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o /tmp/cliproxyapi-new ./cmd/server

/tmp/cliproxyapi-new --help | head -1
~~~

The ldflags target `main`, not `internal/buildinfo`. `cmd/server/main.go`
copies its own package variables into `buildinfo` at startup, so stamping
`buildinfo` directly is silently overwritten.

## 2. Verify before installing

~~~bash
cd /home/minis/workspace/llmproxy
/usr/local/go/bin/gofmt -l .
/usr/local/go/bin/go test ./...
~~~

`gofmt -l` must print nothing. For a change that touches a fork seam, also run
the focused detection commands in [fork-patches.md](fork-patches.md); a clean
build does not prove a seam survived.

## 3. Install and restart

Back up the current binary first and keep it until the new one is accepted.

~~~bash
sudo cp -a /opt/cliproxyapi/bin/cliproxyapi \
  /opt/cliproxyapi/bin/cliproxyapi.before-$(date +%F)
sudo install -o nobody -g nogroup -m 755 \
  /tmp/cliproxyapi-new /opt/cliproxyapi/bin/cliproxyapi
sudo systemctl restart cliproxyapi.service
sudo systemctl is-active cliproxyapi.service
~~~

Confirm the running build is the one you just installed, then watch startup for
credential, catalog, or telemetry errors.

~~~bash
journalctl -u cliproxyapi.service -n 40 --no-pager | grep -i 'version\|error\|warn'
~~~

## 4. Configuration changes

Edit `/etc/cliproxyapi/config.yaml` separately from the binary roll when you
can, so a failure has one obvious cause. Back it up first, merge lists instead
of duplicating top-level keys, and restart afterwards.

~~~bash
sudo cp -a /etc/cliproxyapi/config.yaml \
  /etc/cliproxyapi/config.yaml.before-$(date +%F)
sudo "$EDITOR" /etc/cliproxyapi/config.yaml
sudo systemctl restart cliproxyapi.service
~~~

`config.example.yaml` in the repository root is the reference for available
keys. It is documentation, not the deployed file; nothing reads it at runtime.

## 5. Rollback

~~~bash
sudo cp -a /opt/cliproxyapi/bin/cliproxyapi.before-<date> \
  /opt/cliproxyapi/bin/cliproxyapi
sudo systemctl restart cliproxyapi.service
sudo systemctl is-active cliproxyapi.service
~~~

Restore the config backup the same way for a configuration regression. Roll
back one of the two at a time so the cause stays identifiable.

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
