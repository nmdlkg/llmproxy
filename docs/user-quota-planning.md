# User quota planning

User quota enforcement continues to use the exact configured rolling window
(default 168 hours). No synchronous hourly replenishment or provider manual
reset action is introduced. The scheduler optimizer remains a separate project
in `quota-optimizer-issue-draft.md`.

The user dashboard now consumes two authenticated endpoints:

- `/v0/user/usage/timeline?resolution=1h&days=7`: past hourly usage.
- `/v0/user/usage/releases?resolution=1h&hours=48`: scheduled expiration amounts.

Both support paired RFC3339 `from` and `to` timestamps and `resolution=15m`
for detail ranges up to 24 hours. Hourly ranges are limited to seven days.
History cannot extend into the future; explicit historical ranges remain valid
when a chart ages beyond the default seven-day lookback. Release detail clips
elapsed time from intervals that have not ended. Fully elapsed intervals return
400. The dashboard can inspect a selected hour.

The store groups rows directly in SQLite using `(user_id, occurred_at)` range
filters. Release grouping adds the configured duration before assigning the
bucket, so non-hour-aligned windows work correctly. Each response contains at
most 169 hourly or 97 quarter-hour intervals, including partial edge intervals.
No raw ledger timestamps are returned. Grouping reduces transferred rows, not
the number of matching ledger entries SQLite must aggregate.

Planning snapshots have a 30-second server cache, bounded to 512 entries;
concurrent requests for the same user/query/window are coalesced. Cache keys
use validated query semantics, ignoring unrelated parameters and query ordering. Different
users do not hold a shared lock while querying the database. The cache is
independent of enforcement and does not change quota admission. New ledger
entries and limit changes become visible after its TTL. Responses include
`generated_at` and `cache_ttl_seconds`; HTTP responses use `Cache-Control:
no-store`.

Money uses integer nano-USD strings. Release snapshots also supply
`used_nano_usd` and `limit_nano_usd`. Releases and the origin usage balance are
read in one SQLite transaction, both excluding entries at or after the snapshot
origin. The UI calculates remaining at interval end
as `clamp(limit - used + cumulative releases, 0, limit)`, assuming no new usage
or limit changes. This preserves over-limit debt and avoids mixing cached
release amounts with a newer remaining balance. Forecasts are estimates based
on recorded usage, not reservations or guarantees of provider capacity.

Quota responses add `mode: rolling` and nullable `next_release_at`. The legacy
`reset_at` is retained as the next ledger expiration alias; it is null without
usage. The dashboard replaces full-reset wording with next usage expiration,
labels browser-local time zones, and displays exact amounts in accessible
tables alongside charts. Existing provider quota and management reset UI are
unchanged.

Build the sibling User Panel and use the existing local asset development
configuration to review it. Building the sibling does not automatically update
the proxy's embedded/published panel.
