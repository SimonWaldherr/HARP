# LLM Gateway Demo (Hetzner + Home Network)

This demo shows how to expose local LLM endpoints (Ollama, LM Studio, llmster)
running on your home network through a public HARP proxy on your Hetzner server.

The flow is:

1. **Hetzner server:** run HARP proxy (public HTTP + gRPC).
2. **Home network machine:** run `harp-gateway` with the config from this demo.
3. Clients call your Hetzner URL; HARP forwards requests over gRPC to your home gateway.

No inbound ports are required on your home router.

## 1) Prepare proxy config on Hetzner

In your proxy `config.json`, allow the LLM routes and key used by the gateway:

```json
"allowedRegistration": [
  { "route": "/llm/.*$", "key": "master-key" }
]
```

## 2) Build and run gateway on your home machine

From repo root:

```bash
make build-gateway
./bin/harp-gateway -config demos/llm-gateway/gateway-llm-example.json
```

Edit `gateway-llm-example.json` first:
- set `proxyURL` to your Hetzner HARP gRPC address
- set `key` to the registration key configured on the proxy
- adjust upstream ports/hosts if your local services differ

## 3) Test through your Hetzner proxy

Assuming your public proxy HTTP URL is `https://proxy.example.com`:

### Ollama
```bash
curl https://proxy.example.com/llm/ollama/api/tags
```

### LM Studio (OpenAI-compatible)
```bash
curl https://proxy.example.com/llm/lmstudio/v1/models
```

### llmster
```bash
curl https://proxy.example.com/llm/llmster/v1/models
```

## Notes

- This demo uses path prefixes and strips them before forwarding:
  - `/llm/ollama/...`   -> `http://127.0.0.1:11434/...`
  - `/llm/lmstudio/...` -> `http://127.0.0.1:1234/...`
  - `/llm/llmster/...`  -> `http://127.0.0.1:8000/...` (change as needed)
- If your local endpoint requires auth, add headers in the service `addHeaders` map.
- Prefer HTTPS on Hetzner and strong registration keys.
