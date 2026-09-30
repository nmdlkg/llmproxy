# Tenancy scheduler plugin

This independently built CLIProxyAPI native plugin applies a best-effort sharing
policy to local credential selection and, optionally, a quota-aware utility
optimizer (Linear AX-7). It uses the existing `scheduler.pick` and `usage.handle`
C ABI methods. It does not override the host's fallback behavior.

## Policy

| Credential metadata | Normal plugin selection |
| --- | --- |
| `shared: true` | Eligible |
| `shared: false` | Excluded |
| Owner present, sharing flag missing | Excluded |
| No owner and no sharing flag (ordinary administrator credential) | Eligible |

Legacy string values `"true"` and `"false"` are also supported. Among eligible
candidates, the plugin sorts IDs and rotates with a concurrency-safe global
cursor. There is no owner-only exception: an unshared credential is excluded even
for its owner when the plugin handles selection. Each call reads the current
candidate snapshot, so changing the flag requires no plugin restart.

When no eligible candidate is supplied, the plugin returns `Handled: false`.
**The host may then select an unshared credential.** Disabling/unloading the
plugin, plugin panic recovery, or a higher-priority scheduler can also leave
selection to the host. This is intentionally not an access isolation boundary.

The host prefilters candidates by provider, model support, disabled state,
cooldown, pinned ID, attempted IDs, and highest available priority. The plugin
cannot see shared candidates in lower priority tiers. If a pinned credential or
every candidate in the highest tier is unshared, normal fallback applies. Home
dispatch uses its own selection path and is outside this plugin's scope.

In `legacy` and `shadow` modes the plugin uses its own simple rotation, not the
built-in scheduler's websocket preference, preferred-auth first pass, or
per-model cursor behavior. Only the
highest-priority active scheduler plugin runs; scheduler plugins are not chained.

## Modes

| `mode` | Selection executed | Optimizer |
| --- | --- | --- |
| `legacy` (default) | Sharing filter, then sorted-ID rotation | Off; no usage capability is registered |
| `shadow` | Same as `legacy` | Observes usage, scores every pick, records agreement, reserves on the executed credential |
| `optimizer` | Optimizer argmax among eligible candidates | On |

Sharing policy applies in every mode: the optimizer only ranks shareable
candidates. Invalid configuration fails plugin registration, so the host logs the
error and keeps its own selection instead of running an unintended policy.

In `optimizer` mode set `tenancy.balancing.disabled: true` in the server
configuration (restart required). Otherwise the host reset-window bonus moves
credentials between priority tiers and competes with the optimizer.

## Optimizer model

The optimizer treats selection as constrained stochastic resource allocation:
maximize expected successful requests across future reset windows. It never
generates traffic and never changes the requested model. The code lives in
`optimizer/` (pure Go, no ABI dependency); `main.go` and `scheduler.go` are glue.

**Observations** (`optimizer/window.go`). Every window in a response is kept:
Codex primary and secondary (and named additional limits such as
`x-codex-bengalfox-*`), Anthropic unified `5h`/`7d`/`7d_<scope>` utilization,
and counted token/request families. The window key is
`(account identity, provider, scope, kind)`; kind comes from the reported window
length (`5h`, `7d`), not the primary/secondary slot. A reset time that moves by
more than 90 s starts a new window instance (`Generation`). Percent windows use
native basis points (10000 = 100%); the reporting granularity is kept as
rounding uncertainty. Retry-After is ignored because host cooldowns are
authoritative. The observation time is `RequestedAt + TTFT` (headers arrive at
response start), so a long stream cannot overwrite a newer snapshot. Demand
classes use the requested alias (`UsageRecord.Alias`, falling back to `Model`)
so they match the model seen at pick time.

Account-wide windows (`default`, `tokens`, `requests` scopes) constrain every
request. A model-scoped window (for example `x-codex-bengalfox-*` or Anthropic
`7d_opus`) constrains a class only when the class name contains the scope token
or the window was observed to decrease on a response for that class, so one
exhausted model limit does not block the account for other models. Request
windows consume exactly one unit per request and token windows the class mean
token count.

**Estimation** (`optimizer/estimate.go`). Consumption per window is a decayed
ratio estimator of native units per effective token (`total - 0.9 * cached`),
learned from differences between successive observations of the same instance.
Summing whole intervals makes integer-percent rounding telescope out. Pooled
per-provider/scope/kind estimates cover new accounts; before any evidence, a
prior of `prior-requests-per-window` requests per 5-hour-equivalent with 100%
standard deviation applies. Demand is a seasonal time-of-week hourly EWMA per
class (`provider/model`, dated `-YYYYMMDD` suffixes collapsed), with a global
EWMA fallback. Failed attempts are not counted as demand. Classes are pruned
after `account-ttl` and capped at 256 (least recently seen evicted). Per-credential health
tracks success probability and latency. External consumption shows up as
estimation noise and as `drift-per-hour` uncertainty that grows with
observation age; the optimizer does not claim to know absolute token
entitlements.

**Decision rule** (`optimizer/engine.go`). For a request of class `x`, each
eligible candidate `i` gets

```text
score_i = request_value * p_i - lambda_latency * latency_i - lambda_failure * (1 - p_i)
          - sum_w mu_w * E[d_iw(x)]
```

where `mu_w` is the window's shadow price (marginal future value per native
unit) and `E[d_iw(x)]` its expected consumption. A candidate is admissible when
every window satisfies the chance constraint

```text
b_w - z_w - E[d_w] >= z_(1-epsilon) * sqrt(sd_d^2 + sd_obs^2)
```

with remaining budget `b_w`, in-flight reservations `z_w`, and observation
uncertainty `sd_obs`. The best admissible score wins; exact ties rotate. If no
candidate is admissible, the smallest violation is chosen (upstream 429s and
cooldowns remain authoritative; no new rejection path is added). Accounts
without observations are admitted but charged the highest known opportunity
cost, so they are neither free nor excluded. After a window's reset time passes,
only that window is assumed refilled, and its next reset is taken as the latest
possible time (activation-on-first-use windows start on first request).

**Valuation** (`optimizer/rollout.go`, `optimizer/valuation.go`). A background
worker runs a bounded receding-horizon rollout (approximate DP, not a global
solver): common-random-number demand scenarios over a horizon that covers the
latest known reset (capped by `max-horizon`), with scenario-level demand and
consumption uncertainty, simultaneous windows consumed together, fixed resets
replenishing only their own window, and a terminal value for capacity whose
window does not reset inside the horizon. Shadow prices are finite differences
of the rollout value. A second pass re-evaluates with a continuation policy
that uses the first-pass prices (one approximate policy-iteration step). Work is
bounded by `max-work`: scenarios, then time resolution, are reduced. The pick
path never runs rollouts, I/O, or network calls; it reads an immutable price
snapshot and updates reservations under a short mutex. Windows are indexed by
account and reserved totals are kept incrementally; `BenchmarkEnginePick`
measures about 65 us, 350 us, and 760 us for 32, 200, and 500 candidates.
The rollout time step never exceeds half the shortest window period; when the
work budget is exceeded, scenarios, then horizon, then least-recently-seen
accounts are reduced, and demand cells are capped.

**Reservations.** Each pick reserves the expected consumption on every window of
the executed credential, tagged with the window generation. A usage record
releases the oldest reservation of that credential (FIFO); orphans (rejected by
interceptors, canceled, or selected by host fallback without usage) expire after
`reservation-ttl`. A reservation from an old window instance never depletes a
new one; reservations are dropped as soon as the window's reset time passes.
Reservations are not persisted.

**Scope and limits.**

- Single process only. Several proxy processes sharing OAuth accounts need one
  scheduling owner or a shared atomic reservation authority first.
- By default the optimizer ranks only the highest-priority tier offered by the
  host. `across-priorities: true` opts into the host's generic
  `scheduler_across_priorities` capability; `legacy` and `shadow` decisions
  still honor tier semantics.
- Usage and pick events are correlated per credential, not per attempt, because
  the current ABI has no attempt identifier. Reservation error is bounded by
  FIFO order and the TTL.
- There is no paid-cost signal in the contract, so there is no `lambda_paid`
  term.
- Private-account isolation is not guaranteed; see the fallback note above.

**State.** With `state-path` set, the plugin writes a versioned JSON file
atomically (`checkpoint-interval`, on `plugin.quiesce`, and on shutdown). It
holds windows, estimators, forecasts, health, shadow counters, and a
`diagnostics` summary (price time, rollout value and unserved demand,
reservation count). Per-credential entries are keyed by a SHA-256 of the auth
ID (credential filenames can contain emails), and account identities are
public domain-separated hashes: treat the file as pseudonymous, not anonymous.
It contains no tokens, client API keys, or response headers. A file with
invalid content or an unsupported version is renamed to `*.corrupt-<time>` and
the optimizer starts from an empty belief; on I/O errors the file is kept and
this instance does not write it. `plugin.quiesce` checkpoints and then stops
all writes from the outgoing instance, so a hot replacement owns the file.
Plugin calls recover from panics and return an error envelope instead of
aborting the host. Accounts, windows, and health unseen for
`account-ttl` are pruned, so removed credentials keep no phantom capacity.

## Evaluation

`go test ./optimizer -run Evaluation -v` replays a synthetic 7-day world with
hidden true windows and integer-percent headers against round-robin, the
current host 30-minute/90% bonus, earliest-reset-first, and a binding-reset
greedy baseline (5 fixed seeds, paired 95% intervals). On the committed suite
(near-capacity staggered resets; model-exclusive account with scarce weekly
allowance; mixed short-only/weekly-only accounts) the test logs:

| Policy | Served requests (all scenarios, 5 seeds) |
| --- | --- |
| round-robin | 99,313 |
| legacy 30 m / 90% bonus | 99,466 |
| earliest-reset-first | 100,788 |
| binding-reset greedy | 101,004 |
| optimizer | 101,847 |

Optimizer minus legacy per run: staggered +185 (95% CI 129-241), exclusive
model +148 (137-159), mixed +143 (113-173). The test fails if the optimizer does
not beat legacy in total or regresses by more than 0.5% in any scenario. These
are synthetic results under the stated world model, not production evidence;
set canary thresholds from shadow-mode measurements before enabling
`optimizer`. `TestShadowPricePolicyGapToDPOracle` compares the pricing policy
with exact dynamic programming on small discrete instances (gap 0 on the
committed instances).

## Build and enable

Use a CGO-enabled Go 1.26+ toolchain and C compiler. From this directory:

```sh
go test ./...
go build -buildmode=c-shared -o /tmp/tenancy-scheduler.so .
```

Install the `.so` in the configured plugin directory, using the filename
`tenancy-scheduler.so`. The generated `.h` file is not needed at runtime.
For Linux/amd64, one possible destination is
`<plugins.dir>/linux/amd64/tenancy-scheduler.so`.

Merge this snippet into the server configuration. Start with `shadow`, compare
decisions and outcomes, then switch to `optimizer`:

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    tenancy-scheduler:
      enabled: true
      priority: 100
      mode: shadow                # legacy | shadow | optimizer
      state-path: /var/lib/cliproxyapi/tenancy-scheduler.json
      across-priorities: false
      optimizer:                  # all optional; defaults shown
        epsilon: 0.05             # tolerated overrun probability per known window
        scenarios: 16
        step: 30m
        max-horizon: 192h
        recompute-interval: 1m
        checkpoint-interval: 5m
        stale-after: 30m
        reservation-ttl: 30m
        active-account-window: 24h
        account-ttl: 168h
        request-value: 1
        lambda-latency: 0         # cost per second of latency
        lambda-failure: 0.5       # cost per expected failed attempt
        perturb-requests: 5
        prior-requests-per-window: 500
        demand-cv: 0.3
        consumption-cv: 0.2
        drift-per-hour: 0.005     # external-use uncertainty, fraction of capacity per hour
        terminal-value: 0.5
        max-work: 50000000
        seed: 1

tenancy:
  balancing:
    disabled: true                # only with mode: optimizer
```

Use a priority higher than other scheduler plugins. Ensure the server loads the
library through its normal plugin reload/startup flow. Rollback: set
`mode: legacy` (or disable the plugin) and restore `tenancy.balancing.disabled:
false`. This repository change does not install the library or change a running
server's configuration.

## Host integration and sync cost

Host connections, all listed in `docs/fork-patches.md`:

- `schedulerAuthCandidates` in `sdk/cliproxy/auth/conductor_selection.go`
  populates candidate metadata through `schedulerCandidateMetadata`. It copies
  only scalar `owner_user_id` and `shared` fields plus an opaque
  `account_identity` hash of `account_id` combined with `email` (or `sub`),
  so seats of one Codex workspace stay distinct. Tokens and arbitrary
  credential metadata are not exposed.
- `tenancy.balancing.disabled` (default `false`) turns off the host urgency
  bonus for optimizer mode.

Quota headers and consumption arrive through the existing upstream usage
capability. The optimizer does not import `internal/tenancy`, write the tenant
USD ledger, copy auth files, or refresh credentials. `SchedulerAuthCandidate.Metadata`
already exists in the SDK, so no ABI/schema extension or scheduler fallback
patch is needed.

The local `go.mod` replacement points to the host SDK for development. This
directory can be extracted into a separate repository by replacing that directive
with a pinned compatible SDK version. The library still requires a host carrying
the metadata forwarding change; a host without it cannot distinguish private
credentials from administrator credentials.

After upstream sync, run the host integration and fallback checks from the root:

```sh
go test ./sdk/cliproxy/auth -run 'TestPluginSchedulerReceivesSharingMetadata|TestSchedulerCandidateMetadataAccountIdentity|TestManagerPluginScheduler|TestManagerInactivePluginScheduler'
(cd custom-plugins/tenancy-scheduler && go build -buildmode=c-shared -o /tmp/tenancy-scheduler.so .)
TENANCY_SCHEDULER_PLUGIN=/tmp/tenancy-scheduler.so go test ./internal/pluginhost -run TestNativeTenancySchedulerSmoke
```

Then run this module's tests separately: root `go test ./...` skips nested modules.

This split reduces conflicts in the upstream scheduler while retaining small,
tested host connections. Moving the entire tenancy system to plugins is a separate
project: user API authentication, ownership persistence, quota enforcement, and
usage storage have broader host dependencies. External panel assets and native
backend plugins are distinct extension mechanisms.

References: [plugin development](https://help.router-for.me/plugin/development.html),
[scheduler capability](https://help.router-for.me/plugin/scheduler.html), and the
upstream example in `examples/plugin/scheduler`.
