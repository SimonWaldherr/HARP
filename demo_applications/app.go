package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	harp "../harp"
)

func main() {
	server := harp.NewServer()

	server.HandleFunc("/pages/123", func(w http.ResponseWriter, r *http.Request) {
		body := fmt.Sprintf("Hello from App.go!\n%s - %s\n%s\n", r.Method, r.URL, time.Now().Format(time.RFC3339))
		w.Header().Set("Content-Type", "text/plain")
		_, err := w.Write([]byte(body))
		if err != nil {
			log.Println("Error writing response:", err)
		}
	})

	server.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		body := "User-agent: *\nDisallow: /"
		w.Header().Set("Content-Type", "text/plain")
		_, err := w.Write([]byte(body))
		if err != nil {
			log.Println("Error writing response:", err)
		}
	})

	if err := server.ListenAndServe("ws://localhost:8080/ws"); err != nil {
		log.Fatal(err)
	}
}
