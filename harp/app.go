package harp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"github.com/gorilla/websocket"
)

type Server struct {
	routes []Route
}

func NewServer() *Server {
	return &Server{}
}

func (s *Server) HandleFunc(path string, handler http.HandlerFunc) {
	route := Route{
		Name:    "App-pages",
		Path:    path,
		Port:    8080,
		Handler: handler,
	}
	s.routes = append(s.routes, route)
}

func (s *Server) ListenAndServe(addr string) error {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		for _, route := range s.routes {
			if r.URL.Path == route.Path {
				route.Handler(w, r)
				break
			}
		}
	})

	// Connect to Harp
	conn, _, err := websocket.DefaultDialer.Dial(addr, nil)
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

		// Parse the message from Harp into a HTTPRequest struct
		req := HTTPRequest{}
		if err := json.Unmarshal(message, &req); err != nil {
			return err
		}

		// Detect the route that the message is for
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

				// Set the response status
				s.routes[i].Status.Online = true
				s.routes[i].Status.LastRequest = req.Timestamp
				s.routes[i].Status.LastResponse = resp.Timestamp

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
	}

	return nil
}
