// demos/static-wrapper-go/app.go
package main

import (
	"log"
	"net/http"

	"github.com/SimonWaldherr/HARP/harpserver"
)

func main() {
	// Define the directory to serve static files from.
	staticDir := "./static"

	// Create a file server.
	fileServer := http.FileServer(http.Dir(staticDir))

	// Use http.StripPrefix if needed.
	handler := http.StripPrefix("/static/", fileServer)

	// Wrap the handler using the HARP backend helper.
	server := &harpserver.BackendServer{
		Name:     "StaticFileBackend",
		Domain:   ".*",
		Route:    "/static/",
		Key:      "master-key",
		Handler:  handler,
		ProxyURL: "localhost:50054",
	}

	log.Println("Starting StaticFileBackend using HARP wrapper...")
	if err := server.ListenAndServeHarp(); err != nil {
		log.Fatalf("Error: %v", err)
	}
}
