# HARP Integration Guide

This guide shows the four ways to connect an existing application or server to
a HARP proxy — from the simplest (zero code) to the most flexible (raw gRPC).

---

## Which approach is right for you?

| Approach | Code required? | Best for |
|---|---|---|
| **A – harp-gateway** | None (JSON only) | Any existing HTTP service (Node, Python, …) |
| **B – BackendServer** | ~5 lines of Go | Go apps that already use `net/http` |
| **C – RemoteHelper** | ~10 lines of Go | Custom Go handlers, home-automation helpers |
| **D – Raw gRPC** | Full implementation | Non-Go languages / maximum control |

---

## Prerequisites

A running HARP proxy with at least one allowed registration rule in its
`config.json`:

```json
"allowedRegistration": [
  { "route": "/.*$", "key": "my-secret-key" }
]
```

The proxy gRPC port defaults to `:50054` and the HTTP port defaults to `:8080`.

---

## A – harp-gateway (no Go code)

The `harp-gateway` binary reads a JSON file and forwards HTTP traffic from the
proxy to any local HTTP service. No code changes to your existing service are
needed.

### 1. Build (or download) the gateway

```bash
# from the HARP repo root
make build-gateway
# binary: ./bin/harp-gateway
```

### 2. Create `gateway.json`

```json
{
  "name": "my-gateway",
  "proxyURL": "proxy.example.com:50054",
  "key": "my-secret-key",
  "domain": ".*",
  "reconnectInterval": "5s",
  "services": [
    {
      "name": "My App",
      "route": "/app/",
      "upstream": "http://localhost:3000",
      "stripPrefix": true
    }
  ]
}
```

| Field | Description |
|---|---|
| `proxyURL` | gRPC address of the HARP proxy |
| `key` | Registration key (must match proxy config) |
| `route` | Public path prefix clients will use |
| `upstream` | Local address of your existing service |
| `stripPrefix` | Remove the route prefix before forwarding |
| `streaming` | Set `true` for chunked/token-stream responses |
| `streamingType` | Optional stream defaults: `chunked`, `sse`, `ndjson`, or `text`; setting it enables streaming |
| `addHeaders` | Headers injected into every upstream request |

### 3. Run the gateway

```bash
./bin/harp-gateway -config gateway.json
```

Traffic arriving at `https://proxy.example.com/app/...` is now forwarded to
`http://localhost:3000/...`.  The gateway reconnects automatically if the
connection drops.

---

## B – BackendServer (wrap a Go `http.Handler`)

If your application is written in Go and already uses `net/http`, you can
replace your `http.ListenAndServe` call with `server.ListenAndServeHarp()`.
HARP forwards regular HTTP, streaming responses, and WebSocket upgrades for
handlers that use `http.Hijacker` such as Gorilla WebSocket.

### Before

```go
http.Handle("/api/", myRouter)
log.Fatal(http.ListenAndServe(":8080", nil))
```

### After

```go
import (
    "log"
    "time"
    "github.com/SimonWaldherr/HARP/harpserver"
)

server := &harpserver.BackendServer{
    Name:              "MyApp",
    ProxyURL:          "proxy.example.com:50054",
    Key:               "my-secret-key",
    Domain:            ".*",
    Route:             "/api/",
    Handler:           myRouter,
    ReconnectInterval: 5 * time.Second,
}
log.Fatal(server.ListenAndServeHarp())
```

Your handler receives normal `*http.Request` objects and writes to a standard
`http.ResponseWriter` — no other changes required.

WebSocket endpoints work through the same wrapper:

```go
router.HandleFunc("/ws", websocketHandler)

server := &harpserver.BackendServer{
    Name:     "RealtimeApp",
    ProxyURL: "proxy.example.com:50054",
    Key:      "my-secret-key",
    Domain:   ".*",
    Route:    "/",
    Handler:  router,
}
log.Fatal(server.ListenAndServeHarp())
```

### Multiple routes

```go
server := &harpserver.BackendServer{
    Name:     "MyApp",
    ProxyURL: "proxy.example.com:50054",
    Key:      "my-secret-key",
    Domain:   ".*",
    Routes: []harpserver.RouteConfig{
        {Name: "API",    Path: "/api/",    Handler: apiRouter},
        {Name: "Static", Path: "/static/", Handler: staticHandler},
    },
    ReconnectInterval: 5 * time.Second,
}
log.Fatal(server.ListenAndServeHarp())
```

---

## C – RemoteHelper (function-based handlers)

`RemoteHelper` is the lightest option: register plain functions instead of an
HTTP server. Ideal for home-automation helpers, IoT bridges, or any service
that doesn't need a full `net/http` stack.

```go
import (
    "fmt"
    "log"
    "net/http"
    "time"
    "github.com/SimonWaldherr/HARP/harpserver"
)

helper := &harpserver.RemoteHelper{
    Name:              "MyHelper",
    ProxyURL:          "proxy.example.com:50054",
    Key:               "my-secret-key",
    Domain:            ".*",
    ReconnectInterval: 5 * time.Second,
}

// Register a simple route.
helper.Register("/helper/ping", "Ping", func(r *http.Request) (int, map[string]string, string) {
    return 200, map[string]string{"Content-Type": "text/plain"}, "pong"
})

// Register a Server-Sent Events route.
helper.RegisterSSE("/helper/events", "Events", func(r *http.Request,
    send func(int, map[string]string, string, bool) error) error {

    for i := 0; i < 5; i++ {
        _ = send(200, nil, fmt.Sprintf("data: chunk %d\n\n", i), false)
        time.Sleep(500 * time.Millisecond)
    }
    return send(200, nil, "", true) // signal end-of-stream
})

log.Fatal(helper.ListenAndServe()) // blocks; auto-reconnects
```

---

## D – Raw gRPC (any language)

Use this approach when integrating from a non-Go language or when you need full
control over the protocol.

### Proto definition

The service is defined in `harp/harp.proto`:

```
service HarpService {
  rpc Proxy(stream ClientMessage) returns (stream ServerMessage);
}
```

Generate client stubs for your language with `protoc` and the appropriate
gRPC plugin, then follow these three steps:

### Step 1 – Open the bidirectional stream

```
channel = grpc.insecure_channel("proxy.example.com:50054")
stub    = HarpServiceStub(channel)
stream  = stub.Proxy(request_iterator())
```

### Step 2 – Send a Registration message

```
ClientMessage {
  registration: Registration {
    name:   "MyBackend",
    domain: ".*",
    key:    "my-secret-key",
    routes: [ Route { name: "MyRoute", path: "/mypath/", domain: ".*" } ]
  }
}
```

### Step 3 – Receive requests, send responses

```
for msg in stream:
    req = msg.http_request
    # process req.method and req.url
    # Prefer req.header_values and req.body_bytes.
    # req.headers and req.body are compatibility fields for older peers.

    stream.send(ClientMessage {
        http_response: HTTPResponse {
            status:     200,
            headers:    { "Content-Type": "text/plain" },
            header_values: [
                HTTPHeader { name: "Content-Type", values: ["text/plain"] }
            ],
            body:       "Hello from my backend",
            body_bytes: b"Hello from my backend",
            request_id: req.request_id,
            timestamp:  time.now_nanos(),
        }
    })
```

Use `header_values` when a header can appear more than once, for example
`Set-Cookie`. Use `body_bytes` for binary payloads or any body that is not
guaranteed to be UTF-8.

The connection is persistent. Reconnect with jittered exponential backoff and
re-send the Registration message whenever the stream drops.

---

## Connecting an existing server via harp-gateway (summary)

If you already have a running server (on any port, any language) and just want
to make it reachable through a public HARP proxy:

1. Install `harp-gateway` on the same machine (or network) as your server.
2. Write a `gateway.json` pointing `upstream` at your server's local address.
3. Run `harp-gateway -config gateway.json`.

That's it. No changes to your existing server are required.

---

## Further reading

- Full proxy configuration reference → [README.md](./README.md#configuration)
- Gateway config fields → [README.md](./README.md#harp-gateway-agent)
- Working demo examples → [demos/](./demos/)
