# Project Pitch

## Tagline

Self-hosted reverse tunnel proxy for private HTTP backends.

## Short Description

HARP lets private services publish HTTP, WebSocket, SSE, and streaming routes
through a public proxy without opening inbound firewall ports. Backends connect
outward over gRPC, register the routes they serve, and receive proxied requests
through that persistent connection.

## Why It Exists

Traditional reverse proxies require the proxy to reach every backend. That is
often impossible for home servers, IoT networks, developer laptops, and services
inside restricted networks. HARP flips the connection direction while keeping
standard HTTP semantics at the public edge.

## Primary Use Cases

- Expose a Raspberry Pi or home server safely through a self-hosted public VPS.
- Publish Home Assistant, Pi-hole, Grafana, or local dashboards without router
  port forwarding.
- Share a local development service with testers.
- Expose local LLM gateways such as Ollama or LM Studio.
- Receive webhooks on a private machine.

## What To Say Carefully

Avoid saying "HARP replaces Nginx." A better framing is:

> HARP complements Nginx or Traefik when the backend cannot accept inbound
> connections.

## Demo Script

```bash
docker compose up --build
curl http://localhost:8080/inspect/headers
curl -X POST http://localhost:8080/hooks/demo -d '{"hello":"world"}'
curl http://localhost:8080/hooks/events
```

Talking points:

- No inbound backend port was opened.
- The backend registered itself.
- Standard proxy headers are preserved.
- Health checks and `harpctl` make it automatable.
