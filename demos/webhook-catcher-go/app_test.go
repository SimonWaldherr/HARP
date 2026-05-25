package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEventStoreKeepsNewestEvents(t *testing.T) {
	store := &eventStore{limit: 2}
	store.add(webhookEvent{Path: "/hooks/one"})
	store.add(webhookEvent{Path: "/hooks/two"})
	store.add(webhookEvent{Path: "/hooks/three"})

	events := store.list()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Path != "/hooks/three" || events[1].Path != "/hooks/two" {
		t.Fatalf("events should be newest first, got %#v", events)
	}
}

func TestCaptureHandlerStoresWebhook(t *testing.T) {
	store := &eventStore{limit: 10}
	req := httptest.NewRequest(http.MethodPost, "/hooks/github?delivery=1", strings.NewReader(`{"ok":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "req-1")

	rec := httptest.NewRecorder()
	store.captureHandler(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d", rec.Code)
	}
	var created webhookEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}
	if created.Source != "github" {
		t.Fatalf("unexpected source %q", created.Source)
	}
	if created.Body != `{"ok":true}` {
		t.Fatalf("unexpected body %q", created.Body)
	}

	events := store.list()
	if len(events) != 1 || events[0].RequestID != "req-1" {
		t.Fatalf("unexpected stored events: %#v", events)
	}
}

func TestCaptureHandlerRejectsLargeBodies(t *testing.T) {
	store := &eventStore{limit: 10}
	req := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(strings.Repeat("x", maxBodyBytes+1)))
	rec := httptest.NewRecorder()

	store.captureHandler(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status 413, got %d", rec.Code)
	}
}
