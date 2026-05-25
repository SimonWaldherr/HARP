# Contributing

Thanks for improving HARP. The project is a Go reverse tunnel proxy, so changes
should keep the core behavior simple, observable, and compatible with standard
HTTP expectations.

## Development

```bash
make fmt-all
make vet-all
make test-all
make build
```

For Docker-related changes:

```bash
make docker-compose-config
make docker-build
```

## Guidelines

- Prefer Go standard library patterns, especially `net/http` middleware.
- Keep public protocol changes backward compatible where possible.
- Add focused tests for routing, streaming, WebSocket, caching, and config
  behavior.
- Do not add large dependencies for small helper behavior.
- Keep examples runnable with minimal local setup.

## Pull Requests

Include:

- a short problem statement;
- the implementation summary;
- tests or manual verification;
- any compatibility or security considerations.

## Security-sensitive Changes

Call out changes that affect:

- authentication;
- route registration;
- proxy headers;
- Admin UI/API;
- caching of private responses;
- TLS or transport behavior.
