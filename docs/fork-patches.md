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
| 1. Effective credential priority | Fork-owned: `sdk/cliproxy/auth/scheduler_priority.go`: `PriorityResolver`, `(*Manager).SetPriorityResolver`, `setPriorityResolver`, `applyPriorityResolverLocked`. Upstream call sites in `sdk/cliproxy/auth/scheduler.go`: the `authScheduler.priorityResolver` field and one `s.applyPriorityResolverLocked(meta)` line after each of the three `buildScheduledAuthMetaWithModelSet` upsert calls. `authPriority` and `buildScheduledAuthMeta*` are upstream verbatim. | Lets tenancy compute an effective credential priority. A resolver result is used when it returns `ok == true`; otherwise the existing auth attribute priority remains the fallback. The fast scheduler applies the resolver when entries are upserted and refreshes its buckets when the resolver is installed. | `sdk/cliproxy/auth/scheduler_seams_test.go`: `TestSchedulerPriorityResolver`<br><br><code>go test ./sdk/cliproxy/auth -run '^TestSchedulerPriorityResolver$'</code> |
| 2. Preferred-auth-IDs first pass | Fork-owned: `sdk/cliproxy/auth/scheduler_preferred.go`: `PreferredAuthIDsMetadataKey`, `preferredAuthIDsFromMetadata`, `pickPreferredSingle`, `pickPreferredMixed`; `sdk/api/handlers/handlers_preferred_auth.go`: `WithPreferredAuthIDs`, `addPreferredAuthIDsMetadata`. Upstream call sites: one guarded call at the top of `pickSingleWithStrategy` and `pickMixedWithStrategy` in `scheduler.go`, and `addPreferredAuthIDsMetadata(ctx, meta)` in `requestExecutionMetadata` (`sdk/api/handlers/handlers.go`). | Carries a soft list of preferred credential IDs through request metadata. On the first unpinned pick, the upstream selection runs once with every non-preferred scheduled auth treated as already tried, so ready compatible preferred credentials win; cooldowns, disabled credentials, incompatible models, weights, and cursors behave exactly as in normal selection. If none is ready, normal selection runs without the preferred list. | `sdk/cliproxy/auth/scheduler_seams_test.go`: `TestSchedulerPreferredAuthSingleProvider`, `TestManagerPreferredCoolingAuthRequestFallsThrough`, `TestSchedulerPreferredAuthMixedProvider`, `TestPreferredAuthIDsFromMetadata`<br><br><code>go test ./sdk/cliproxy/auth -run '^(TestSchedulerPreferredAuthSingleProvider&#124;TestManagerPreferredCoolingAuthRequestFallsThrough&#124;TestSchedulerPreferredAuthMixedProvider&#124;TestPreferredAuthIDsFromMetadata)$'</code> |
| 3. Auto-routing before model-router dispatch | Fork-owned: `sdk/api/handlers/handlers_autoroute.go`: `resolveAutoRoute`, `resolveForkModel`, `resolveAutoRoutedModel`, `forcedFallbackUnavailableError`, `forcedQuotaFallback`, `forcedFallbackRouteDecision`; `sdk/api/handlers/handlers_fork.go`: `withForkRequestValues`, `withForkClientAttribution`. Upstream call sites: `resolveForkModel` at the non-streaming and streaming entry points (`handlers_execution.go`, `handlers_stream.go`), `resolveAutoRoutedModel` at the count entry point, the forced-fallback guard at the top of `applyModelRouter` and `forcedFallbackRouteDecision` after the prepared stream route (`handlers_routing.go`, `handlers_stream.go`), and two helper calls in `GetContextWithCancel` (`handlers.go`). | Resolves an enabled `auto` or `auto:<tier>` model before `applyModelRouter`, while preserving the existing registry fallback when auto-routing is disabled or cannot resolve. Forced quota fallback skips every model router, ignores prepared stream routes, and fails with 502 when the fallback model has no provider. All three execution entry points keep the original requested model in metadata after resolution. | `sdk/api/handlers/handlers_autoroute_test.go`: `TestResolveAutoRoutedModel`, `TestAutoRoutingExecutionEntryPointsPreserveOriginalRequestedModel`; `handlers_forced_fallback_test.go`: `TestForcedFallbackSkipsEveryModelRouterTarget`, `TestForcedFallbackIgnoresPreparedStreamModelRoute`<br><br><code>go test ./sdk/api/handlers -run 'AutoRout&#124;ForcedFallback&#124;PreferredAuth'</code> |
| 4. Tenant access-provider ordering | `internal/access/reconcile.go`: `ApplyAccessProviders`, `userProviderFirst`; `sdk/access/registry.go`: `SetExclusiveProvider`, `ClearExclusiveProvider`, `RegisteredProviders` | `internal/access/keyextract`: `FromRequest` is used only by the tenant provider; the upstream `config_access` provider keeps its own extraction. Moves the tenant provider to index 0 after reconciliation. The SDK registry has an exclusive-provider restriction but no priority concept, so ordering is the mechanism that makes tenant authentication win when both tenant and service providers can authenticate the same request. | `internal/access/reconcile_test.go`: `TestApplyAccessProvidersOrdersAndDisablesTenantProvider`; `internal/access/config_access/keyextract_parity_test.go`: `TestKeyExtractParityWithConfigProvider`<br><br><code>go test ./internal/access/...</code> |
| 5. Tenancy middleware and route insertion | Fork-owned: `internal/api/server_middleware_tenancy.go`: `tenantPolicy`, `tenantChain`, `quotaFallbackUnsupportedRoutes`, `rejectForcedQuotaFallbackOnRoutes`, `setTenantUserFromAccessResult`, `CredentialValidationMiddleware`, `UserQuotaMiddleware`. Upstream call sites: `internal/api/server_routes.go`: one `X.Use(s.tenantPolicy()...)` per group (`/v1`, `/openai/v1`, `/backend-api/codex`, `/v1beta`), `s.tenantChain(realtimeAuth, handler)...` on the six `realtimeAuth` routes, and `s.registerUserRoutes()`; `internal/api/server_middleware.go`: one `setTenantUserFromAccessResult` call in `accessAuthMiddleware`. | Resolves the tenant identity after authentication, adds a soft preferred-auth context for validation traffic, and enforces quota or the configured auto-routing fallback. Policy order is auth → validation → quota → forced-fallback guard. Group routes that cannot honor the fallback model (`/v1/alpha/search`, `/v1/live`, `/v1/live/:call_id`, `/backend-api/codex/alpha/search`) and every policy realtime route fail closed with 429. | `internal/api/server_tenant_policy_test.go`: `TestTenantPolicyRouteMatrix` (every policy route denies an over-quota tenant; realtime helper routes do not), `TestTenantPolicyFallbackGuardOnlyAffectsUnsupportedRoutes`; `internal/api/server_validation_middleware_test.go`: `TestCredentialValidationMiddleware`; `internal/api/server_quota_middleware_test.go`: `TestUserQuotaMiddleware`, `TestQuotaFallbackHTTPExecutionUsesFallbackForExplicitModel`, `TestQuotaFallbackUnsupportedDirectRoutesFailClosed`.<br><br><code>go test ./internal/api -run 'TenantPolicy&#124;CredentialValidation&#124;UserQuota&#124;QuotaFallback'</code> |
| 5a. Fork server lifecycle | Fork-owned: `internal/api/server_fork.go`: `forkRuntime` (`newForkRuntime`, `attachManagement`, `startErr`, `start`, `stop`, `reload`, `tenancyStore`); `internal/api/server_fork_options.go`: `forkOptionConfig`, `WithTenancyService`, `WithOTelUsage`. Upstream call sites: `Server.fork` field, two constructor calls and the `Start`/`Stop` hooks in `server.go`, `serverOptionConfig.fork`, and `s.fork.reload(cfg)` / `s.fork.tenancyStore()` in `server_reload.go`. | Owns the tenancy service, OTel usage sink, tenant user handler, and user-panel supervisor. Initialization errors block `Start`. `Stop` always releases fork resources after the HTTP server closes; an HTTP shutdown error takes precedence and fork errors are then logged. Reload reconfigures tenancy quota, the user handler, and the panel supervisor, and emits the OTel restart notice. | `internal/api/server_tenant_policy_test.go`: `TestForkRuntimeNilSafe`, `TestForkRuntimeStartErrPreventsServing`, `TestForkRuntimeStopClosesTenancyAndReloadReconfigures`, `TestForkRuntimeStopPrefersHTTPShutdownError`; `internal/api/otel_usage_test.go`.<br><br><code>go test ./internal/api -run 'ForkRuntime&#124;OTel'</code> |
| 6. Typed tenancy and auto-routing configuration | Fork-owned files only: `internal/config/tenancy_types.go` (`TenancyConfig`, `TenancyQuota`, `TenancyBalancing`, `ModelPriceOverride`, `UserPanelConfig`, `DefaultUserPanelGitHubRepository`), `autoroute_types.go`, `openrouter_types.go`, `otel_types.go`, and the matching `*_normalization.go` files. `internal/config/config_fork.go`: `applyForkConfigDefaults`, `normalizeForkConfig`. Upstream files keep only the hook calls in `config_load.go` / `parse.go`, the `Config.Tenancy` / `Config.OTel` and `SDKConfig.AutoRouting` / `SDKConfig.OpenRouter` field declarations, and the `tenancy.user-panel.github-repository` known-default case in `config_yaml.go`. | Keeps the fork's server-side tenancy section and handler-facing auto-routing section as typed configuration. Both loaders call the same hooks, so defaults, trimming, lower-casing, OTel validation, and the Tenancy → AutoRouting → OpenRouter order (OpenRouter copies into `Tenancy.Pricing`) cannot diverge. | `internal/config/fork_config_golden_test.go`: `TestForkConfigNormalizationGolden` (golden snapshots in `internal/config/testdata`, loader/parser parity), `TestForkConfigInvalidOTelIsRejectedByBothLoaders`, `TestForkConfigWriteBackOmitsUserPanelDefault`; `internal/config/money_test.go`: `TestTenancyMoneyConfigUsesExactNanoUSD`.<br><br><code>go test ./internal/config</code> |
| 7. Auth-file ownership stamping | Fork-owned: `internal/api/handlers/authfiles/ownership.go`: `StampOwnerFromContext`, `ComposeOwnerStampHook`, `FinalizeTokenRecord`; `internal/api/server_fork.go`: `attachManagement` installs `ComposeOwnerStampHook(customHook)` as the management post-auth hook. Upstream call sites in `internal/api/handlers/management/auth_files_fields.go` `(*Handler).saveTokenRecord`: `defer authfiles.LockRegistration()()` and one `authfiles.FinalizeTokenRecord` call after the custom hook. | Stamps the resolved tenancy user ID into both `Auth.Metadata["owner_user_id"]` and `Auth.Attributes["owner_user_id"]` for user-initiated OAuth, before the custom hook (so the hook observes the owner) and again after it (so the hook cannot replace the owner). Management auth without a tenancy context remains unowned. | `internal/api/handlers/authfiles/ownership_test.go`; `registration_test.go`: `TestFinalizeTokenRecordRestampsOwnerAndRejectsDuplicates`, `TestComposeOwnerStampHookRunsBeforeCustomHook`.<br><br><code>go test ./internal/api/handlers/authfiles ./internal/api/handlers/management</code> |
| 8. Tenant credential uniqueness and auth-file persistence | Upstream management helpers (`isUnsafeAuthFileName`, `buildAuthFromFileData`, `authIDForPath`, `upsertAuthRecord`, `writeAuthFile`) are kept verbatim; `internal/api/handlers/management/auth_files_fork.go` exports `BuildAuthFromFileData`, `UpsertAuthRecord`, `AuthIDForPath` so the management handler satisfies `authfiles.RecordPersister`. Upstream `writeAuthFile` carries two fork calls: `authfiles.PrepareAuthFileWrite` (lock, JSON name, inside-auth-dir, no symlink, `CheckUserRegistration`, `MkdirAll 0700`) and `authfiles.SecureAuthFile` (`chmod 0600`). Tenant uploads use `authfiles.WriteAuthFile` with the persister injected in `user.NewHandler`. `sdk/cliproxy/auth/credential_identity.go`: `CredentialIdentityKeys`; `internal/tenancy/service.go`: `credentialResolver`. | Rejects user registration of existing upstream credentials before persistence, including renamed files and pending watcher loads. Returns an actionable duplicate warning. Counts same-owner copies once for quota and excludes identities also registered to another owner or management. Tenant uploads share the upstream parser (including legacy metadata key normalization) and do not run the post-auth persist hook. Does not repair historical ownership overwrites. | `internal/api/handlers/authfiles/registration_test.go` (duplicate identity, concurrent writes before watcher load, upstream parsing, unsafe destinations, 0600 permissions); `internal/api/handlers/management/auth_files_fork_test.go`: `TestAuthFileNameSafetyParity`, `TestUserExposedOAuthFlowsBindSessionToUser`.<br><br><code>go test ./internal/api/handlers/authfiles ./internal/api/handlers/management ./internal/api/handlers/user ./internal/tenancy</code>; <code>go test -race ./internal/api/handlers/authfiles ./internal/api/handlers/user</code> |
| 9. Fork model catalog overlay | Fork-owned: `internal/registry/models/fork_models_overlay.json` (sections `codex-free`, `codex-team`, `codex-plus`, `codex-pro`, `antigravity`), `internal/registry/model_overlay.go`: `applyForkModelOverlay`, `mergeModelOverlay`, `catalogSections`. Upstream call sites: one `applyForkModelOverlay` line after decoding in `loadModelsFromBytes` and in `fetchModelsFromRemote` (`model_updater.go`). `models.json` is upstream verbatim. | Keeps fork-only models (for example `gpt-5.4`, `gpt-5.4-mini`, `gpt-5.3-codex-spark`, Antigravity Gemini variants) in the embedded, `--local-model`, and remote-refresh catalogs, including after upstream CI regenerates `models.json`. Upstream entries win on a case-insensitive ID match (debug log); remaining overlay entries are appended to their section in order. A malformed overlay is logged and ignored. | `internal/registry/model_overlay_test.go`: embedded order, Codex plan exposure, upstream precedence, malformed overlay, no shared pointers, remote refresh.<br><br><code>go test ./internal/registry -run ForkModelOverlay</code> |

## 4. Candidates to upstream

Seams 1, 2, and 3 are purely additive extension points. When they are unset, the existing
attribute-priority, normal scheduler, and registry-based model-routing behavior remains the default.
Local branches based on `upstream/main` (`v7.3.17`) carry each proposal with tests. They are
**not pushed**; submitting them needs separate approval. If upstream accepts one, delete the
matching fork file in the next sync instead of carrying both versions.

| Branch | Upstream change | Fork cleanup after acceptance |
| --- | --- | --- |
| `upstream-pr/priority-resolver` | `sdk/cliproxy/auth/scheduler_priority.go` (+ test), four hook lines in `scheduler.go` | Delete fork `scheduler_priority.go`; keep `tenancy` calling `SetPriorityResolver` |
| `upstream-pr/preferred-auth-ids` | `sdk/cliproxy/auth/scheduler_preferred.go`, `sdk/api/handlers/handlers_preferred_auth.go` (+ tests), two hooks in `scheduler.go`, one in `handlers.go` | Delete both fork files and their tests |
| `upstream-pr/model-name-resolver` | New generic `BaseAPIHandler.ModelNameResolver` hook applied at the three execution entry points | Replace the fork's `resolveAutoRoutedModel` call sites with a resolver installed from `internal/api`; keep forced-fallback handling fork-owned |

PR body drafts:

- **feat(auth): add optional effective priority resolver to the scheduler.** Embedders sometimes
  need a credential's scheduling priority to follow runtime data (for example, prefer
  credentials whose quota window resets soon) without rewriting the `priority` attribute on
  disk. `Manager.SetPriorityResolver` installs a `func(*Auth) (int, bool)`; the scheduler
  applies it when entries are upserted and re-buckets existing entries on install. Unset, or
  `ok == false`, keeps today's attribute priority, so default behavior is unchanged. Test:
  `TestSchedulerPriorityResolver`.
- **feat(auth): add soft preferred auth IDs first pass.** Middleware can ask the scheduler to try
  specific credentials first (for example to validate a credential with real traffic) without
  pinning. `WithPreferredAuthIDs(ctx, ids)` adds `preferred_auth_ids` to execution metadata. On
  an unpinned first attempt the scheduler runs its normal selection with every non-preferred
  credential treated as already tried; if no preferred credential is ready it falls through to
  normal selection. Cooldowns, disabled state, model support, weights, websocket preference, and
  cursors are unchanged. Tests: `TestSchedulerPreferredAuth*`, `TestPreferredAuthIDsFromMetadata`,
  `TestWithPreferredAuthIDs*`.
- **feat(handlers): add optional model name resolver hook.** Lets embedders map a virtual model
  (for example `auto`) to a concrete model before plugin model routing and provider selection,
  at the non-streaming, streaming, and count entry points. The original requested model is
  still reported under `RequestedModelMetadataKey`. Nil or empty results keep the requested
  model. Test: `TestModelNameResolverExecutionEntryPointsPreserveOriginalRequestedModel`.

The security rows in section 6 (header/query/body redaction, no API key in the usage queue,
source provenance) are separate upstream candidates and have no branch yet.

## 5. Known layering note

The `sdk/` packages importing `internal/` is upstream's own design, not a fork artifact. Do not
attempt to "clean up" that dependency direction during an upstream merge; doing so would create
unrelated conflicts.

The fork's own contribution to that count lives only in fork-owned files:

| File | Fork-added imports | Added lines |
| --- | --- | --- |
| `sdk/api/handlers/handlers_autoroute.go` (fork-owned) | `internal/autoroute` | new file |
| `sdk/api/handlers/handlers_fork.go` (fork-owned) | `internal/autoroute`, `internal/constant`, `internal/tenancy` | new file |

Since the conflict-surface refactor, no upstream `sdk/` file carries a fork-added `internal/` import.


The required current-tree check is:

~~~bash
grep -rn "CLIProxyAPI/v7/internal/" sdk/ --include=*.go | grep -v _test | wc -l
~~~

After the v7.3.17 merge and the conflict-surface refactor it returns `208`: `201` from upstream
(`git grep -n "CLIProxyAPI/v7/internal/" upstream/main -- 'sdk/*.go' | grep -v _test | wc -l`) plus
`7` in fork-owned `sdk/` files. That existing layering is part of the upstream baseline; the
fork-specific changes should remain limited to their documented seams.

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

### Conflict surface after the refactor (Phases 1-5)

| Metric | Before (post-merge) | After |
| --- | --- | --- |
| Modified upstream files | 55 | 45 |
| Lines added / deleted in modified upstream files | `+2,573/-421` | `+1,047/-143` |
| Largest fork hunks in upstream Go files | `config_normalization.go` +341, `config_types.go` +197/-7, `server_middleware.go` +169, `auth_files_crud.go` +15/-134 | `cmd/server/main.go` +54/-7 (unchanged), `server.go` +12, `scheduler.go` +10, `auth_files_crud.go` +9 |
| Upstream files restored verbatim | - | `config_types.go`, `config_defaults.go`, `config_normalization.go`, `server_options.go` (one field), `auth_files.go`, `config_access/provider.go`, `selector.go`, `conductor.go`, `conductor_execution.go`, `handlers_context.go`, `handlers_model_router_test.go`, `models.json` |

Most remaining lines are the security rows (redaction, usage queue, source provenance), docs and
dependencies, and `cmd/server/main.go`, which the refactor intentionally left unchanged.

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
| `internal/registry/models/models.json` | seam (fork catalog entries; now upstream verbatim, see seam 9) | 5 |
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
