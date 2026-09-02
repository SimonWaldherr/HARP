# Production Guide

HARP is useful when backends cannot be reached directly by a public proxy.
Backends connect outward over gRPC and register the routes they serve.

## Baseline Deployment

Run HARP behind an established edge proxy or load balancer when possible:

```text
Internet -> TLS edge (Nginx/Traefik/ELB) -> HARP HTTP port
Private backend -> outbound gRPC -> HARP gRPC port
```

Recommended defaults:

- Keep `enableAdminUI` disabled publicly unless protected by Basic Auth and `adminAllowedCIDRs`.
- Bind metrics to loopback, for example `127.0.0.1:9091`.
- Use long random route keys in `allowedRegistration`.
- Prefer specific route regexes over catch-all rules.
- Keep `maxRequestBodySize`, `maxHeaderSize`, rate limits, and timeouts enabled.
- Use `loadBalancingStrategy: "round_robin"` when multiple agents serve the same route.
- Use an external TLS edge for certificate automation unless you explicitly need HARP HTTPS/HTTP3.

## Admin UI

Admin is off by default. If enabled:

```json
{
  "enableAdminUI": true,
  "adminPath": "/admin",
  "adminUsername": "admin",
  "adminPassword": "replace-with-long-random-password",
  "adminInsecureSkipAuth": false,
  "adminAllowedCIDRs": ["127.0.0.1/32", "::1/128"]
}
```

`adminInsecureSkipAuth` is intended only for isolated local testing. Do not use
it on a public interface.

## Caching

HARP caches non-streaming `GET` responses when `enableCache` is true and
`cacheType` is not `none`. Streaming responses such as SSE and token streams are
not cached. Use conservative TTLs for authenticated or frequently changing
routes.

## Load Balancing

When multiple backends register the same domain/path route, HARP creates one
route pool.

Strategies:

- `round_robin`: spread requests across available backend connections.
- `least_connections`: prefer the healthy backend with the fewest inflight
  requests and long-lived connections.
- `first`: always use the first registered backend. Useful for deterministic
  debugging or active/passive operational models.

For rolling upgrades from pre-2.0 HARP releases, temporarily set
`compatibilityMode` to `v1`; see the migration notes in the README. This mode
does not weaken Admin authentication or registration keys.

Set `connectionPoolSize` to cap registered backend streams per route. Failed
streams are removed from selection immediately; idempotent requests can fail
over once to another healthy stream. Size the memory cache independently with
`cacheMaxItems`.

## systemd

Install binaries and config:

```bash
sudo install -d -o harp -g harp /etc/harp /var/lib/harp
sudo install -m 0755 bin/harp-proxy /usr/local/bin/harp-proxy
sudo install -m 0640 deploy/configs/proxy.production.example.json /etc/harp/config.json
sudo install -m 0644 deploy/systemd/harp-proxy.service /etc/systemd/system/harp-proxy.service
sudo systemctl daemon-reload
sudo systemctl enable --now harp-proxy
```

Create the `harp` user before installing:

```bash
sudo useradd --system --home /var/lib/harp --shell /usr/sbin/nologin harp
```

## Operational Checks

```bash
harpctl ready -addr http://127.0.0.1:8080
harpctl metrics -addr http://127.0.0.1:9091
journalctl -u harp-proxy -f
```

## Security Checklist

- [ ] Replace every example key and password.
- [ ] Restrict Admin with `adminAllowedCIDRs`.
- [ ] Keep metrics private or behind authentication.
- [ ] Put TLS at the edge or configure HARP TLS explicitly.
- [ ] Use route-specific registration rules.
- [ ] Review cache behavior for private data.
- [ ] Monitor `backend_errors`, `active_connections`, and `rate_limited`.
- [ ] Run upgrades through a staging proxy first.
