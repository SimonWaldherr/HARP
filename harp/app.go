package harp

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/gorilla/websocket"
)

// Server is a Harp server
type Server struct {
	routes []Route
}

// NewServer creates a new server
func NewServer() *Server {
	return &Server{}
}

// HandleFunc registers a handler for the given path
func (s *Server) HandleFunc(path string, handler http.HandlerFunc) {
	route := Route{
		Name:    "App-pages",
		Path:    path,
		Port:    8080,
		Handler: handler,
	}
	s.routes = append(s.routes, route)
}

// isCacheable checks if a request is cacheable
func isCacheable(resp HTTPResponse, req HTTPRequest) bool {
	if resp.Headers["Cache-Control"] == "no-store" {
		return false
	}

	if resp.Headers["Cache-Control"] == "no-cache" {
		return false
	}

	if resp.Headers["Cache-Control"] == "private" {
		return false
	}

	if resp.Headers["Cache-Control"] == "must-revalidate" {
		return false
	}

	if resp.Headers["Cache-Control"] == "proxy-revalidate" {
		return false
	}

	if resp.Headers["Cache-Control"] == "max-age=0" {
		return false
	}

	if resp.Headers["Cache-Control"] == "s-maxage=0" {
		return false
	}

	if req.Method != "GET" {
		return false
	}

	if resp.StatusCode != 200 {
		return false
	}

	return true
}

// handleMessage handles a message from Harp
func (s *Server) handleMessage(conn *websocket.Conn, message []byte) error {
	// Parse the message from Harp into a HTTPRequest struct
	req := HTTPRequest{}
	if err := json.Unmarshal(message, &req); err != nil {
		log.Println("Error parsing message:", err)
		return err
	}

	// Find the route that matches the request
	for i, route := range s.routes {
		if req.URL == route.Path {

			// Create a new HTTPResponse struct
			resp := HTTPResponse{
				StatusCode: 200,
				Body:       "Hello from Harp!",
			}

			// Create a response writer
			w := httptest.NewRecorder()

			// Create a request
			r, err := http.NewRequest(req.Method, req.URL, nil)
			if err != nil {
				return err
			}

			// Call the handler for the route
			s.routes[i].Handler(w, r)

			// Get the response headers from the response writer
			resp.Headers = make(map[string]string)
			for k, v := range w.Header() {
				resp.Headers[k] = v[0]
			}

			// Get the response from the response writer
			resp.Body = w.Body.String()

			// Get the response status code from the response writer
			resp.StatusCode = w.Code

			// Set the response ID
			resp.ResponseId = req.ResponseId

			// Set the response timestamp
			resp.Timestamp = time.Now()

			// Set the response latency
			resp.Latency = resp.Timestamp.Sub(req.Timestamp)

			// Detect cachability and set the Cacheable flag
			if isCacheable(resp, req) {
				resp.Cacheable = true
			}

			// Set the response status
			s.routes[i].Status.Online = true
			s.routes[i].Status.LastRequest = req.Timestamp
			s.routes[i].Status.LastResponse = resp.Timestamp

			// Set the avg latency
			s.routes[i].Status.AvgLatency = (s.routes[i].Status.AvgLatency + resp.Latency) / 2

			// Convert the HTTPResponse struct into a JSON string
			respJSON, err := json.Marshal(resp)
			if err != nil {
				return err
			}
			// Send the JSON string to Harp
			if err := conn.WriteMessage(websocket.TextMessage, respJSON); err != nil {
				return err
			}
			break
		}
	}
	return nil
}

// ListenAndServe listens on the TCP network address addr and then calls Serve
func (s *Server) ListenAndServe(addr string) error {
	// Check if there are any routes
	if len(s.routes) == 0 {
		return fmt.Errorf("No routes registered")
	}

	// Check if the address is valid
	if addr == "" {
		return fmt.Errorf("No address specified")
	}

	// prepare secure websocket connection
	dialer := websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: false}
	dialer.TLSClientConfig.ServerName = "harp"
	dialer.EnableCompression = true

	// Connect to Harp
	conn, _, err := dialer.Dial(addr, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Generate the registration message from the Server struct and send it to Harp
	for _, route := range s.routes {
		reg := Registration{
			Name: route.Name,
			//Domain: route.Domain,
			Domain: ".*",
			Key:    "123",
			Routes: []Route{route},
		}
		if err := conn.WriteJSON(reg); err != nil {
			return err
		}
	}

	// Listen for messages from Harp
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			return err
		}

		// Handle all messages from Harp concurrently
		go s.handleMessage(conn, message)
	}

	return nil
}

// ListenAndServeWithAutoReconnect listens on the TCP network address addr and then calls Serve
func (s *Server) ListenAndServeWithAutoReconnect(addr string) error {
	for {
		if err := s.ListenAndServe(addr); err != nil {
			log.Println("Error:", err)
		}
	}
}
