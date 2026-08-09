# Semantic router sidecar

This Compose file runs the upstream `vllm-project/semantic-router` apiserver on CPU. It publishes only
`127.0.0.1:8080`; no Envoy data-plane port is exposed.

Start it with:

```bash
docker compose up -d
```

CLIProxyAPI calls:

```text
POST http://127.0.0.1:8080/api/v1/classify/intent
```

The named `/models` volume persists Hugging Face model data, including the ModernBERT classifier, across
container replacement and restarts.
