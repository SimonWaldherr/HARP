# SSE Demo

This demo exposes a Server-Sent Events stream through HARP.

## Run

Start the HARP proxy:

```bash
go run . -config config.json
```

In another terminal, start the SSE backend:

```bash
go run ./demos/sse-go -proxy localhost:50054
```

Then connect through the HARP HTTP proxy:

```bash
curl -N http://localhost:8080/events
```

The route uses `RemoteHelper.RegisterSSE`, so HARP applies SSE response
defaults such as `Content-Type: text/event-stream` and flushes each chunk.
