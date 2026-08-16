# Fork patch ledger and upstream tracking

This repository is a fork of `github.com/router-for-me/CLIProxyAPI/v7`. The fork started at
upstream tag `v7.2.101` (`42a00a2`) and adds multi-tenancy, quota, automatic model routing, the
OpenRouter catalog, OTel usage export, and the `/v0/user` API with its embedded dashboard.

## 1. Why this file exists

The fork remains a normal upstream-tracking fork rather than becoming a plugin. Upstream changes
must be merged periodically, while the fork-specific patch seams must remain visible and reviewable.

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
| 1. Effective credential priority | `sdk/cliproxy/auth/selector.go`: `PriorityResolver`, `(*Manager).SetPriorityResolver`; `sdk/cliproxy/auth/scheduler.go`: `authScheduler.priorityResolver`, `buildScheduledAuthMeta` | Lets tenancy compute an effective credential priority. A resolver result is used when it returns `ok == true`; otherwise the existing auth attribute priority remains the fallback. The fast scheduler applies the resolver when entries are upserted and refreshes its buckets when the resolver is installed. | `sdk/cliproxy/auth/scheduler_seams_test.go`: `TestSchedulerPriorityResolver`<br><br><code>go test ./sdk/cliproxy/auth -run '^TestSchedulerPriorityResolver$'</code> |
| 2. Preferred-auth-IDs first pass | `sdk/cliproxy/auth/scheduler.go`: `pickSingleWithStrategy`, `pickMixedWithStrategy`; `sdk/cliproxy/auth/conductor_execution.go`: `PreferredAuthIDsMetadataKey`, `preferredAuthIDsFromMetadata` | Carries a soft list of preferred credential IDs through request metadata. On the first pick, ready compatible preferred credentials are tried before normal priority selection; pinned credentials, tried credentials, cooldowns, disabled credentials, and incompatible models still take precedence or fall through as designed. The metadata constant is defined in `conductor_execution.go` as `preferred_auth_ids`. | `sdk/cliproxy/auth/scheduler_seams_test.go`: `TestSchedulerPreferredAuthSingleProvider`, `TestManagerPreferredCoolingAuthRequestFallsThrough`, `TestSchedulerPreferredAuthMixedProvider`, `TestPreferredAuthIDsFromMetadata`<br><br><code>go test ./sdk/cliproxy/auth -run '^(TestSchedulerPreferredAuthSingleProvider|TestManagerPreferredCoolingAuthRequestFallsThrough|TestSchedulerPreferredAuthMixedProvider|TestPreferredAuthIDsFromMetadata)$'</code> |
| 3. Auto-routing before model-router dispatch | `sdk/api/handlers/handlers_routing.go`: `resolveAutoRoute`, `resolveAutoRoutedModel`, `applyModelRouter`; `sdk/api/handlers/handlers_execution.go`: non-streaming and count call sites; `sdk/api/handlers/handlers_stream.go`: streaming call site | Resolves an enabled `auto` or `auto:<tier>` model before `applyModelRouter`, while preserving the existing registry fallback when auto-routing is disabled or cannot resolve. The package variable `resolveAutoRoute` is the testable resolver seam. All three execution entry points keep the original requested model in metadata after resolution. | `sdk/api/handlers/handlers_autoroute_test.go`: `TestResolveAutoRoutedModel`, `TestAutoRoutingExecutionEntryPointsPreserveOriginalRequestedModel`<br><br><code>go test ./sdk/api/handlers -run '^(TestResolveAutoRoutedModel|TestAutoRoutingExecutionEntryPointsPreserveOriginalRequestedModel)$'</code> |
| 4. Tenant access-provider ordering | `internal/access/reconcile.go`: `ApplyAccessProviders`, `userProviderFirst`; `sdk/access/registry.go`: `SetExclusiveProvider`, `ClearExclusiveProvider`, `RegisteredProviders` | Moves the tenant provider to index 0 after reconciliation. The SDK registry has an exclusive-provider restriction but no priority concept, so ordering is the mechanism that makes tenant authentication win when both tenant and service providers can authenticate the same request. | `internal/access/reconcile_test.go`: `TestApplyAccessProvidersOrdersAndDisablesTenantProvider`<br><br><code>go test ./internal/access -run '^TestApplyAccessProvidersOrdersAndDisablesTenantProvider$'</code> |
| 5. Tenancy middleware and route insertion | `internal/api/server_middleware.go`: `CredentialValidationMiddleware`, `UserQuotaMiddleware`, `credentialValidationMiddleware`, `userQuotaMiddleware`; `internal/api/server_routes.go`: `setupRoutes` and the `.Use()` calls on `/v1`, `/openai/v1`, `/backend-api/codex`, and `/v1beta` | Resolves the tenant identity after authentication, adds a soft preferred-auth context for validation traffic, and enforces quota or the configured auto-routing fallback. The route groups install the middleware before their API handlers; unsupported direct routes remain fail-closed when fallback cannot be applied. | `internal/api/server_validation_middleware_test.go`: `TestCredentialValidationMiddleware`; `internal/api/server_quota_middleware_test.go`: `TestUserQuotaMiddleware`, `TestQuotaFallbackHTTPExecutionUsesFallbackForExplicitModel`, `TestQuotaFallbackUnsupportedDirectRoutesFailClosed`<br><br><code>go test ./internal/api -run '^(TestCredentialValidationMiddleware|TestUserQuotaMiddleware|TestQuotaFallbackHTTPExecutionUsesFallbackForExplicitModel|TestQuotaFallbackUnsupportedDirectRoutesFailClosed)$'</code> |
| 6. Typed tenancy and auto-routing configuration | `internal/config/config_types.go`: `TenancyConfig`, `TenancyQuota`, `TenancyBalancing`, `AutoRoutingConfig`, `AutoRoutingTier`; `internal/config/config_normalization.go`: `SanitizeTenancyConfig`, `SanitizeAutoRoutingConfig`; `internal/config/sdk_config.go`: `SDKConfig.AutoRouting`; `internal/config/config.go`: `Config.Tenancy` | Keeps the fork's server-side tenancy section and handler-facing auto-routing section as typed configuration. Parsing and sanitization apply defaults, normalize keys and model lists, preserve the explicit disabled-by-default behavior, and expose the canonical structures consumed at runtime. | `internal/config/money_test.go`: `TestTenancyMoneyConfigUsesExactNanoUSD`; the package test also compiles and exercises the shared parse/normalization pipeline used by both sections.<br><br><code>go test ./internal/config</code> |
| 7. Auth-file ownership stamping | `internal/api/handlers/authfiles/ownership.go`: `StampOwnerFromContext`; `internal/api/handlers/management/auth_files_fields.go`: `(*Handler).saveTokenRecord` calls `StampOwnerFromContext` before and after the custom post-auth hook | Stamps the resolved tenancy user ID into both `Auth.Metadata["owner_user_id"]` and `Auth.Attributes["owner_user_id"]` for user-initiated OAuth. The second stamp prevents a custom hook from replacing the resolved owner; management auth without a tenancy context remains unowned. | `internal/api/handlers/authfiles/ownership_test.go`: `TestStampOwnerFromContextUsesResolvedUser`, `TestStampOwnerFromContextLeavesManagementAuthUnowned`; the management package is included so the `saveTokenRecord` call sites compile.<br><br><code>go test ./internal/api/handlers/authfiles ./internal/api/handlers/management</code> |

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
