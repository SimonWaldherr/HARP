// demos/headers-go exposes a small inspection endpoint through HARP.
//
// Run a HARP proxy first, then start this demo:
//
//	go run ./demos/headers-go -proxy localhost:50054
//
// Test through the HARP HTTP proxy:
//
//	curl -H 'X-Request-ID: demo-1' http://localhost:8080/inspect/headers
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/SimonWaldherr/HARP/harpserver"
)

var proxyAddr = flag.String("proxy", "localhost:50054", "Address of the HARP proxy gRPC server")

type inspectionResponse struct {
	Method          string              `json:"method"`
	URL             string              `json:"url"`
	Host            string              `json:"host"`
	RequestID       string              `json:"requestId,omitempty"`
	Forwarded       string              `json:"forwarded,omitempty"`
	XForwardedFor   []string            `json:"xForwardedFor,omitempty"`
	XForwardedHost  string              `json:"xForwardedHost,omitempty"`
	XForwardedPort  string              `json:"xForwardedPort,omitempty"`
	XForwardedProto string              `json:"xForwardedProto,omitempty"`
	Via             []string            `json:"via,omitempty"`
	Headers         map[string][]string `json:"headers"`
}

func main() {
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/inspect/headers", headersHandler)
	mux.HandleFunc("/inspect/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	server := &harpserver.BackendServer{
		Name:              "HeadersDemo",
		Domain:            ".*",
		Route:             "/inspect/",
		Key:               "master-key",
		Handler:           mux,
		ProxyURL:          *proxyAddr,
		ReconnectInterval: 5 * time.Second,
	}

	log.Printf("Headers demo connecting to HARP proxy at %s", *proxyAddr)
	log.Println("Registered route: /inspect/")
	log.Println("Test with: curl -H 'X-Request-ID: demo-1' http://localhost:8080/inspect/headers")
	if err := server.ListenAndServeHarp(); err != nil {
		log.Fatalf("headers demo failed: %v", err)
	}
}

func headersHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := inspectionResponse{
		Method:          r.Method,
		URL:             r.URL.String(),
		Host:            r.Host,
		RequestID:       r.Header.Get("X-Request-ID"),
		Forwarded:       r.Header.Get("Forwarded"),
		XForwardedFor:   r.Header.Values("X-Forwarded-For"),
		XForwardedHost:  r.Header.Get("X-Forwarded-Host"),
		XForwardedPort:  r.Header.Get("X-Forwarded-Port"),
		XForwardedProto: r.Header.Get("X-Forwarded-Proto"),
		Via:             r.Header.Values("Via"),
		Headers:         cloneHeader(r.Header),
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("failed to encode inspection response: %v", err)
	}
}

func cloneHeader(headers http.Header) map[string][]string {
	out := make(map[string][]string, len(headers))
	for key, values := range headers {
		out[key] = append([]string(nil), values...)
	}
	return out
}
