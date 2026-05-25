package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHeadersHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/inspect/headers?x=1", nil)
	req.Host = "public.example.test"
	req.Header.Set("X-Request-ID", "req-1")
	req.Header.Set("Forwarded", `for="203.0.113.7";proto="https";host="public.example.test"`)
	req.Header.Set("X-Forwarded-Host", "public.example.test")
	req.Header.Set("X-Forwarded-Port", "443")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Add("Via", "1.1 edge")
	req.Header.Add("Via", "1.1 harp")

	rec := httptest.NewRecorder()
	headersHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	var body inspectionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}
	if body.Host != "public.example.test" {
		t.Fatalf("unexpected host %q", body.Host)
	}
	if body.RequestID != "req-1" {
		t.Fatalf("unexpected request id %q", body.RequestID)
	}
	if len(body.Via) != 2 {
		t.Fatalf("expected Via chain, got %#v", body.Via)
	}
}
