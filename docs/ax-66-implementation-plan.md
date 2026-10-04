# AX-66 Usage Forecast and Load-Balancing Plan

## Goal

Add a tenant-scoped weekly usage planning response that combines usage ledger observations with provider quota snapshots. The response must expose observed usage, a deterministic next-week baseline forecast, remaining capacity when the provider exposes an authoritative limit, and API price equivalent with reproducible provenance. Forecast output is advisory and shadow-only; it must not change routing eligibility.

## Scope for AX-66

Implementation order is fixed: collection and additive migration → tenant-safe aggregation → explicit deterministic baseline → read-only API → shadow adapter. The two implementation children are AX-71 (observation, forecast, pricing API) and AX-75 (advisory capacity snapshot and planning UI). AX-79 is the single deferred follow-up for routing activation, entitlement authority, external integrations, pooling, and historical repricing.

### 1. Canonical observation

Introduce a normalized observation projection over the existing usage ledger and quota-window records. The projection is keyed by `(user_id, canonical_auth_id, provider, plan, model_family, quota_scope, window)` and contains:

- input, output, reasoning, cache-read, cache-creation, and total tokens;
- request, success, failure, and quota-event counts;
- provider-native remaining/used values, reset timestamps, observation timestamp, and source;
- data freshness, coverage, confidence, and an external-usage/unknown flag.

Credential secrets, access tokens, and raw response headers are never persisted or returned. Missing plan, model family, or quota scope remains explicitly unknown rather than being inferred from a credential filename or account alias.

The current ledger does not retain all of this information. Add additive nullable columns (or a versioned observation table) for plan, model family, quota scope, detailed token categories, source, and price-catalog version. Existing rows migrate as `unknown` and are never silently backfilled. Keep `canonical_auth_id` internal; API responses use a stable redacted credential handle. A provider/account identity resolver must be explicit and versioned so two aliases cannot create duplicate capacity, while unrelated credentials cannot be merged.

The existing `quota_windows` row is a latest snapshot keyed by auth/provider. Do not change that meaning. Store multi-window observations separately with `quota_scope_id`, `window_kind`, `native_unit`, `observed_at`, `reset_at`, authority, and source. A shared credential's global quota is never presented as a tenant-owned entitlement; tenant drill-down only includes credentials whose ownership is currently authorized, and shared values are anonymized or aggregated.

### 2. Aggregation and storage boundary

Extend `internal/tenancy` with a read-only aggregation method that returns provider, auth, plan, model-family, and quota-scope rows for a bounded UTC interval. Reuse existing `UsageEntry`, `QuotaWindow`, and pricing resolution. Keep provider percentage/native units separate from token counts. Use `canonical_auth_id + quota_scope` as the deduplication key when combining snapshots; do not add capacity twice for aliases or shared scopes. Define precedence when multiple snapshots overlap: newest observation wins for freshness, while usage ledger rows remain additive and are never replaced by quota percentages.

Existing timeline and release endpoints remain backward compatible. New aggregation code must enforce the authenticated user boundary in the SQL query and must not expose raw ledger timestamps or credential metadata.

### 3. Deterministic forecast

Add a pure forecast package with a controllable clock supplied by callers. Its input and output types must be independent of Gin, SQLite, and provider-specific auth structs.

- With at least seven days of observations, calculate an EWMA demand baseline and a conservative interval.
- Use a fixed seven-day horizon, UTC hourly buckets, `alpha = 0.3`, and the most recent complete bucket as the initial EWMA value. A forecast interval is a heuristic range, not a statistical confidence interval, until calibrated against held-out observations.
- With less data, use the latest complete window and mark confidence `low` with explicit coverage.
- Widen the interval for reset crossings, stale quota observations, plan changes, model-mix changes, or conflicting usage/quota signals.
- Return `observed_weekly_tokens`, `projected_next_week_tokens`, `estimated_available_tokens` only when an entitlement is authoritative, and `remaining_until_reset` when a reset window is known.
- If only a provider percentage or native unit is available, return that native value and an unknown token entitlement instead of manufacturing a token cap.

Define `coverage = observed_duration / requested_training_duration`, distinguish no observations (`unknown`) from a complete observed zero-demand period (`zero`), and treat a snapshot whose reset has passed as stale/unknown. Preserve observation time per value; a fresh reset timestamp does not make an old used/limit pair authoritative.

The forecast must be deterministic for identical observations and timestamps, and it must never mutate routing state.

Persist the forecast inputs needed for audit (observation IDs, algorithm version, generated timestamp, and catalog version) or return them in the response. A later catalog update must not silently reprice an already generated result.

### 4. API price equivalent

Use the existing model pricing catalog and override precedence to calculate a separate API price equivalent for input, output, reasoning, cache read, and cache creation tokens. Include:

- `price_catalog_version`;
- effective timestamp/date;
- model/provider source;
- missing-price fields and an overall `price_status`.

Derive a content hash plus mapping/override version as the catalog version. Do not call fetch time the provider's effective date. Existing ledger cost remains immutable; current price-equivalent calculations use a separate basis and return `unknown` when historical provenance is unavailable.

This value is an API list-price equivalent. It is not a subscription charge, provider entitlement, or credential quota.

### 5. User API response

Add a new bounded, read-only `GET /v0/user/usage/forecast` response. It must support provider → plan → credential → model-family drill-down and include the interval, generated timestamp, source, freshness, confidence, coverage, observed/projected/remaining values, units, reason codes, stable ordering, truncation metadata, and advisory marker. Existing timeline/release fields and response semantics remain unchanged. Use null for unknown values and zero only for observed complete zeroes; never overload zero as missing.

Use an additive `schema_version` response and a stable error contract for unavailable providers. Cap row counts and query spans, and avoid returning raw `auth_id`, source URLs containing credentials, or arbitrary provider header values. The handler must use the authenticated user from middleware for every query; never accept a user ID from the request body or query string.

### 6. Shadow load-balancing output

Expose a normalized capacity snapshot and advisory plan containing credential preservation/use priority, predicted exhaustion risk, and model-exclusive capacity. Define the shadow consumer contract for AX-7/AX-65, including algorithm version, stale threshold, disabled/cooldown filters, unsupported-model handling, and stable snapshot IDs. Mark every result advisory. Upstream quota, 429, cooldown, disabled credentials, and current round-robin behavior remain authoritative. Shared-account global exhaustion is reported as a limitation when tenant demand cannot establish it.

## Acceptance matrix

| AX-66 requirement | Component | Acceptance evidence |
| --- | --- | --- |
| Provider/auth/plan/model observed and projected usage | AX-72 + AX-71 | Tenant-isolated aggregation and deterministic forecast fixtures |
| Remaining capacity with uncertainty | AX-72 + AX-71 | Multi-window, stale-reset, unknown-entitlement tests |
| API price equivalent and provenance | AX-78 | Catalog hash/version and missing-category tests |
| Weekly planning drill-down | API + AX-74 | Bounded endpoint contract and stable ordering tests |
| Advisory load-balancing plan | AX-75 | Shadow snapshot comparison proves no selector mutation |
| No false precision or secret leakage | All | Unknown/null semantics, redaction, shared-credential tests |

## Tests and verification

- SQL aggregation and ownership isolation across users.
- Alias/shared-quota deduplication and unknown quota scope.
- EWMA, low-data fallback, confidence/coverage, stale data, and reset crossing.
- Provider percentage/native-unit separation from token capacity.
- Price catalog versioning, missing prices, and all token categories.
- Secret/raw-header redaction.
- Additive migration from old ledger rows and rollback/read compatibility with the previous binary.
- SQLite reopen and migration rollback-failure behavior.
- Canonical identity resolver tests for same-account aliases and distinct accounts sharing a plan.
- Repeated requests return the same algorithm/catalog version for the same snapshot.
- Bounded query spans and maximum drill-down row counts.
- Backward compatibility for timeline and release endpoints.
- Forecast errors and stale snapshots cannot change routing eligibility.

Required checks:

```bash
gofmt -w .
go test ./...
go build -o test-output ./cmd/server && rm test-output
```

## Deferred work

The following are intentionally outside AX-66 and require a separate design or canary:

1. Automatically changing routing weights or selection order from forecast output.
2. Treating community plan labels (for example Pro 20x or Max 20x) as official token entitlements.
3. Browser-cookie/session scraping or external token/cost analytics services.
4. Cross-tenant or global capacity pooling.
5. A production optimizer rollout before shadow telemetry, canary thresholds, and rollback criteria are available.
6. Historical repricing when a provider changes its published catalog.
7. Destructive schema changes or a migration that requires the previous release to be offline.
8. Automatic discovery of canonical identities from browser sessions or provider cookies.

## Proposed follow-up sub-issues

- **AX-71 — usage observation, forecast, and pricing API**: implement additive observation storage, canonical aggregation, deterministic forecast, and versioned price provenance.
- **AX-75 — advisory capacity snapshot and planning UI**: expose bounded drill-down output and connect it to AX-7/AX-65 shadow telemetry without changing routing.
- **AX-79 — deferred routing and entitlement follow-ups**: keep automatic routing activation, authoritative subscription entitlements, external integrations, cross-tenant pooling, and historical repricing out of AX-66.
