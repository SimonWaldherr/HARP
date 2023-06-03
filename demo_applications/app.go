package main

import (
	"fmt"
	"log"
	"time"

	harp "../harp"
)

func main() {
	server := harp.NewServer()

	server.HandleFunc("/pages/123", func(req *harp.HTTPRequest, respond func(*harp.HTTPResponse) error) {
		res := &harp.HTTPResponse{
			Status:     200,
			Headers:    map[string]string{"Content-Type": "text/plain"},
			Body:       fmt.Sprintf("Hello from App.go!\n%s - %s\n%s\n", req.Method, req.URL, time.Now().Format(time.RFC3339)),
			ResponseId: req.ResponseId,
		}

		if err := respond(res); err != nil {
			log.Println("Error sending response:", err)
		}
	})

	server.HandleFunc("/robots.txt", func(req *harp.HTTPRequest, respond func(*harp.HTTPResponse) error) {
		res := &harp.HTTPResponse{
			Status:     200,
			Headers:    map[string]string{"Content-Type": "text/plain"},
			Body:       fmt.Sprint("User-agent: *\nDisallow: /"),
			ResponseId: req.ResponseId,
		}

		if err := respond(res); err != nil {
			log.Println("Error sending response:", err)
		}
	})

	if err := server.ListenAndServe("ws://localhost:8080/ws"); err != nil {
		log.Fatal(err)
	}
}
