# llmproxy metrics in llm-otel

The proxy exports metrics with OTLP/HTTP to a dedicated collector receiver on
TCP port `4328`. The endpoint stays in proxy configuration so changing this
segregation port never requires a code change.

## Collector receiver

Merge the following receiver into the existing `otelcol-contrib`
configuration. Preserve the pipeline's existing receivers, processors, and
exporters when adding `otlp/llmproxy`.

```yaml
receivers:
  otlp/llmproxy:
    protocols:
      http:
        endpoint: 0.0.0.0:4328

service:
  pipelines:
    metrics:
      receivers:
        - <existing-receiver>
        - otlp/llmproxy
      processors:
        - <existing-processors>
      exporters:
        - <existing-exporters>
```

Publish `4328/tcp` from the collector container to the collector host. Keep
`resource_to_telemetry_conversion: enabled` on the Prometheus exporter; the
proxy deliberately limits its process resource labels to `service.name`,
`service.version`, `deployment.environment`, and `host.name`.

## Tailscale grant

Give only the proxy host access to the collector's dedicated receiver. Replace
the example tags with the tags or host aliases used by the tailnet:

```json
{
  "grants": [
    {
      "src": ["tag:llmproxy"],
      "dst": ["tag:llm-otel"],
      "ip": ["tcp:4328"]
    }
  ]
}
```

## Proxy environment

No endpoint is compiled into the proxy. Set all values in the proxy process
environment:

```bash
LLMPROXY_OTEL_ENABLED=true
LLMPROXY_OTEL_ENDPOINT=http://<llm-otel-tailnet-address>:4328
LLMPROXY_OTEL_INTERVAL_SECONDS=15
LLMPROXY_OTEL_ENVIRONMENT=production
```

`LLMPROXY_OTEL_ENABLED` defaults to false.
`LLMPROXY_OTEL_INTERVAL_SECONDS` defaults to `15` when enabled. The documented
endpoint uses the team's dedicated `4328` receiver, but the code has no endpoint
or port default.

## monthly_report.py compatibility

Two reporter changes are required before proxy metrics are used for billing
reports:

1. Add `unclassified` to `TOKEN_TYPE_PRICE_KEYS`. Without it, valid tokens that
   cannot be partitioned by a provider protocol are silently priced at zero.
2. Compute per-model proxy totals from the category series:

   ```promql
   sum by (user_email, model) (llmproxy_token_usage_tokens_total)
   ```

   The proxy intentionally does not emit `token_type="total"` because its v2
   token categories are non-overlapping and already sum to the authoritative
   total.
