package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestJoinURL(t *testing.T) {
	tests := []struct {
		name string
		base string
		path string
		want string
	}{
		{name: "with scheme", base: "http://localhost:8080", path: "/readyz", want: "http://localhost:8080/readyz"},
		{name: "without scheme", base: "localhost:8080", path: "/healthz", want: "http://localhost:8080/healthz"},
		{name: "base path", base: "https://proxy.example.test/harp", path: "/metrics", want: "https://proxy.example.test/harp/metrics"},
		{name: "drops query", base: "http://localhost:8080?x=1", path: "/livez", want: "http://localhost:8080/livez"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := joinURL(tc.base, tc.path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("joinURL(%q, %q) = %q, want %q", tc.base, tc.path, got, tc.want)
			}
		})
	}
}

func TestFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}))
	defer server.Close()

	body, statusCode, err := fetch(context.Background(), server.Client(), server.URL, "/readyz", time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if statusCode != http.StatusOK {
		t.Fatalf("unexpected status %d", statusCode)
	}
	if string(body) != `{"status":"ready"}` {
		t.Fatalf("unexpected body %q", body)
	}
}

func TestRunHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"status":"healthy"}`))
	}))
	defer server.Close()

	var stdout, stderr strings.Builder
	code := run([]string{"health", "-addr", server.URL}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("expected exitOK, got %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"healthy"`) {
		t.Fatalf("expected health output, got %q", stdout.String())
	}
}
