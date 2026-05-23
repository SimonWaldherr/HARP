# WebSocket Demo

This demo is a minimal direct WebSocket echo server.

It is intentionally not routed through HARP. The current HARP protocol forwards
HTTP requests and HTTP responses, including server-to-client streaming responses
such as SSE. A WebSocket is different: after the HTTP `Upgrade`, both client and
server can send frames independently over the same connection. That needs a
dedicated full-duplex tunnel in the HARP protocol.

## Run

```bash
go run ./demos/websocket-go
```

Open:

```text
http://localhost:8091/
```

The page connects to `ws://localhost:8091/ws`, sends a test message, and prints
the echo response.

## HARP Equivalent Today

For server-to-browser event streams through HARP, use the SSE demo:

```bash
go run ./demos/sse-go -proxy localhost:50054
curl -N http://localhost:8080/events
```

To proxy WebSockets through HARP in the future, the protocol needs a new
full-duplex stream message type for client frames and backend frames after the
initial upgrade.
