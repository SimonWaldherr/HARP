package harpserver

import (
	"net"
	"net/http"
	"testing"
)

func TestIsWebSocketUpgrade(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "keep-alive, Upgrade")
	req.Header.Set("Upgrade", "websocket")

	if !isWebSocketUpgrade(req) {
		t.Fatal("expected request to be detected as WebSocket upgrade")
	}
}

func TestWebSocketResponseWriterHijack(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	writer := newWebSocketResponseWriter(server)
	conn, rw, err := writer.Hijack()
	if err != nil {
		t.Fatalf("Hijack failed: %v", err)
	}
	if conn == nil || rw == nil {
		t.Fatal("expected hijacked connection and buffered read-writer")
	}
	if !writer.Hijacked() {
		t.Fatal("expected writer to be marked hijacked")
	}

	done := make(chan error, 1)
	go func() {
		_, err := client.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n\r\n"))
		done <- err
	}()

	buf := make([]byte, len("HTTP/1.1 101 Switching Protocols\r\n\r\n"))
	if _, err := rw.Read(buf); err != nil {
		t.Fatalf("reading from hijacked connection failed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("writing to pipe failed: %v", err)
	}
}
