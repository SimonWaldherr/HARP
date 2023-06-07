package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/SimonWaldherr/HARP/harp"
)

// Some very complex business logic
func complexBusinessLogic() {
	time.Sleep(5 * time.Second)
}

func main() {
	// Create a new server
	server := harp.NewServer()
	// Register a handler for the /test route
	server.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
		body := fmt.Sprintf("Hello from the \"/test\"-page !\n%s - %s\n%s\n", r.Method, r.URL, time.Now().Format(time.RFC3339))
		w.Header().Set("Content-Type", "text/plain")

		complexBusinessLogic()

		_, err := w.Write([]byte(body))
		if err != nil {
			log.Println("Error writing response:", err)
		}
	})

	// Register a handler for the /pages/123 route
	server.HandleFunc("/pages/123", func(w http.ResponseWriter, r *http.Request) {
		body := fmt.Sprintf("Hello from App.go!\n%s - %s\n%s\n", r.Method, r.URL, time.Now().Format(time.RFC3339))
		w.Header().Set("Content-Type", "text/plain")
		_, err := w.Write([]byte(body))
		if err != nil {
			log.Println("Error writing response:", err)
		}
	})

	// Register a handler for the /robots.txt route
	server.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		body := "User-agent: *\nDisallow: /"
		w.Header().Set("Content-Type", "text/plain")
		_, err := w.Write([]byte(body))
		if err != nil {
			log.Println("Error writing response:", err)
		}
	})

	// Start the server
	if err := server.ListenAndServe("ws://localhost:8080/ws"); err != nil {
		log.Fatal(err)
	}
}
