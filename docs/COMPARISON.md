# Comparison

HARP is not trying to replace every use of Nginx, HAProxy, or Traefik. Its
primary value is exposing private backends that initiate outbound connections to
a public proxy.

## Where HARP Fits

Use HARP when:

- the backend is behind NAT or a firewall;
- inbound ports on the backend network are impossible or undesirable;
- route registration should be dynamic;
- you want a self-hosted alternative to hosted tunnel services;
- Go `net/http` services should be exposed with little glue code.

Use Nginx, HAProxy, Traefik, or a cloud load balancer when:

- the proxy can directly reach every backend;
- you need mature L7 policy ecosystems;
- you need provider-native autoscaling, WAF, or certificate automation;
- you need extremely battle-tested edge serving for static assets.

## Feature Matrix

| Capability | HARP | Nginx | HAProxy | Traefik | Cloudflare Tunnel/ngrok |
|---|---:|---:|---:|---:|---:|
| Reverse proxy | yes | yes | yes | yes | yes |
| Backend behind NAT without inbound ports | yes | no | no | no | yes |
| Self-hosted control plane | yes | yes | yes | yes | no/partial |
| Dynamic backend registration | yes | config/API dependent | config/API dependent | yes | service dependent |
| WebSocket | yes | yes | yes | yes | yes |
| SSE/live streaming | yes | yes | yes | yes | yes |
| Load balancing | route pool | mature | mature | mature | service dependent |
| Built-in admin UI | yes | no | stats page | dashboard | hosted dashboard |
| Edge TLS automation | manual/external | external/manual | external/manual | built-in ACME | hosted |
| Ecosystem maturity | early | very high | very high | high | high |

## Positioning

One sentence:

> HARP is a self-hosted reverse tunnel proxy for private HTTP backends.

Short pitch:

> Backends connect outward to HARP, register routes, and receive HTTP,
> WebSocket, and streaming requests over a persistent gRPC tunnel. This makes
> private services reachable without opening inbound firewall ports.

## Non-goals

- Replace Nginx as a general static file server.
- Replace HAProxy for very advanced load-balancing policy.
- Replace a cloud provider's managed WAF or global edge network.
- Invent a HARP-specific middleware model when Go `net/http` middleware already
  works.
