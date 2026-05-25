# HARP Quickstart

This guide gets a local HARP proxy and demo backend running in a few minutes.

## Option 1: Docker Compose

```bash
docker compose up --build
```

Then test the bundled demos through the proxy:

```bash
curl http://localhost:8080/inspect/headers
curl -X POST http://localhost:8080/hooks/demo -d '{"hello":"world"}'
curl http://localhost:8080/hooks/events
```

The Compose stack starts:

- `harp-proxy` on `localhost:8080` and gRPC on `localhost:50054`
- `headers-demo` behind HARP at `/inspect/`
- `webhook-catcher` behind HARP at `/hooks/`

The default Compose config uses `change-me` as the backend registration key.
Change it before running outside a local machine.

## Option 2: Go Binaries

Start the proxy:

```bash
make run
```

In another terminal, start a demo backend:

```bash
make run-demo-headers
```

Test it:

```bash
curl -H 'X-Request-ID: quickstart-1' http://localhost:8080/inspect/headers
```

## Health Checks

Use `harpctl` instead of shell-specific curl snippets:

```bash
make build-tools
./bin/harpctl wait -addr http://localhost:8080 -timeout 30s
./bin/harpctl health -addr http://localhost:8080
./bin/harpctl metrics -addr http://localhost:9091
```

## Next Steps

- Read [Production](PRODUCTION.md) before exposing HARP publicly.
- Read [Comparison](COMPARISON.md) to understand where HARP fits relative to Nginx, Traefik, HAProxy, Cloudflare Tunnel, and ngrok.
- Use `cmd/harp-gateway/gateway-example.json` for no-code local service publishing.
