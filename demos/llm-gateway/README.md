# LLM Gateway Demo (Public HARP Proxy + Home Network)

This demo shows how to expose local LLM endpoints (Ollama, LM Studio, llmster)
running on your home network through a public HARP proxy, including live token
streaming for NDJSON and Server-Sent Events responses.

The flow is:

1. **Public server/VPS:** run HARP proxy (public HTTP + gRPC).
2. **Home network machine:** run `harp-gateway` with the config from this demo.
3. Clients call your public URL; HARP forwards requests over gRPC to your home gateway.

No inbound ports are required on your home router.

## 1) Prepare proxy config on your public host

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
- set `proxyURL` to your public HARP gRPC address
- set `key` to the registration key configured on the proxy
- adjust upstream ports/hosts if your local services differ
- leave `timeoutSeconds` at `0` for live generation streams that may run longer
  than a fixed request timeout

## 3) Test through your public proxy

Assuming your public proxy HTTP URL is `https://proxy.example.com`:

### Ollama
```bash
curl https://proxy.example.com/llm/ollama/api/tags
```

Live Ollama generation stream (NDJSON):

```bash
curl -N https://proxy.example.com/llm/ollama/api/generate \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "llama3.2",
    "prompt": "Write a haiku about reverse proxies.",
    "stream": true
  }'
```

### LM Studio (OpenAI-compatible)
```bash
curl https://proxy.example.com/llm/lmstudio/v1/models
```

Live LM Studio chat completion stream (SSE):

```bash
curl -N https://proxy.example.com/llm/lmstudio/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "local-model",
    "messages": [
      { "role": "user", "content": "Stream three short facts about Go." }
    ],
    "stream": true
  }'
```

### llmster
```bash
curl https://proxy.example.com/llm/llmster/v1/models
```

Live llmster OpenAI-compatible stream (SSE, adjust model as needed):

```bash
curl -N https://proxy.example.com/llm/llmster/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "local-model",
    "messages": [
      { "role": "user", "content": "Stream a one-sentence answer." }
    ],
    "stream": true
  }'
```

## Notes

- This demo uses path prefixes and strips them before forwarding:
  - `/llm/ollama/...`   -> `http://127.0.0.1:11434/...`
  - `/llm/lmstudio/...` -> `http://127.0.0.1:1234/...`
  - `/llm/llmster/...`  -> `http://127.0.0.1:8000/...` (change as needed)
- Live streaming is enabled in the example config:
  - Ollama uses `"streamingType": "ndjson"` for newline-delimited JSON.
  - LM Studio and llmster use `"streamingType": "sse"` for OpenAI-compatible
    Server-Sent Events.
  - `curl -N` disables curl's output buffering so tokens appear as they arrive.
- For higher throughput, tune:
  - `upstreamMaxIdleConns`
  - `upstreamMaxIdleConnsPerHost`
  - `upstreamMaxConnsPerHost`
- If your local endpoint requires auth, add headers in the service `addHeaders` map.
- Prefer HTTPS on your public host and strong registration keys.
