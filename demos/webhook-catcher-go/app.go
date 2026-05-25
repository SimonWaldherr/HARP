// demos/webhook-catcher-go is a small ready-to-use webhook sink exposed
// through HARP. It stores the most recent events in memory and serves them as
// JSON for local inspection.
//
// Run a HARP proxy first, then start this demo:
//
//	go run ./demos/webhook-catcher-go -proxy localhost:50054
//
// Send a webhook through HARP:
//
//	curl -X POST http://localhost:8080/hooks/github -d '{"hello":"world"}'
//	curl http://localhost:8080/hooks/events
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SimonWaldherr/HARP/harpserver"
)

const maxBodyBytes = 1 << 20

var (
	proxyAddr = flag.String("proxy", "localhost:50054", "Address of the HARP proxy gRPC server")
	maxEvents = flag.Int("max-events", 50, "Maximum webhook events to keep in memory")
)

type eventStore struct {
	mu     sync.RWMutex
	nextID int64
	limit  int
	events []webhookEvent
}

type webhookEvent struct {
	ID          int64               `json:"id"`
	ReceivedAt  time.Time           `json:"receivedAt"`
	Source      string              `json:"source"`
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	Query       string              `json:"query,omitempty"`
	RequestID   string              `json:"requestId,omitempty"`
	ContentType string              `json:"contentType,omitempty"`
	Headers     map[string][]string `json:"headers"`
	Body        string              `json:"body"`
}

func main() {
	flag.Parse()
	if *maxEvents <= 0 {
		log.Fatal("-max-events must be greater than zero")
	}

	store := &eventStore{limit: *maxEvents}
	mux := http.NewServeMux()
	mux.HandleFunc("/hooks/events", store.eventsHandler)
	mux.HandleFunc("/hooks/", store.captureHandler)

	server := &harpserver.BackendServer{
		Name:              "WebhookCatcher",
		Domain:            ".*",
		Route:             "/hooks/",
		Key:               "master-key",
		Handler:           mux,
		ProxyURL:          *proxyAddr,
		ReconnectInterval: 5 * time.Second,
	}

	log.Printf("Webhook catcher connecting to HARP proxy at %s", *proxyAddr)
	log.Println("Registered route: /hooks/")
	log.Println(`Send: curl -X POST http://localhost:8080/hooks/demo -d '{"hello":"world"}'`)
	log.Println("List: curl http://localhost:8080/hooks/events")
	if err := server.ListenAndServeHarp(); err != nil {
		log.Fatalf("webhook catcher failed: %v", err)
	}
}

func (s *eventStore) captureHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/hooks/events" {
		s.eventsHandler(w, r)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch {
		http.Error(w, "use POST, PUT, or PATCH", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	if len(body) > maxBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}

	event := webhookEvent{
		ReceivedAt:  time.Now().UTC(),
		Source:      strings.TrimPrefix(r.URL.Path, "/hooks/"),
		Method:      r.Method,
		Path:        r.URL.Path,
		Query:       r.URL.RawQuery,
		RequestID:   r.Header.Get("X-Request-ID"),
		ContentType: r.Header.Get("Content-Type"),
		Headers:     cloneHeader(r.Header),
		Body:        string(body),
	}
	event = s.add(event)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(event); err != nil {
		log.Printf("failed to encode webhook event: %v", err)
	}
}

func (s *eventStore) eventsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "use GET", http.StatusMethodNotAllowed)
		return
	}
	events := s.list()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"count":  len(events),
		"events": events,
	}); err != nil {
		log.Printf("failed to encode webhook events: %v", err)
	}
}

func (s *eventStore) add(event webhookEvent) webhookEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	event.ID = s.nextID
	s.events = append(s.events, event)
	if len(s.events) > s.limit {
		copy(s.events, s.events[len(s.events)-s.limit:])
		s.events = s.events[:s.limit]
	}
	return event
}

func (s *eventStore) list() []webhookEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	events := append([]webhookEvent(nil), s.events...)
	sort.Slice(events, func(i, j int) bool {
		return events[i].ID > events[j].ID
	})
	return events
}

func cloneHeader(headers http.Header) map[string][]string {
	out := make(map[string][]string, len(headers))
	for key, values := range headers {
		out[key] = append([]string(nil), values...)
	}
	return out
}

func (e webhookEvent) String() string {
	return fmt.Sprintf("#%d %s %s", e.ID, e.Method, e.Path)
}
