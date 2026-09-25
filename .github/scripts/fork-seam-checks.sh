#!/usr/bin/env bash
# Runs every focused fork seam check from docs/fork-patches.md.
# A seam check failing here means an upstream merge silently removed or
# changed a fork extension point, even when the broad test suite passes.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

run() {
	printf '\n==> %s\n' "$*"
	"$@"
}

# Seam 0: scheduler sharing metadata (host side and plugin module).
run go test ./sdk/cliproxy/auth -run 'TestPluginSchedulerReceivesSharingMetadata|TestManagerPluginScheduler|TestManagerInactivePluginScheduler'
(cd custom-plugins/tenancy-scheduler && run go test ./...)

# Seams 1-2: effective priority and preferred-auth first pass.
run go test ./sdk/cliproxy/auth -run '^(TestSchedulerPriorityResolver|TestSchedulerPreferredAuthSingleProvider|TestManagerPreferredCoolingAuthRequestFallsThrough|TestSchedulerPreferredAuthMixedProvider|TestPreferredAuthIDsFromMetadata)$'

# Seam 3: auto-routing before model-router dispatch.
run go test ./sdk/api/handlers -run 'AutoRout|ForcedFallback|PreferredAuth'

# Seam 4: tenant access-provider ordering and key extraction parity.
run go test ./internal/access/...

# Seam 5: tenancy middleware and route insertion, including realtime routes.
run go test ./internal/api -run 'TestCredentialValidationMiddleware|TestUserQuotaMiddleware|TestQuotaFallback|Realtime|TenantPolicy|ForkRuntime|OTel'

# Seam 6: typed tenancy, auto-routing, OpenRouter, and OTel configuration.
run go test ./internal/config

# Seams 7-8: ownership stamping, duplicate registration, and isolation.
run go test ./internal/api/handlers/authfiles ./internal/api/handlers/management ./internal/api/handlers/user ./internal/tenancy
run go test -race ./internal/api/handlers/authfiles ./internal/api/handlers/user

# Seam 9: fork model catalog overlay.
run go test ./internal/registry

printf '\nAll fork seam checks passed.\n'
