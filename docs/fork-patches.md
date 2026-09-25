# Fork patch ledger and upstream tracking

This repository is a fork of `github.com/router-for-me/CLIProxyAPI/v7`. The fork started at
upstream tag `v7.2.101` (`42a00a2`) and adds multi-tenancy, quota, automatic model routing, the
OpenRouter catalog, OTel usage export, and the `/v0/user` API with its embedded dashboard.

## 1. Why this file exists

The tenancy backend remains an upstream-tracking fork. Small independent policies can use native
plugins, such as `custom-plugins/tenancy-scheduler`, while retaining a documented host connection.
Upstream changes must be merged periodically, and fork-specific patch seams must remain reviewable.

This document is the ledger for those seams. Run the procedure below after every upstream merge,
then walk every row in the table. The goal is to catch a merge that compiles and passes broad tests
but silently removes, bypasses, or changes one of the fork's extension points.

The table describes the current tree, not a promise that upstream will keep the same layout. If an
upstream merge moves a symbol, update this ledger and its detection command as part of the merge
review.

## 2. Upstream tracking procedure

Run these commands from the repository root when preparing an upstream update. They are instructions
for the next tracking update; this documentation change does not run them.

~~~bash
git remote add upstream https://github.com/router-for-me/CLIProxyAPI
git fetch upstream --tags
git log --oneline v7.2.101..upstream/main
~~~

Upstream releases very frequently—there are more than 1,000 tags—so do not chase every tag. Merge
the latest useful release once every 2–4 weeks. Create a tracking branch named
`chore/upstream-<tag>` and merge the selected tag into it:

~~~bash
git switch -c chore/upstream-<tag>
git merge <upstream-tag>
~~~

Use a merge, not a rebase. `origin/dev`, `origin/feat/*`, and `origin/legacy` are already
published; rebasing them rewrites shared history and makes the fork harder to synchronize.

After the merge, run the baseline checks:

~~~bash
gofmt -l .
go build -o /tmp/cli-proxy-api ./cmd/server && rm /tmp/cli-proxy-api
go test ./...
~~~

Then review the seam ledger below. A clean build is necessary but does not replace checking the
fork-specific behavior.

## 3. Fork patch seams (the ledger)

Each row records a deliberate fork seam and the focused test command used to detect its loss or
behavioral regression. The commands below were run successfully against the current tree.

| Seam name | Files and symbols | What it does | How to detect breakage |
| --- | --- | --- | --- |
| Scheduler sharing metadata | `sdk/cliproxy/auth/conductor_selection.go`: `schedulerAuthCandidates`; `sdk/cliproxy/auth/scheduler_sharing.go`: `schedulerSharingMetadata` | Passes only scalar `owner_user_id` and `shared` metadata to native scheduler plugins. The optional tenancy scheduler filters its supplied candidates; built-in fallback remains unchanged. | `go test ./sdk/cliproxy/auth -run 'TestPluginSchedulerReceivesSharingMetadata\|TestManagerPluginScheduler\|TestManagerInactivePluginScheduler'`; also run `go test ./...` inside `custom-plugins/tenancy-scheduler` (a separate module). |
| 1. Effective credential priority | `sdk/cliproxy/auth/selector.go`: `PriorityResolver`, `(*Manager).SetPriorityResolver`; `sdk/cliproxy/auth/scheduler.go`: `authScheduler.priorityResolver`, `buildScheduledAuthMeta` | Lets tenancy compute an effective credential priority. A resolver result is used when it returns `ok == true`; otherwise the existing auth attribute priority remains the fallback. The fast scheduler applies the resolver when entries are upserted and refreshes its buckets when the resolver is installed. | `sdk/cliproxy/auth/scheduler_seams_test.go`: `TestSchedulerPriorityResolver`<br><br><code>go test ./sdk/cliproxy/auth -run '^TestSchedulerPriorityResolver$'</code> |
| 2. Preferred-auth-IDs first pass | `sdk/cliproxy/auth/scheduler.go`: `pickSingleWithStrategy`, `pickMixedWithStrategy`; `sdk/cliproxy/auth/conductor_execution.go`: `PreferredAuthIDsMetadataKey`, `preferredAuthIDsFromMetadata` | Carries a soft list of preferred credential IDs through request metadata. On the first pick, ready compatible preferred credentials are tried before normal priority selection; pinned credentials, tried credentials, cooldowns, disabled credentials, and incompatible models still take precedence or fall through as designed. The metadata constant is defined in `conductor_execution.go` as `preferred_auth_ids`. | `sdk/cliproxy/auth/scheduler_seams_test.go`: `TestSchedulerPreferredAuthSingleProvider`, `TestManagerPreferredCoolingAuthRequestFallsThrough`, `TestSchedulerPreferredAuthMixedProvider`, `TestPreferredAuthIDsFromMetadata`<br><br><code>go test ./sdk/cliproxy/auth -run '^(TestSchedulerPreferredAuthSingleProvider|TestManagerPreferredCoolingAuthRequestFallsThrough|TestSchedulerPreferredAuthMixedProvider|TestPreferredAuthIDsFromMetadata)$'</code> |
| 3. Auto-routing before model-router dispatch | `sdk/api/handlers/handlers_routing.go`: `resolveAutoRoute`, `resolveAutoRoutedModel`, `applyModelRouter`; `sdk/api/handlers/handlers_execution.go`: non-streaming and count call sites; `sdk/api/handlers/handlers_stream.go`: streaming call site | Resolves an enabled `auto` or `auto:<tier>` model before `applyModelRouter`, while preserving the existing registry fallback when auto-routing is disabled or cannot resolve. The package variable `resolveAutoRoute` is the testable resolver seam. All three execution entry points keep the original requested model in metadata after resolution. | `sdk/api/handlers/handlers_autoroute_test.go`: `TestResolveAutoRoutedModel`, `TestAutoRoutingExecutionEntryPointsPreserveOriginalRequestedModel`<br><br><code>go test ./sdk/api/handlers -run '^(TestResolveAutoRoutedModel|TestAutoRoutingExecutionEntryPointsPreserveOriginalRequestedModel)$'</code> |
| 4. Tenant access-provider ordering | `internal/access/reconcile.go`: `ApplyAccessProviders`, `userProviderFirst`; `sdk/access/registry.go`: `SetExclusiveProvider`, `ClearExclusiveProvider`, `RegisteredProviders` | Moves the tenant provider to index 0 after reconciliation. The SDK registry has an exclusive-provider restriction but no priority concept, so ordering is the mechanism that makes tenant authentication win when both tenant and service providers can authenticate the same request. | `internal/access/reconcile_test.go`: `TestApplyAccessProvidersOrdersAndDisablesTenantProvider`<br><br><code>go test ./internal/access -run '^TestApplyAccessProvidersOrdersAndDisablesTenantProvider$'</code> |
| 5. Tenancy middleware and route insertion | `internal/api/server_middleware.go`: `CredentialValidationMiddleware`, `UserQuotaMiddleware`, `credentialValidationMiddleware`, `userQuotaMiddleware`; `internal/api/server_routes.go`: `setupRoutes` and the `.Use()` calls on `/v1`, `/openai/v1`, `/backend-api/codex`, and `/v1beta` | Resolves the tenant identity after authentication, adds a soft preferred-auth context for validation traffic, and enforces quota or the configured auto-routing fallback. The route groups install the middleware before their API handlers; unsupported direct routes remain fail-closed when fallback cannot be applied. | `internal/api/server_validation_middleware_test.go`: `TestCredentialValidationMiddleware`; `internal/api/server_quota_middleware_test.go`: `TestUserQuotaMiddleware`, `TestQuotaFallbackHTTPExecutionUsesFallbackForExplicitModel`, `TestQuotaFallbackUnsupportedDirectRoutesFailClosed`<br><br><code>go test ./internal/api -run '^(TestCredentialValidationMiddleware|TestUserQuotaMiddleware|TestQuotaFallbackHTTPExecutionUsesFallbackForExplicitModel|TestQuotaFallbackUnsupportedDirectRoutesFailClosed)$'</code> |
| 6. Typed tenancy and auto-routing configuration | `internal/config/config_types.go`: `TenancyConfig`, `TenancyQuota`, `TenancyBalancing`, `AutoRoutingConfig`, `AutoRoutingTier`; `internal/config/config_normalization.go`: `SanitizeTenancyConfig`, `SanitizeAutoRoutingConfig`; `internal/config/sdk_config.go`: `SDKConfig.AutoRouting`; `internal/config/config.go`: `Config.Tenancy` | Keeps the fork's server-side tenancy section and handler-facing auto-routing section as typed configuration. Parsing and sanitization apply defaults, normalize keys and model lists, preserve the explicit disabled-by-default behavior, and expose the canonical structures consumed at runtime. | `internal/config/money_test.go`: `TestTenancyMoneyConfigUsesExactNanoUSD`; the package test also compiles and exercises the shared parse/normalization pipeline used by both sections.<br><br><code>go test ./internal/config</code> |
| 7. Auth-file ownership stamping | `internal/api/handlers/authfiles/ownership.go`: `StampOwnerFromContext`; `internal/api/handlers/management/auth_files_fields.go`: `(*Handler).saveTokenRecord` calls `StampOwnerFromContext` before and after the custom post-auth hook | Stamps the resolved tenancy user ID into both `Auth.Metadata["owner_user_id"]` and `Auth.Attributes["owner_user_id"]` for user-initiated OAuth. The second stamp prevents a custom hook from replacing the resolved owner; management auth without a tenancy context remains unowned. | `internal/api/handlers/authfiles/ownership_test.go`: `TestStampOwnerFromContextUsesResolvedUser`, `TestStampOwnerFromContextLeavesManagementAuthUnowned`; the management package is included so the `saveTokenRecord` call sites compile.<br><br><code>go test ./internal/api/handlers/authfiles ./internal/api/handlers/management</code> |
| 8. Tenant credential uniqueness | `sdk/cliproxy/auth/credential_identity.go`: `CredentialIdentityKeys`; `internal/api/handlers/authfiles/registration.go`: `CheckUserRegistration`; upload and OAuth persistence callers; `internal/tenancy/service.go`: `credentialResolver` | Rejects user registration of existing upstream credentials before persistence, including renamed files and pending watcher loads. Returns an actionable duplicate warning. Counts same-owner copies once for quota and excludes identities also registered to another owner or management. Does not repair historical ownership overwrites. | `go test ./internal/api/handlers/authfiles ./internal/api/handlers/management ./internal/api/handlers/user ./internal/tenancy`; concurrency regression coverage: `go test -race ./internal/api/handlers/authfiles ./internal/api/handlers/user`. |

## 4. Candidates to upstream

Seams 1, 2, and 3 are purely additive extension points. When they are unset, the existing
attribute-priority, normal scheduler, and registry-based model-routing behavior remains the default.
They are therefore candidates for upstream pull requests. If accepted upstream, the corresponding
fork patches can be deleted instead of carried through future merges.

## 5. Known layering note

The `sdk/` packages importing `internal/` is upstream's own design, not a fork artifact. Do not
attempt to "clean up" that dependency direction during an upstream merge; doing so would create
unrelated conflicts.

The fork's own contribution to that count is 4 import lines pulling 2 distinct packages
(`internal/autoroute`, `internal/tenancy`) into 3 non-test files, totalling 62 added lines:

| File | Fork-added imports | Added lines |
| --- | --- | --- |
| `sdk/api/handlers/handlers.go` | `internal/autoroute`, `internal/tenancy` | +15 |
| `sdk/api/handlers/handlers_routing.go` | `internal/autoroute` | +43 |
| `sdk/api/handlers/handlers_stream.go` | `internal/autoroute` | +6 |

All three are purely additive with well-separated insertion points, so a 3-way merge resolves them
automatically. Reverting them would rewrite files upstream edits frequently and *create* conflicts
rather than remove them.

The required current-tree check is:

~~~bash
grep -rn "CLIProxyAPI/v7/internal/" sdk/ --include=*.go | grep -v _test | wc -l
~~~

It returns `153`. That existing layering is part of the upstream baseline; the fork-specific changes
should remain limited to their documented seams.

## 6. Upstream baseline and conflict surface (Phase 0)

Recorded for the conflict-surface refactor described in
[`handoff-conflict-surface-refactor.md`](handoff-conflict-surface-refactor.md).

| Item | Value |
| --- | --- |
| Upstream tag merged | `v7.3.17` (`9bdde54b`) on branch `chore/upstream-v7.3.17` |
| Fork head before merge | `9da8ff1c` (`main`, `origin/main`) |
| Production release accepting `9da8ff1c` | **Pending operator confirmation.** `/var/lib/cliproxy-deploy` is root-only; record the release ID and rollback state from `rollout.py status` here. |
| Phase 0 release candidate | `20260925T063219Z-30472e260861` (`deploy/safe-rollout/build.sh`, full tests passed) |
| `git diff --shortstat upstream/main...HEAD` after merge | 184 files, `+26,298/-421` |
| Added / modified upstream files | 129 added, **55 modified** (`+2,573/-421` in modified files) |
| `git config rerere.enabled` | `true` |

Conflicts resolved in this merge: `internal/api/server.go` (listener mutex vs. user-panel
supervisor and fork stop errors), `internal/redisqueue/plugin.go` (execution/trace IDs vs.
tenant attribution), `internal/runtime/executor/helps/usage_helpers.go` (trace IDs vs.
source provenance). Silent breakages the merge produced and that the build caught:
a `config -> otelusage -> sdk/cliproxy/usage -> logging -> config` import cycle (fixed by the
dependency-free `internal/otelusage/otelspec`), and a dropped `strconv` import in
`internal/watcher/synthesizer/file.go`. Upstream `377747b8` made static Antigravity
`native_capabilities.web_search` authoritative, so the fork's reset of `SupportsWebSearch`
before applying probe hints was dropped.

The upstream CI step `.github/scripts/refresh-model-catalogs.sh` overwrites
`internal/registry/models/models.json` from `router-for-me/models`; fork-only catalog
entries must therefore live in the overlay file, not in `models.json`.

### Modified upstream file classification

Categories: **seam** = tenancy or fork feature extension point; **security** = fork hardening
kept as-is and proposed upstream; **noise** = no behavior; **decl** = field or constant
declaration that must stay in the upstream type. The phase column names the refactor phase
that shrinks the file; `-` means the change stays as-is.

| File(s) | Category | Phase |
| --- | --- | --- |
| `internal/config/config_types.go`, `config_normalization.go`, `config_load.go`, `parse.go`, `config_defaults.go` | seam | 1 |
| `internal/config/config.go`, `sdk_config.go`, `config_yaml.go` | decl | - |
| `internal/api/server.go`, `server_reload.go`, `server_options.go`, `server_middleware.go`, `server_routes.go` | seam | 2 |
| `cmd/server/main.go` | seam (CLI flags, remote store, OpenRouter updater) | - |
| `internal/api/handlers/management/auth_files.go`, `auth_files_crud.go`, `auth_files_fields.go` | seam | 3 |
| `internal/api/handlers/management/auth_files_provider_oauth.go`, `oauth_sessions.go`, `oauth_callback.go` (+ tests) | seam (OAuth session owner binding) | - |
| `internal/access/config_access/provider.go` | seam | 3 |
| `internal/access/reconcile.go` | seam 4 | - |
| `internal/watcher/synthesizer/file.go` | seam (ownership attributes) | - |
| `sdk/api/handlers/handlers.go`, `handlers_context.go`, `handlers_routing.go`, `handlers_stream.go`, `handlers_execution.go`, `handlers_model_router_test.go` | seam 3 | 4 |
| `sdk/cliproxy/auth/scheduler.go`, `selector.go`, `conductor.go`, `conductor_execution.go`, `conductor_selection.go` | seams 0-2 | 4 |
| `internal/registry/models/models.json` | seam (fork catalog entries) | 5 |
| `sdk/cliproxy/service_models.go` | seam (Codex plan catalog mapping) | - |
| `internal/util/provider.go`, `internal/api/middleware/request_logging.go`, `response_writer.go` (+ test) | security (header, query, and body redaction) | - |
| `internal/redisqueue/plugin.go`, `sdk/cliproxy/usage/manager.go`, `internal/runtime/executor/helps/usage_helpers.go`, `logging_helpers.go` (+ test) | security (no API key in queue, source provenance) and tenant attribution | - |
| `sdk/config/config.go` | decl (SDK type aliases) | - |
| `internal/pluginhost/host.go`, `internal/runtime/executor/claude_thinking_replay_test.go`, `codex_stream_bootstrap_buffering_test.go` | noise (gofmt; listed in `.github/scripts/gofmt-allowlist.txt`) | - |
| `go.mod`, `go.sum`, `config.example.yaml`, `.gitignore` | dependencies and docs | - |

The security rows are upstream pull request candidates. Do not refactor them in the
conflict-surface phases.

### Continuous checks

`.github/workflows/fork-seams.yml` runs `.github/scripts/fork-seam-checks.sh` (every focused
command in section 3 plus the race tests), the full suite, and a server build. Run the script
locally after every upstream merge.
