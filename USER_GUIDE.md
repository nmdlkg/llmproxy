# User Guide

For people using the shared internal proxy. You do **not** need the admin
management key for anything in this guide.

The proxy exposes OpenAI-, Claude-, Gemini- and Codex-compatible APIs backed by a
**shared pool** of provider accounts. You register your own provider account
once; it joins the pool, and in return you get quota proportional to what you
contributed. Everyone draws from the whole pool, so a single account being rate
limited does not block you.

Assumed reachable over the tailnet at `https://<proxy-host>:8317` (8317 is the
default port). Substitute your actual host throughout.

---

## 1. Get your API key

Ask an admin to create your account. They will hand you a key that looks like
`cp_u_...`.

**The key is shown exactly once.** It is stored only as a SHA-256 hash, so
nobody — including admins — can recover it later. If you lose it, ask for a new
one, or issue one yourself from the dashboard once you can log in.

Treat it like a password: it is your identity, your quota, and your credentials.

---

## 2. Point your tool at the proxy

Use your key as a bearer token. All of these work:

```
Authorization: Bearer cp_u_...
X-Api-Key: cp_u_...
X-Goog-Api-Key: cp_u_...
?key=cp_u_...
```

Common setups:

```bash
# OpenAI-compatible clients
export OPENAI_BASE_URL=https://<proxy-host>:8317/v1
export OPENAI_API_KEY=cp_u_...

# Claude-compatible clients
export ANTHROPIC_BASE_URL=https://<proxy-host>:8317
export ANTHROPIC_API_KEY=cp_u_...
```

Endpoints available: `/v1/chat/completions`, `/v1/messages`, `/v1/responses`,
`/v1beta/...` (Gemini), `/backend-api/codex/...`, `/v1/models`.

### If your tool also reports its own telemetry

Codex CLI, Claude Code and OpenCode can export usage metrics themselves. When
you route them through this proxy, **both** the tool and the proxy observe the
same request, which double counts on dashboards. If you point a tool at the
proxy, also set:

```bash
export OTEL_RESOURCE_ATTRIBUTES=llm_route=proxy
```

Dashboards filter tool metrics on that label so the proxy's numbers are used
instead. Set it in the same place you set the base URL — the two belong
together.

---

## 3. Register your provider account

This is the part that earns you quota. Your credential is stored **on the
server**, never on your laptop.

### Recommended: log in from your own machine

Run the login locally — the browser callback only works on the machine running
the browser — and the credential is uploaded to the server:

```bash
./cli-proxy-api --claude-login \
  --remote https://<proxy-host>:8317 \
  --user-key cp_u_...
```

Swap the provider flag as needed:

| Provider | Flag |
|---|---|
| Claude | `--claude-login` |
| Codex (browser) | `--codex-login` |
| Codex (device code) | `--codex-device-login` |
| Antigravity | `--antigravity-login` |
| Kimi | `--kimi-login` |
| xAI | `--xai-login` |

Useful extras: `--no-browser` prints the URL instead of opening it, and
`--oauth-callback-port <n>` changes the local callback port if the default is
taken.

Notes:
- `--remote` requires HTTPS unless the target is loopback.
- The key and the credential body are never logged.

### Alternative: the dashboard

Open `https://<proxy-host>:8317/user`, paste your API key, and start the OAuth
flow there. The provider will redirect to a `localhost` URL that fails to load —
that is expected. Copy the **full** URL from the address bar and paste it back
into the dashboard. This exists because the server cannot receive a callback
aimed at your laptop.

---

## 4. The dashboard

`https://<proxy-host>:8317/user` — it prompts for your API key and keeps
everything scoped to you:

- register, list and delete your credentials
- toggle **shared** on a credential
- see your quota and usage
- issue and revoke your own API keys

You can only ever see and touch your own credentials. Another user's
credentials are invisible to you, and the reverse is also true.

### Sharing

A credential you register starts **not shared**. Turning `shared` on puts it
into the common pool for everyone to draw from, and that is what grants you the
matching quota. Turning it off withdraws it.

---

## 5. Quota and usage

Quota is a **weekly rolling window**, measured in USD-equivalent cost:

```
your limit = base allowance for your tier
           + a contribution per shared account you registered,
             weighted by provider and plan (e.g. a Codex Pro seat is
             worth more than a free one)
```

The cost figure is derived from published per-token prices. It is a **relative
weight for how expensive a request was, denominated in USD — not a bill.** These
are subscription accounts, so no one is charged per token.

Check it with the dashboard, or:

```bash
curl -H "Authorization: Bearer cp_u_..." https://<proxy-host>:8317/v0/user/usage
```

Returns `limit`, `used`, `remaining`, `window` and the reset time.

> **Enforcement is currently off.** Usage is recorded but no request is refused
> yet, while real numbers are collected to set sensible limits. When it is
> turned on, exceeding your quota returns `429` with `Retry-After`, or — if the
> operator enables it — quietly downgrades you to a cheaper model instead of
> failing.

### Keeping your credential healthy

Nothing currently probes your provider token on a schedule, so an expired token
is discovered the next time the pool happens to pick it. If a provider login of
yours stops working, re-run the login for that provider (section 3). Checking
`/user` after a provider-side password or session change is worth the ten
seconds.

---

## 6. The `auto` model

Request `auto` as the model name and the proxy picks one for you based on how
hard your prompt looks:

```json
{ "model": "auto", "messages": [ ... ] }
```

Hard prompts get a stronger model, easy ones a cheaper one. Usage records keep
`auto` as the requested name and record the model that actually ran, so you can
see both.

If classification is unavailable, `auto` falls back to its previous meaning (the
newest available model) — it never errors just because routing is unavailable.

Thinking suffixes still work: `auto(high)`.

---

## 7. Troubleshooting

| Symptom | Cause |
|---|---|
| `401` / auth error | Key wrong, revoked, or your account is disabled. |
| `429` with `Retry-After` | Quota exceeded (once enforcement is on), or the whole pool is rate limited. Retry after the given delay. |
| `404` on a credential | It is not yours, or it does not exist. These are deliberately indistinguishable. |
| `/user` will not load | Tenancy may be disabled server-side, or you are outside the tailnet. |
| Login says the callback failed | Expected for the dashboard flow — copy the full failed URL back into the dashboard. Or use the CLI flow instead. |
| Your credential shows an auth failure | Your provider token expired. Re-run the login for that provider. |
| `unknown provider for model X` | That model is not served by any credential in the pool right now. |

---

## Appendix: admin

Admins hold a key with the `admin` role and get one extra subtree. It is
role-based, not the management key.

```bash
# create a user; response includes a one-time plaintext key to hand over
curl -X POST -H "Authorization: Bearer <admin-key>" \
  -H 'Content-Type: application/json' \
  -d '{"email":"person@example.com","display_name":"Person","role":"user","tier":"default"}' \
  https://<proxy-host>:8317/v0/user/admin/users

curl -H "Authorization: Bearer <admin-key>" https://<proxy-host>:8317/v0/user/admin/users
# PATCH /v0/user/admin/users/:id    change tier, role, display name, disabled
# DELETE /v0/user/admin/users/:id   (an admin cannot delete itself)
```

There is no email delivery: creating a user returns the key once and handing it
over is manual.

Operator-side configuration (`tenancy`, `auto-routing`, `openrouter`, `otel`)
lives in `config.yaml`; see `config.example.yaml` for the documented blocks.
