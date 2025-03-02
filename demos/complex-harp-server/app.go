// demos/complex-harp-server/app.go
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/SimonWaldherr/HARP/harpserver"
	"github.com/gorilla/mux"
)

func main() {
	router := mux.NewRouter()

	// Endpoint for service status.
	router.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		status := map[string]interface{}{
			"status":  "ok",
			"time":    time.Now().Format(time.RFC3339),
			"version": "1.0.0",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	}).Methods("GET")

	// Endpoint for echoing posted JSON data.
	router.HandleFunc("/api/echo", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payload)
	}).Methods("POST")

	server := &harpserver.BackendServer{
		Name:     "ComplexBackend",
		Domain:   ".*",
		Route:    "/api/",
		Key:      "master-key",
		Handler:  router,
		ProxyURL: "localhost:50051",
	}

	log.Println("Starting ComplexBackend using HARP wrapper...")
	if err := server.ListenAndServeHarp(); err != nil {
		log.Fatalf("Error: %v", err)
	}
}
