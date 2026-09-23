# CLIProxyAPI admin-level runbook

This runbook covers administration of a shared, multi-tenant CLIProxyAPI
deployment. End-user setup is in [USER_GUIDE.md](../USER_GUIDE.md).

Example deployment:

- proxy: nmdl-2, 100.110.30.57:8317
- config: /etc/cliproxyapi/config.yaml
- binary: /opt/cliproxyapi/bin/cliproxyapi
- service: system-level cliproxyapi.service
- auth data: /var/lib/cliproxyapi/auths (owned by `nobody:nogroup`)

Replace these values for another deployment. Do not use old workstation paths
such as ~/.config/cliproxyapi for a system service.

## Operating rules

- The management secret and an admin user's cp_u key are different credentials.
  The management secret cannot call the user-admin API.
- User keys are shown once and stored only as hashes. Transfer them through an
  approved secret-sharing channel.
- In multi-tenancy, oauth-model-alias and payload.override are global. They
  affect every matching tenant and are not per-user settings.
- Back up live configuration before editing it. Merge lists and do not create
  duplicate top-level YAML keys.
- Never print keys, OAuth tokens, or plaintext response files in logs.
- When an assistant needs sudo, escalate the exact command and purpose to the
  operator after completing unprivileged preparation. The operator runs it in a
  trusted shell; never request a sudo password in chat. Sandbox approval does
  not supply OS-level sudo authentication.

## Onboarding checklist

For a new deployment or handover:

1. Confirm the systemd unit's `User`, `ExecStart`, and `--config` values.
2. Back up the live config before enabling tenancy or changing mappings.
3. Ensure the service account can read the config and `auths/`, with no
   group- or world-readable secret files.
4. Enable `tenancy.enabled`, restart, and verify `/healthz`.
5. Create the administrator in section 1, then regular users in section 2.

For an existing deployment, begin with the read-only systemd check in section 3
and record the actual paths before making changes.

## 1. Bootstrap or recover an admin

Enable tenancy.enabled in the system config, then stop the service:

~~~bash
sudo systemctl stop cliproxyapi.service
sudo -u nobody /opt/cliproxyapi/bin/cliproxyapi \
  -config /etc/cliproxyapi/config.yaml \
  --create-admin-user you@example.com
sudo systemctl start cliproxyapi.service
sudo systemctl is-active cliproxyapi.service
~~~

The key is printed exactly once. Running the command for an existing email issues
a recovery key instead of creating a duplicate. Use the actual service account,
binary, and config path from the systemd unit.

## 2. Create and manage users

Prepare a temporary curl config on a trusted device. This keeps the admin key
out of the curl command line:

~~~bash
PROXY_URL=http://100.110.30.57:8317
read -rsp 'Admin user API key: ' ADMIN_KEY
echo
admin_auth_file="$(mktemp)"
chmod 600 "$admin_auth_file"
printf 'header = "Authorization: Bearer %s"\n' "$ADMIN_KEY" > "$admin_auth_file"
unset ADMIN_KEY
trap 'rm -f "$admin_auth_file"' EXIT
~~~

The key must belong to a user with role admin. Do not use
remote-management.secret-key.

Create a regular user:

~~~bash
read -rp 'User email: ' USER_EMAIL
read -rp 'Display name: ' USER_DISPLAY_NAME
invite_response="$(mktemp)"
chmod 600 "$invite_response"

curl --fail-with-body -sS --config "$admin_auth_file" \
  -X POST -H 'Content-Type: application/json' \
  -d "$(jq -n --arg email "$USER_EMAIL" --arg display_name "$USER_DISPLAY_NAME" \
    '{email:$email,display_name:$display_name,role:"user",tier:"default",key_label:"initial"}')" \
  "$PROXY_URL/v0/user/admin/users" > "$invite_response"

jq '{user, api_key}' "$invite_response"
rm -f "$invite_response"
unset invite_response USER_EMAIL USER_DISPLAY_NAME
~~~

Transfer api_key through an approved secret-sharing channel. There is no email
delivery or invitation link. The user can then open PROXY_URL/user, issue
device-specific keys, and revoke the initial key.

List users and count enabled regular users:

~~~bash
curl --fail-with-body -sS --config "$admin_auth_file" \
  "$PROXY_URL/v0/user/admin/users" |
  jq -r '(["ID","EMAIL","DISPLAY_NAME","TIER","DISABLED"]|@tsv),
    (.users|sort_by(.email)[]|select(.role=="user")|
    [.id,.email,.display_name,.tier,(.disabled|tostring)]|@tsv)'

curl --fail-with-body -sS --config "$admin_auth_file" \
  "$PROXY_URL/v0/user/admin/users" |
  jq '[.users[]|select(.role=="user" and .disabled==false)]|length'
~~~

Disable or re-enable an account after resolving its ID:

~~~bash
USER_ID='<user-id>'
curl --fail-with-body -sS --config "$admin_auth_file" \
  -X PATCH -H 'Content-Type: application/json' \
  -d '{"disabled":true}' \
  "$PROXY_URL/v0/user/admin/users/$USER_ID" | jq .
# Re-enable with {"disabled":false}
~~~

Disabling keeps the record but prevents its keys from authorizing requests.
Remove the temporary auth file when finished:

~~~bash
rm -f "$admin_auth_file"
trap - EXIT
unset admin_auth_file USER_ID PROXY_URL
~~~

## 3. Decide whether a Codex bump needs deployment

This fork has separate catalog paths:

- internal/registry/models/models.json contains general provider/model definitions.
- internal/registry/models/codex_client_models.json contains Codex client metadata
  and capabilities. Listing an ID alone does not verify this metadata.

Both files are embedded at build time. Editing a checkout does **not** change
an installed binary or running process. Remote updates are held in memory;
they do not update the checkout or installed binary.

| Mode / finding | Required action | Rebuild / redeploy? |
| --- | --- | --- |
| Normal mode, both remote catalogs include the model | Verify both runtime catalogs and an authenticated request. Updaters run at startup and every 3 hours. | No. A restart is optional to trigger startup fetching sooner. |
| Normal mode, refresh failed or remote catalogs lack the model | Check logs, source catalogs, and connectivity. A restart only retries fetching. | Not by default. An embedded-only bump can be replaced by a later successful remote refresh. |
| `--local-model`, installed embedded catalogs lack the model | Update both embedded catalogs, build, install, and restart while retaining `--local-model`. | Yes, even for a catalog-only change. |
| Home mode | Update/verify model IDs in Home. Without `--local-model`, the Codex client metadata updater still runs. | No for a Home model-list change; local `models.json` cannot add Home IDs. |
| Running binary lacks a required updater, protocol capability, or runtime fix | Prepare and validate the implementation update, then use section 5. | Yes. |
| Only an optional `-fast` alias is needed | Merge the global alias and payload rule in section 4. | No; this is configuration. |
| Model works, but its local name differs from the OpenRouter pricing ID | Add an explicit pricing mapping as described below. | No; this is configuration. |

Start with this read-only check (no sudo when systemd permits it):

~~~bash
systemctl show cliproxyapi.service -p ActiveState -p ExecStart -p User -p FragmentPath
~~~

Escalate the following checks to the operator if privileged access is needed.
Inspect flags and logs locally; do not paste secrets from units or logs:

~~~bash
sudo systemctl cat cliproxyapi.service
sudo journalctl -u cliproxyapi.service --since '6 hours ago' --no-pager |
  rg -i 'model|catalog|home|local-model|error'
~~~

The operator should also inspect `home.enabled` in the actual config locally.
Neither `ActiveState=active` nor the absence of `--local-model` proves that
remote refresh succeeded or that Home mode is disabled.

The updaters try these source pairs in order:

- https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/models.json
  then https://models.router-for.me/models.json
- https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/codex_client_models.json
  then https://models.router-for.me/codex_client_models.json

Source contents are not proof that the running service loaded them. Use a user
key for the tenant being checked, not the management secret, to inspect both
effective catalogs. Run this in a trusted shell; retain the temporary auth file
for section 4 if needed:

~~~bash
PROXY_URL=http://100.110.30.57:8317
NEW_MODEL=gpt-6-astra
read -rsp 'User API key: ' CLIPROXY_API_KEY
echo
model_auth_file="$(mktemp)"
chmod 600 "$model_auth_file"
printf 'header = "Authorization: Bearer %s"\n' "$CLIPROXY_API_KEY" > "$model_auth_file"
unset CLIPROXY_API_KEY
trap 'rm -f "$model_auth_file"' EXIT

curl -fsS --config "$model_auth_file" "$PROXY_URL/v1/models" |
  jq --arg model "$NEW_MODEL" '[.data[] | select(.id == $model)]'

curl -fsS --config "$model_auth_file" \
  "$PROXY_URL/v1/models?client_version=0.153.0" |
  jq --arg model "$NEW_MODEL" '[.models[] | select(.slug == $model) |
    {slug, context_window, max_context_window, supported_reasoning_levels,
     tool_mode, multi_agent_version, use_responses_lite}]'
~~~

Use the client's actual version for `client_version`. Compare metadata with
the matching upstream entry; unknown IDs can receive a generic template. An
empty result means this tenant cannot currently discover the model. Check
plan, credentials, exclusions, and Home configuration before rebuilding.

For this GPT-6 Astra bump, only the two embedded catalogs changed: the upstream
entry was added to Codex Team, Plus, and Pro definitions and the Codex client
catalog. No executor or translator changed. Both remote catalogs already
contained Astra when checked. This prepares future builds and local-model
deployments; it does not itself require redeploying a normal remote-update
deployment. Live refresh and account access still require verification.

Before declaring the bump operational, make a small authenticated Responses
request with the exact model ID and a supported reasoning level. Check a
completed response, reported model, and tenant usage attribution. Test the
alias separately only if configured. Discovery and `/healthz` alone do not
prove upstream execution works. Never paste API keys or OAuth tokens into chat.
Remove the temporary auth file after all checks using the cleanup in section 4,
even when skipping alias configuration.

### Resolve model-name mismatches in usage pricing

A model can execute successfully while its usage is priced incorrectly. The
OpenRouter pricing matcher only accepts unique case-insensitive matches after
removing a vendor prefix or date suffix; it does not convert version punctuation.
For example, local `claude-fable-5-1` does not match the catalog ID
[`anthropic/claude-fable-5.1`](https://openrouter.ai/anthropic/claude-fable-5.1).
Without an explicit mapping, the proxy can use fallback token prices, while
cc-otel can report the tokens as unpriced and omit their cost.

Verify the exact target ID in the OpenRouter catalog, then merge this entry into
the existing `/etc/cliproxyapi/config.yaml` mapping:

~~~yaml
openrouter:
  model-map:
    "claude-fable-5-1": "anthropic/claude-fable-5.1"
~~~

Catalog pricing requires `openrouter.enabled: true` and
`openrouter.cost-basis: openrouter`. This mapping affects accounting, not the
upstream request model; do not create an OAuth routing alias to fix pricing.
Follow [the configuration change procedure](build-and-deploy.md#4-configuration-changes)
for backup, ownership, permissions, and restart. No binary rebuild is required.

cc-otel has independent mappings in `usage_cost_exporter/cost_exporter.py` and
`usage_reporter/monthly_report.py`; both include this Fable mapping. Its
`MODEL_ID_OVERRIDES` environment setting takes precedence over built-in entries.
Updating proxy configuration alone does not update cc-otel. After an exporter
mapping change, restart the exporter and verify that the model disappears from
`ai_usage_unpriced_tokens_last_7d{source="llmproxy"}` and has a cost in
`ai_usage_estimated_cost_last_7d_usd{source="llmproxy"}`. Check the proxy's
accounted rates separately in the user/admin usage view after a request.

The exporter recalculates the rolling seven-day estimate, but the mapping change
does not retroactively reprice its lifetime counters or the proxy's existing
usage ledger. Reassess quota limits using the corrected estimate before enabling
enforcement; a previously low estimate may have omitted substantial usage.

## 4. Add an optional global Codex alias

Back up the live config:

~~~bash
sudo cp -a /etc/cliproxyapi/config.yaml \
  /etc/cliproxyapi/config.yaml.before-model-change
~~~

Replace NEW_MODEL with the exact upstream ID and merge this into the existing
oauth-model-alias.codex list:

~~~yaml
oauth-model-alias:
  codex:
    - name: "NEW_MODEL"
      alias: "NEW_MODEL-fast"
      fork: true
~~~

If priority service tier is required and supported by the upstream account,
merge this into the existing payload.override list:

~~~yaml
payload:
  override:
    - models:
        - name: "NEW_MODEL-fast"
          protocol: "codex"
      params:
        service_tier: priority
~~~

This rule is global. It does not grant a tenant access to a model that its
credential cannot use, and it does not choose one tenant's credential. A
codex-service-tier plugin is not required for this configuration rule.

Escalate the restart to the operator, then verify with the temporary user-auth
file from section 3:

~~~bash
sudo systemctl restart cliproxyapi.service
sudo systemctl is-active cliproxyapi.service
curl -fsS "$PROXY_URL/healthz"
curl -fsS --config "$model_auth_file" "$PROXY_URL/v1/models" |
  jq --arg model "$NEW_MODEL" '[.data[].id |
    select(. == $model or . == ($model + "-fast"))]'
~~~

Use a user key for model checks, not the management secret. Model listing does
not prove that every shared OAuth credential can execute the model.

After all checks, remove the temporary user-auth file:

~~~bash
rm -f "$model_auth_file"
trap - EXIT
unset model_auth_file NEW_MODEL PROXY_URL
~~~

## 5. Build and deploy only when section 3 requires it

Rebuild for required implementation changes or to update embedded catalogs
used with `--local-model`. Normal runtime catalog refresh requires neither a
build nor binary installation. A restart alone cannot update embedded data.

Use the staged rollout workflow in [build-and-deploy.md](build-and-deploy.md).
Commit the reviewed revision, build the bundle, and verify it in the isolated
staging service before starting the monitored production rollout:

~~~bash
cd /home/minis/workspace/llmproxy
bash deploy/safe-rollout/build.sh
sudo python3 /usr/local/lib/cliproxy-deploy/rollout.py stage /tmp/cliproxy-bundles/<release>
sudo systemctl start --no-block cliproxyapi-deploy@<release>.service
sudo journalctl -fu cliproxyapi-deploy@<release>.service
~~~

Install the tooling once using the linked guide before these commands. It checks
startup and local API health and automatically restores the previous release on
failure during the observation window. Repeat model discovery, alias, reasoning,
tenancy, and telemetry checks appropriate to the change; the mock staging test
cannot establish real upstream entitlement. Never overwrite the release symlink
with the legacy direct binary `install`/`cp` workflow.

## 6. Rollback

~~~bash
sudo cp -a /etc/cliproxyapi/config.yaml.before-model-change \
  /etc/cliproxyapi/config.yaml
sudo chown nobody:nogroup /etc/cliproxyapi/config.yaml
sudo chmod 600 /etc/cliproxyapi/config.yaml
sudo systemctl restart cliproxyapi.service
~~~

For a binary regression after a monitored rollout, use
`sudo python3 /usr/local/lib/cliproxy-deploy/rollout.py rollback`.
This restores the previous binary/plugin/config snapshot without restoring the DB
or credential files. Review later configuration edits before manual rollback.
If a request returns auth_unavailable, fix credential entitlement or freshness;
never map a new name to an unrelated model merely to pass a test.
Linux-only OpenCode setup is in [USER_GUIDE.md](../USER_GUIDE.md).
