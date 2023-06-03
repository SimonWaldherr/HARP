package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// HTTPRequest is the request sent from the Proxy to the App
type HTTPRequest struct {
	Method     string            `json:"method"`
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	ResponseId string            `json:"responseId"`
}

// Route is a route that the App registers with the Proxy
type Route struct {
	Name     string          `json:"name"`
	Path     string          `json:"path"`
	RegExp   string          `json:"regexp"`
	Port     int             `json:"port"`
	Domain   string          `json:"domain"`
	IPorHost string          `json:"ipOrHost"`
	Status   Status          `json:"-"` // We store the status of the route here
	Pattern  *regexp.Regexp  `json:"-"` // We store the compiled regular expression here
	Conn     *websocket.Conn `json:"-"` // We store the WebSocket connection here
}

// Registration is the message sent from the App to the Proxy to register routes
type Registration struct {
	Name   string  `json:"name"`
	Domain string  `json:"domain"`
	Key    string  `json:"key"`
	Routes []Route `json:"routes"`
}

// Status is the status of a route
type Status struct {
	Online      bool      `json:"online"`
	LastRequest time.Time `json:"lastRequest"`
}

// HTTPResponse is the response sent from the App to the Proxy
type HTTPResponse struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	ResponseId string            `json:"responseId"`
}

// A map to store routes. You can use a more sophisticated structure for better performance
var routes map[string][]Route

// A map to store pending responses
var pendingResponses map[string]chan *HTTPResponse = make(map[string]chan *HTTPResponse)

var mutex sync.RWMutex

var upgrader = websocket.Upgrader{
	ReadBufferSize:  2048,
	WriteBufferSize: 2048,
}

func handleWebSocketConnections(w http.ResponseWriter, r *http.Request) {
	// Upgrade initial GET request to a websocket
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Println(err)
	}

	defer ws.Close()

	for {
		// Read the incoming route registration message
		_, message, err := ws.ReadMessage()

		if err != nil {
			log.Println("read:", err)
			break
		}

		// Determine the type of the message (HTTPResponse or Registration)
		m := make(map[string]interface{})
		err = json.Unmarshal(message, &m)
		if err != nil {
			log.Println("Error unmarshalling JSON:", err)
			continue
		}

		// Handle HTTPResponse messages
		if _, ok := m["status"]; ok {
			res := &HTTPResponse{}
			err = json.Unmarshal(message, res)
			if err != nil {
				log.Println("Error unmarshalling JSON:", err)
				continue
			}

			// Send the response to the correct client
			mutex.RLock()
			if responseChannel, ok := pendingResponses[res.ResponseId]; ok {
				responseChannel <- res
			}
			mutex.RUnlock()

			continue
		}

		// Handle Registration messages
		reg := &Registration{}
		err = json.Unmarshal(message, reg)
		if err != nil {
			log.Println("Error unmarshalling JSON:", err)
			continue
		}

		// Register the routes and store the WebSocket connection
		for _, route := range reg.Routes {
			pattern, err := regexp.Compile(reg.Domain + route.Path)
			if err != nil {
				log.Println("Error compiling regexp:", err)
				continue
			}
			route.Conn = ws
			route.Name = reg.Name
			route.Domain = reg.Domain
			route.Pattern = pattern
			route.Port = reg.Routes[0].Port
			route.IPorHost = r.Host
			mutex.Lock()
			routes[route.Path] = append(routes[route.Path], route)
			mutex.Unlock()
		}
	}
}

func handleHTTPRequest(w http.ResponseWriter, r *http.Request) {
	id := uuid.New()

	buf := new(strings.Builder)
	io.Copy(buf, r.Body)

	// Convert the HTTP request to our HTTPRequest struct
	hr := &HTTPRequest{
		Method:     r.Method,
		URL:        r.URL.String(),
		Headers:    make(map[string]string),
		Body:       buf.String(),
		ResponseId: id.String(),
	}

	for name, values := range r.Header {
		hr.Headers[name] = values[0]
	}

	// Create a response channel and add it to the pendingResponses map
	responseChannel := make(chan *HTTPResponse, 1)
	mutex.Lock()
	pendingResponses[id.String()] = responseChannel
	mutex.Unlock()

	// Find the correct route and forward the request to the application
	mutex.RLock()

	var ok bool = false
	for _, routex := range routes {
		for _, route := range routex {
			if route.Domain == ".*" || route.Domain == r.Host {
				if route.Pattern.MatchString(r.URL.Path) {
					route.Conn.WriteJSON(hr)
					ok = true
					break
				}
			}
		}
	}

	//conns, ok := routes[r.URL.Path]
	mutex.RUnlock()

	if !ok {
		// No matching route, return a 404
		log.Printf("handleHTTPRequest: No matching route for %s\n", r.URL.String())
		http.NotFound(w, r)
		return
	}

	// Wait for a response and write it back to the client
	select {
	case res := <-responseChannel:
		for name, value := range res.Headers {
			w.Header().Set(name, value)
		}
		w.WriteHeader(res.Status)
		fmt.Fprint(w, res.Body)
		mutex.Lock()
		delete(pendingResponses, id.String())
		mutex.Unlock()
	case <-time.After(time.Second * 5):
		// Timeout after 5 seconds
		http.Error(w, "Timeout", http.StatusGatewayTimeout)
		mutex.Lock()
		delete(pendingResponses, id.String())
		mutex.Unlock()
	}
}

func handleStatusInfoPage(w http.ResponseWriter, r *http.Request) {
	// Create a table with all routes and their status
	table := "<table><tr><th>App</th><th>Domain</th><th>Path</th><th>Port</th><th>Connected</th><th>Host</th></tr>"
	mutex.RLock()
	for _, routex := range routes {
		for _, route := range routex {
			table += fmt.Sprintf("<tr><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%t</td><td>%s</td></tr>", route.Name, route.Domain, route.Pattern.String(), route.Port, true, route.IPorHost)
		}
	}
	mutex.RUnlock()
	table += "</table>"
	fmt.Fprint(w, table)
}

func init() {
	routes = make(map[string][]Route)
	pendingResponses = make(map[string]chan *HTTPResponse)

	// Check WebSocket Connections in all routes periodically
	// for availability and remove the ones that are not available
	go func() {
		for {
			mutex.Lock()
			for path, route := range routes {
				for i, r := range route {
					err := r.Conn.WriteMessage(websocket.PingMessage, []byte{})
					if err != nil {
						routes[path] = append(routes[path][:i], routes[path][i+1:]...)
					}
				}
			}
			mutex.Unlock()
			time.Sleep(time.Second * 5)

		}
	}()
}

func main() {
	r := mux.NewRouter()

	// WebSocket endpoint
	r.HandleFunc("/ws", handleWebSocketConnections)

	// Status info page
	r.HandleFunc("/status", handleStatusInfoPage)

	// All other requests
	r.PathPrefix("/").HandlerFunc(handleHTTPRequest)

	srv := &http.Server{
		Handler:      r,
		Addr:         "127.0.0.1:8080",
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}

	fmt.Println("Listening on 127.0.0.1:8080")
	srv.ListenAndServe()
}
