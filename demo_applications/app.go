package main

import (
	"fmt"
	"log"
	"time"
	
	. "../harp"

	"github.com/gorilla/websocket"
)

// WebSocket connection to the Proxy server
var ws *websocket.Conn

func main() {
	// Connect to the Proxy server
	var err error
	ws, _, err = websocket.DefaultDialer.Dial("ws://localhost:8080/ws", nil)
	if err != nil {
		log.Println("Error connecting to WebSocket server:", err)
		return
	}
	defer ws.Close()

	// Register routes
	reg := &Registration{
		Name:   "App",
		Domain: ".*",
		Key:    "123456",
		Routes: []Route{
			{
				Name:   "App-pages",
				Path:   "/pages/123",
				RegExp: "^/pages/123$",
				Port:   80,
				Handler: func(req *HTTPRequest) {
					res := &HTTPResponse{
						Status:     200,
						Headers:    map[string]string{"Content-Type": "text/plain"},
						Body:       fmt.Sprintf("Hello from App.go!\n%s - %s\n%s\n", req.Method, req.URL, time.Now().Format(time.RFC3339)),
						ResponseId: req.ResponseId,
					}

					err := ws.WriteJSON(res)
					if err != nil {
						log.Println("Error sending response:", err)
					}
				},
			},
			{
				Name:   "All-robots",
				Path:   "/robots.txt",
				RegExp: "^/robots.txt$",
				Port:   80,
				Handler: func(req *HTTPRequest) {
					res := &HTTPResponse{
						Status:     200,
						Headers:    map[string]string{"Content-Type": "text/plain"},
						Body:       fmt.Sprint("User-agent: *\nDisallow: /"),
						ResponseId: req.ResponseId,
					}

					err := ws.WriteJSON(res)
					if err != nil {
						log.Println("Error sending response:", err)
					}
				},
			},
		},
	}
	err = ws.WriteJSON(reg)
	if err != nil {
		log.Println("Error sending registration:", err)
		return
	}

	// Wait for HTTP requests
	for {
		req := &HTTPRequest{}

		err = ws.ReadJSON(req)
		if err != nil {
			log.Println("Error reading JSON:", err)
			break
		}

		// Handle the HTTP request
		switch req.URL {
		case "/favicon.ico":
			// Ignore favicon requests
			continue
		case "/":
			// Send back a simple response
			res := &HTTPResponse{
				Status:     200,
				Headers:    map[string]string{"Content-Type": "text/plain"},
				Body:       "Hello from App.go!\n" + req.ResponseId + "\n",
				ResponseId: req.ResponseId,
			}

			err := ws.WriteJSON(res)
			if err != nil {
				log.Println("Error sending response:", err)
			}
			continue
		}

		for _, route := range reg.Routes {
			if route.Handler != nil && route.Path == req.URL {
				route.Handler(req)
				continue
			}
		}
	}
}
