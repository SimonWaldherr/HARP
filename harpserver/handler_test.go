package harpserver

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	pb "github.com/SimonWaldherr/HARP/harp"
)

func TestResponseRecorder(t *testing.T) {
	rr := newResponseRecorder()

	if rr.code != http.StatusOK {
		t.Errorf("expected default status 200, got %d", rr.code)
	}

	rr.Header().Set("Content-Type", "text/plain")
	if rr.Header().Get("Content-Type") != "text/plain" {
		t.Error("expected Content-Type header to be set")
	}

	rr.WriteHeader(http.StatusCreated)
	if rr.code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", rr.code)
	}

	n, err := rr.Write([]byte("hello world"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 11 {
		t.Errorf("expected 11 bytes written, got %d", n)
	}
	if rr.Body.String() != "hello world" {
		t.Errorf("expected body 'hello world', got %q", rr.Body.String())
	}
}

func TestStreamingResponseWriterFlushesChunks(t *testing.T) {
	type sentChunk struct {
		status  int
		headers map[string]string
		body    string
		end     bool
	}
	var chunks []sentChunk
	writer := newStreamingResponseWriter(func(statusCode int, headers map[string]string, body string, end bool) error {
		chunks = append(chunks, sentChunk{status: statusCode, headers: headers, body: body, end: end})
		return nil
	})

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusAccepted)
	if _, err := writer.Write([]byte("data: one\n\n")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	writer.Flush()
	if _, err := writer.Write([]byte("data: two\n\n")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	if chunks[0].status != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d", http.StatusAccepted, chunks[0].status)
	}
	if chunks[0].headers["Content-Type"] != "text/event-stream" {
		t.Fatalf("expected first chunk headers to include content type")
	}
	if chunks[1].headers != nil {
		t.Fatalf("expected second chunk to omit repeated headers")
	}
	if !chunks[2].end {
		t.Fatal("expected final chunk to be marked as end")
	}
}

func TestResponseRecorderMultipleWrites(t *testing.T) {
	rr := newResponseRecorder()
	rr.Write([]byte("foo"))
	rr.Write([]byte("bar"))
	if rr.Body.String() != "foobar" {
		t.Errorf("expected 'foobar', got %q", rr.Body.String())
	}
}

func TestHeaderToMap(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Custom", "value")

	m := headerToMap(h)

	if m["Content-Type"] != "application/json" {
		t.Errorf("expected 'application/json', got %q", m["Content-Type"])
	}
	if m["X-Custom"] != "value" {
		t.Errorf("expected 'value', got %q", m["X-Custom"])
	}
}

func TestHeaderToMapEmpty(t *testing.T) {
	m := headerToMap(http.Header{})
	if len(m) != 0 {
		t.Errorf("expected empty map, got %d entries", len(m))
	}
}

func TestConvertProtoToHTTPRequest(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		url     string
		body    string
		headers map[string]string
	}{
		{
			name:    "simple GET",
			method:  "GET",
			url:     "http://example.com/test",
			body:    "",
			headers: map[string]string{"Accept": "text/html"},
		},
		{
			name:    "POST with body",
			method:  "POST",
			url:     "http://example.com/api",
			body:    `{"key":"value"}`,
			headers: map[string]string{"Content-Type": "application/json"},
		},
		{
			name:    "URL with query params",
			method:  "GET",
			url:     "http://example.com/search?q=test&page=1",
			body:    "",
			headers: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			protoReq := &pb.HTTPRequest{
				Method:  tc.method,
				Url:     tc.url,
				Body:    tc.body,
				Headers: tc.headers,
			}

			req, err := convertProtoToHTTPRequest(protoReq)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if req.Method != tc.method {
				t.Errorf("expected method %q, got %q", tc.method, req.Method)
			}
			if req.URL.String() != tc.url {
				t.Errorf("expected URL %q, got %q", tc.url, req.URL.String())
			}
			for k, v := range tc.headers {
				if req.Header.Get(k) != v {
					t.Errorf("expected header %s=%q, got %q", k, v, req.Header.Get(k))
				}
			}
		})
	}
}

func TestConvertProtoToHTTPRequestPrefersRepeatedHeadersAndBodyBytes(t *testing.T) {
	protoReq := &pb.HTTPRequest{
		Method: "POST",
		Url:    "http://example.com/api",
		Body:   "legacy",
		Headers: map[string]string{
			"Set-Cookie": "legacy=1",
		},
		HeaderValues: []*pb.HTTPHeader{{
			Name:   "Set-Cookie",
			Values: []string{"a=1", "b=2"},
		}},
		BodyBytes: []byte{0, 1, 2},
	}

	req, err := convertProtoToHTTPRequest(protoReq)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := req.Header.Values("Set-Cookie"); len(got) != 2 || got[0] != "a=1" || got[1] != "b=2" {
		t.Fatalf("expected repeated Set-Cookie headers, got %#v", got)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("unexpected body read error: %v", err)
	}
	if !bytes.Equal(body, []byte{0, 1, 2}) {
		t.Fatalf("expected binary body, got %#v", body)
	}
}

func TestConvertProtoToHTTPRequestInvalidURL(t *testing.T) {
	protoReq := &pb.HTTPRequest{
		Method: "GET",
		Url:    "://invalid",
	}

	_, err := convertProtoToHTTPRequest(protoReq)
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

func TestBackendServerFields(t *testing.T) {
	srv := &BackendServer{
		Name:     "test-backend",
		Domain:   ".*",
		Route:    "/api",
		Key:      "secret",
		ProxyURL: "localhost:50054",
	}

	if srv.Name != "test-backend" {
		t.Errorf("unexpected Name: %s", srv.Name)
	}
	if srv.Domain != ".*" {
		t.Errorf("unexpected Domain: %s", srv.Domain)
	}
}
