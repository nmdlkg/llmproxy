# Conflict Surface Reduction Refactor — Handoff Draft

Draft plan — no implementation started.

## Current baseline

- Fork `main` and `origin/main` are at `9da8ff1c`.
- `upstream/main` is 60 commits ahead of the fork branch at the time of drafting.
- The fork diff against `upstream/main` is 183 files (`+26,085/-419`). Most of the 127 added files are isolated tenancy, user, auth-file, autoroute, and user-panel components. The higher-risk overlap is concentrated in 56 existing upstream files.
- The current release candidate was fully tested and built before deployment. This document only defines the refactor plan; it does not change runtime behavior or configuration.
- The existing seam ledger and focused checks are in [`docs/fork-patches.md`](fork-patches.md). Keep that ledger authoritative and update it whenever a symbol moves.

## Goals

1. Reduce the number and size of fork changes in files that upstream edits frequently.
2. Keep multi-tenancy behavior identical, including ownership, credential selection, quota enforcement, fallback, and realtime routes.
3. Make upstream synchronization a sequence of small, reviewable merges with stable extension points.
4. Preserve rollback compatibility and avoid introducing a database schema migration as part of this refactor.

## Non-goals

- Do not rebase shared branches or rewrite published history.
- Do not redesign tenancy policy, quota semantics, provider behavior, or the storage schema.
- Do not clean up the existing `sdk` to `internal` dependency direction; that is upstream baseline behavior.
- Do not move unrelated upstream code merely to reduce a line-count metric.
- Do not make a production rollout part of a refactor PR. Each completed phase gets its own build, test, and release candidate.

## Conflict hotspots

| Area | Current hotspot | Why it conflicts | Planned boundary |
| --- | --- | --- | --- |
| Configuration | `internal/config/config_types.go`, `config_normalization.go` | Fork tenancy and auto-routing defaults are mixed with frequently changing upstream config types and sanitization | Move fork type declarations and normalizers into focused files; leave a small canonical hook in the upstream loader |
| API lifecycle | `internal/api/server.go` | Tenancy, OTel, panel, and upstream lifecycle setup share constructors and start/stop paths | Compose fork runtime dependencies behind a small lifecycle object |
| Middleware and routes | `internal/api/server_middleware.go`, `server_routes.go` | Tenancy validation/quota/fallback is interleaved with upstream route registration | Put fork policy in dedicated middleware and route-policy registration helpers |
| SDK handlers | `sdk/api/handlers/handlers.go`, `handlers_context.go`, `handlers_routing.go`, `handlers_stream.go` | Auto-routing and original-model metadata touch common execution paths | Use a narrow model-resolution policy seam and preserve the original request metadata |
| Auth scheduling | `sdk/cliproxy/auth/scheduler.go`, conductor files | Priority and preferred-auth behavior are mixed with upstream scheduler edits | Keep additive resolver/preference interfaces and propose them upstream |
| Startup | `cmd/server/main.go` | Tenancy, catalog, OTel, and user-panel bootstrapping enlarge the upstream entrypoint diff | Extract fork startup composition into a helper with a stable call site |
| Auth persistence | management/auth-file provider handlers | Ownership stamping and duplicate checks can be copied into every provider integration | Inject one common persistence policy around `saveTokenRecord` |
| Model catalog | `internal/registry/models/models.json` | Upstream changes this large generated/static file frequently | Keep upstream catalog intact and merge fork entries through a typed overlay |

## Phased implementation plan

### Phase 0 — Baseline and protection

Before changing code:

1. Confirm the production release accepted `9da8ff1c`; record the release ID and rollback state.
2. Create a tracking branch with `git switch -c chore/upstream-<tag>` and merge upstream with `git merge <upstream-tag>`. Never rebase the shared fork branches.
3. Enable reusable conflict resolution with `git config rerere.enabled true`.
4. Record `git diff --stat upstream/main...HEAD` and the modified-file count.
5. Turn every check in `docs/fork-patches.md` into a required CI job, including tenancy isolation, quota/fallback, realtime, scheduler metadata, and config tests.

Exit criteria: clean worktree, baseline tests pass, no unresolved seam-ledger row, and a reproducible release bundle can be built.

### Phase 1 — Separate configuration ownership

Move fork-owned declarations from `internal/config/config_types.go` into focused files such as `tenancy_types.go`, `autoroute_types.go`, and `openrouter_types.go`. Move corresponding sanitization from `config_normalization.go` into `tenancy_normalization.go`, `autoroute_normalization.go`, and `openrouter_normalization.go`.

Keep `Config` and `SDKConfig` field declarations stable. Consolidate duplicated defaulting and normalization calls behind one `normalizeForkConfig(*Config)` hook invoked by the existing loader paths.

Required checks:

- `go test ./internal/config`
- Money/default and disabled-by-default tests
- YAML round-trip and config reload tests
- Fork seam tests from `docs/fork-patches.md`

Rollback: revert the file-move/composition commit as one unit. Do not alter persisted config keys or database state.

### Phase 2 — Isolate server lifecycle and route policy

Move fork-owned lifecycle state and start/stop/reload behavior from `internal/api/server.go` into a `forkRuntime` composition object. Preserve existing `ServerOption` APIs and keep only one or two stable construction hooks in `server.go`.

Move tenancy validation, quota, and forced-fallback middleware into a dedicated file. Replace repeated route-group middleware wiring with a route-policy registration helper or table in `server_routes.go`. Preserve policy order for `/v1`, `/openai/v1`, `/backend-api/codex`, `/v1beta`, and `/v1/realtime*`.

Required checks:

- `go test ./internal/api`
- Credential validation, quota, fallback fail-closed, and realtime route tests
- Start/stop/reload tests, including SQLite close and OTel shutdown
- `go test -race ./internal/api/handlers/user ./internal/api/handlers/authfiles`

Rollback: revert only the lifecycle/policy PR; keep the previous binary and config release available. No schema downgrade is allowed.

### Phase 3 — Centralize auth persistence policy

Keep provider-specific OAuth handlers focused on provider data. Add a common `AuthRecordPolicy` or equivalent hook around `saveTokenRecord` so ownership stamping, duplicate identity checks, secure writes, and post-auth ownership re-stamping happen in one place.

Preserve both `Auth.Metadata["owner_user_id"]` and `Auth.Attributes["owner_user_id"]`. Preserve same-owner duplicate counting and exclusion of credentials owned by another tenant or management.

Required checks:

- `go test ./internal/api/handlers/authfiles ./internal/api/handlers/management ./internal/api/handlers/user ./internal/tenancy`
- `go test -race ./internal/api/handlers/authfiles ./internal/api/handlers/user`
- Upload, OAuth, rename, pending-watcher, and ownership isolation tests

Rollback: revert the hook wiring without changing historical ownership records or schema.

### Phase 4 — Upstream-friendly SDK seams

Keep the following additive interfaces small and behavior-neutral when unset:

- `PriorityResolver` and effective credential priority in `sdk/cliproxy/auth/selector.go` and `scheduler.go`.
- Soft `preferred_auth_ids` metadata handling in scheduler/conductor execution.
- A model-resolution callback around auto-routing in `sdk/api/handlers` that preserves the original requested model in metadata.

Prepare separate upstream PRs for these seams. If upstream accepts one, remove the fork-only implementation in a later sync rather than carrying both versions. Do not combine upstream submission work with lifecycle or storage changes.

Required checks:

- Focused scheduler seam tests in `sdk/cliproxy/auth`
- Auto-routing execution-entry-point tests in `sdk/api/handlers`
- `go test ./sdk/cliproxy/auth ./sdk/api/handlers`
- Full tests and server build before each upstream merge

Rollback: disable the resolver/callback and verify the existing priority and registry routing paths remain unchanged.

### Phase 5 — Overlay fork model entries

Stop editing the frequently changing `internal/registry/models/models.json` for fork-only entries. Add a typed fork override source and merge it in the loader with explicit precedence and case-insensitive ID deduplication. Keep upstream entries authoritative unless the policy explicitly documents an override.

Required checks:

- Registry loader and model capability tests
- Duplicate-ID, precedence, local-model, and remote-updater tests
- `go test -mod=mod ./...`
- `go build -o /tmp/cli-proxy-api ./cmd/server`
- `bash deploy/safe-rollout/build.sh`

Rollback: disable the overlay source and use the prior catalog release. Do not rewrite the upstream catalog file during rollback.

## Multi-tenancy preservation gates

Every phase must preserve and test:

- Tenant owner isolation for upload, OAuth, rename, pending watcher loads, and management auth.
- Tenant access-provider ordering and exclusive-provider behavior.
- Quota observe-only versus enforce behavior, automatic fallback, and fail-closed behavior for unsupported direct routes.
- Quota and validation middleware on `/v1/realtime*` as well as normal API routes.
- Preferred-auth IDs, effective priority, cooldown, disabled credential, and incompatible-model fallthrough rules.
- Scheduler plugin sharing metadata (`owner_user_id`, `shared`) without leaking secrets.
- Same-owner duplicate semantics and exclusion of credentials owned by another tenant.
- Config reload updates, `--local-model` remote-updater behavior, and Home mode behavior.
- OTel tenant attribution and request/header redaction.

A phase is not mergeable if a focused gate is skipped, even when the full build passes.

## Release and stop conditions

For each phase, use one PR and one architectural boundary. Before merge:

```bash
gofmt -l .
go test -mod=mod ./...
go build -o /tmp/cli-proxy-api ./cmd/server
bash deploy/safe-rollout/build.sh
```

Stage the resulting bundle with the rollout controller, observe the staging journal, then promote from an operational root shell. Back up SQLite with a consistent SQLite backup mechanism before any change that could write new schema. The rollout controller does not downgrade schemas during rollback.

Stop and investigate if any of the following occurs:

- A merge requires editing the same upstream hunk in more than one unrelated concern.
- A tenancy gate changes behavior or becomes impossible to express without provider-specific copies.
- A config or database key is renamed, removed, or made incompatible.
- The scheduler or fallback path bypasses tenant identity, quota, or preferred-auth metadata.
- Staging fails, the production restart monitor reports repeated failures, or rollback leaves a pending deployment record.

## Next-session start commands

```bash
cd /home/minis/workspace/llmproxy
git status --short --branch
git fetch upstream --tags
git config rerere.enabled true
git log --oneline -5 main
git rev-list --left-right --count origin/main...upstream/main
git diff --stat upstream/main...main
sed -n '1,240p' docs/fork-patches.md
```

Start with Phase 0 only. Do not edit runtime code until the accepted production release and baseline bundle are recorded.
