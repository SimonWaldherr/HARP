# WebSocket Demo

This demo is a minimal WebSocket echo server exposed through HARP.

## Run Through HARP

Start the HARP proxy:

```bash
go run . -config config.json
```

In another terminal, start the WebSocket backend:

```bash
go run ./demos/websocket-go -proxy localhost:50054
```

Open:

```text
http://localhost:8080/
```

The page connects to `ws://localhost:8080/ws`, sends a test message, and prints
the echo response.

## Run Directly

For comparison, the same handler can run without HARP:

```bash
go run ./demos/websocket-go -direct -addr :8091
```

Open:

```text
http://localhost:8091/
```
