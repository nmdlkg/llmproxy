# Tenancy scheduler plugin

This independently built CLIProxyAPI native plugin applies a best-effort sharing
policy to normal local credential selection. It uses the existing `scheduler.pick`
C ABI. It does not override the host's fallback behavior.

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

The plugin uses its own simple rotation, not the built-in scheduler's websocket
preference, preferred-auth first pass, or per-model cursor behavior. Only the
highest-priority active scheduler plugin runs; scheduler plugins are not chained.

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

Merge this snippet into the server configuration:

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    tenancy-scheduler:
      enabled: true
      priority: 100
```

Use a priority higher than other scheduler plugins. Ensure the server loads the
library through its normal plugin reload/startup flow. This repository change
does not install the library or change a running server's configuration.

## Host integration and sync cost

The only existing host implementation changed is `schedulerAuthCandidates` in
`sdk/cliproxy/auth/conductor_selection.go`, which now populates candidate metadata
through `schedulerSharingMetadata`. That helper copies only scalar
`owner_user_id` and `shared` fields. Tokens and arbitrary credential metadata
are not exposed. `SchedulerAuthCandidate.Metadata` already exists in the SDK,
so no ABI/schema extension or scheduler fallback patch is needed.

The local `go.mod` replacement points to the host SDK for development. This
directory can be extracted into a separate repository by replacing that directive
with a pinned compatible SDK version. The library still requires a host carrying
the metadata forwarding change; a host without it cannot distinguish private
credentials from administrator credentials.

After upstream sync, run the host integration and fallback checks from the root:

```sh
go test ./sdk/cliproxy/auth -run 'TestPluginSchedulerReceivesSharingMetadata|TestManagerPluginScheduler|TestManagerInactivePluginScheduler'
```

Then run this module's tests separately: root `go test ./...` skips nested modules.

This split reduces conflicts in the upstream scheduler while retaining one small,
tested host connection. Moving the entire tenancy system to plugins is a separate
project: user API authentication, ownership persistence, quota enforcement, and
usage storage have broader host dependencies. External panel assets and native
backend plugins are distinct extension mechanisms.

References: [plugin development](https://help.router-for.me/plugin/development.html),
[scheduler capability](https://help.router-for.me/plugin/scheduler.html), and the
upstream example in `examples/plugin/scheduler`.
