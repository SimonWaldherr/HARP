package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/SimonWaldherr/HARP/harp"
	//harp "./harp"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// A map to store routes. You can use a more sophisticated structure for better performance
var routes map[string][]harp.Route

// A map to store pending responses
var pendingResponses map[string]pendingResponse = make(map[string]pendingResponse)

var mutex sync.RWMutex

// upgrader is used to upgrade the HTTP server connection to a WebSocket connection
var upgrader = websocket.Upgrader{
	ReadBufferSize:  2048,
	WriteBufferSize: 2048,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// HTTP Cache Structure
type cacheEntry struct {
	Expires time.Time
	Content harp.HTTPResponse
}

// Pending Response Structure
type pendingResponse struct {
	Path      string
	Channel   chan *harp.HTTPResponse
	Timestamp time.Time
}

// HTTP Cache
var cache = make(map[string]cacheEntry)

// cleanup request-struct for caching
func cleanupRequest(req harp.HTTPRequest) harp.HTTPRequest {
	h := sha256.New()
	h.Write([]byte(req.Body))

	req.Body = fmt.Sprintf("%x", h.Sum(nil))
	req.Headers = make(map[string]string)
	req.ResponseId = ""

	return req
}

// insertCache inserts a new entry into the cache
func insertCache(req harp.HTTPRequest, res harp.HTTPResponse) {
	// check if request is cacheable
	if res.StatusCode != 200 || res.Headers["Cache-Control"] == "no-cache" {
		return
	}

	// check if request should be cached
	if req.Method != "GET" {
		return
	}

	// check if request is authenticated
	if req.Headers["Authorization"] != "" {
		return
	}

	// only cache Plain-Text like JSON, XML, HTML, CSS, JS, ... in memory
	if !strings.HasPrefix(res.Headers["Content-Type"], "text/") {
		return
	}

	// only cache small responses in memory
	if len(res.Body) > 10000000 {
		return
	}

	//TODO: cache larger responses and binary data on disk

	// cleanup request-struct for caching
	req = cleanupRequest(req)

	// insert into cache
	cache[fmt.Sprintf("%#v", req)] = cacheEntry{
		Expires: time.Now().Add(time.Duration(30) * time.Minute),
		Content: res,
	}
}

// getCache returns the cached response for a given request
func getCache(req harp.HTTPRequest) *harp.HTTPResponse {
	// check if request is cacheable
	if req.Method != "GET" {
		return nil
	}

	// cleanup request-struct for caching
	req = cleanupRequest(req)

	if entry, ok := cache[fmt.Sprintf("%#v", req)]; ok {

		if entry.Expires.After(time.Now()) {
			return &entry.Content
		}
	}
	return nil
}

// cleanupCache removes expired entries from the cache
func cleanupCache() {
	for {
		time.Sleep(1 * time.Minute)
		for req, entry := range cache {
			if entry.Expires.Before(time.Now()) {
				delete(cache, req)
			}
		}
	}
}

// handleWebSocketConnections handles incoming WebSocket connections
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
			res := &harp.HTTPResponse{}
			err = json.Unmarshal(message, res)
			if err != nil {
				log.Println("Error unmarshalling JSON:", err)
				continue
			}

			// Send the response to the correct client
			mutex.RLock()
			if responseChannel, ok := pendingResponses[res.ResponseId]; ok {
				responseChannel.Channel <- res
			}
			mutex.RUnlock()

			continue
		}

		// Handle Registration messages
		reg := &harp.Registration{}
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

func cacheControl(w http.ResponseWriter, r *http.Request) {
	// set cache control headers
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", time.Now().Format(time.RFC1123))
}

// checkCache checks if a request should be cached
func checkCache(r *http.Request) bool {
	if r.Header.Get("Cache-Control") == "no-cache" {
		return false
	}

	if r.Header.Get("Pragma") == "no-cache" {
		return false
	}

	if r.Header.Get("If-Modified-Since") != "" {
		return false
	}

	if r.Header.Get("If-None-Match") != "" {
		return false
	}

	return true
}

// handleHTTPRequest handles incoming HTTP requests
func handleHTTPRequest(w http.ResponseWriter, r *http.Request) {
	// Generate a unique ID for this request
	id := uuid.New()

	// Read the HTTP request body
	buf := new(strings.Builder)
	io.Copy(buf, r.Body)

	// Convert the HTTP request to our HTTPRequest struct
	hr := &harp.HTTPRequest{
		Method:     r.Method,
		URL:        r.URL.String(),
		Headers:    make(map[string]string),
		Body:       buf.String(),
		ResponseId: id.String(),
		Timestamp:  time.Now(),
	}

	// Copy the HTTP headers
	for name, values := range r.Header {
		hr.Headers[name] = values[0]
	}

	// Check if the request is cached
	if checkCache(r) {
		if res := getCache(*hr); res != nil {
			// Send the cached response to the client
			w.WriteHeader(200)
			w.Header().Set("X-Powered-By", "Harp-Cache")
			for name, value := range res.Headers {
				w.Header().Set(name, value)
			}

			// set X-Powered-By header
			w.Header().Set("X-Powered-By", "Harp-Cache")

			// set cache control headers
			//cacheControl(w, r)

			// send response body
			//w.Write([]byte(res.Body))
			fmt.Fprint(w, res.Body)

			return
		}
	}

	// Create a response channel and add it to the pendingResponses map
	// The channel will be used to send the response back to the client
	// when it is received from the application server via WebSocket

	//responseChannel := make(chan *harp.HTTPResponse, 1)

	mutex.Lock()
	pendingResponses[id.String()] = pendingResponse{
		Channel:   make(chan *harp.HTTPResponse, 1),
		Timestamp: time.Now(),
		Path:      r.URL.Path,
	}
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
	case res := <-pendingResponses[id.String()].Channel:
		for name, value := range res.Headers {
			w.Header().Set(name, value)
		}
		w.Header().Set("X-Powered-By", "Harp-Proxy")
		fmt.Fprint(w, res.Body)

		mutex.Lock()

		// Delete the response channel from the pendingResponses map
		delete(pendingResponses, id.String())

		// Set AvgLatency in the route status
		if routes[r.URL.Path][0].Status.AvgLatency == 0 {
			routes[r.URL.Path][0].Status.AvgLatency = res.Latency
		} else {
			routes[r.URL.Path][0].Status.AvgLatency = (routes[r.URL.Path][0].Status.AvgLatency + res.Latency) / 2
		}

		// Set Connected in the route status
		routes[r.URL.Path][0].Status.Connected = true

		mutex.Unlock()

		// Cache the response
		if res.Cacheable {
			insertCache(*hr, *res)
		}
	case <-time.After(time.Second * 30):
		// Timeout after 30 seconds
		http.Error(w, "Timeout", http.StatusGatewayTimeout)
		mutex.Lock()
		delete(pendingResponses, id.String())
		mutex.Unlock()
	}
}

// handleStatusInfoPage handles the /statusinfo page
func handleStatusInfoPage(w http.ResponseWriter, r *http.Request) {
	// Create a table with all routes and their status
	table := "<table><tr><th>App</th><th>Domain</th><th>Path</th><th>Port</th><th>Connected</th><th>Host</th><th>Avg. Latency</th></tr>"
	mutex.RLock()
	for _, routex := range routes {
		for _, route := range routex {
			table += fmt.Sprintf("<tr><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%t</td><td>%s</td><td>%s</td></tr>", route.Name, route.Domain, route.Pattern.String(), route.Port, route.Status.Connected, route.IPorHost, route.Status.AvgLatency)
		}
	}
	table += "</table>"

	// Create list of all routes
	routeList := "<ul>"
	for _, routex := range routes {
		for _, route := range routex {
			routeList += fmt.Sprintf("<li>%s</li>", route.Pattern.String())
		}
	}

	routeList += "</ul>"

	// Create a table with all pending responses
	pending := "<h2>Pending Responses</h2><table><tr><th>Response ID</th><th>Path</th><th>Timestamp</th></tr>"
	for id, response := range pendingResponses {
		pending += fmt.Sprintf("<tr><td>%s</td><td>%#v</td><td>%v</td></tr>", id, response.Path, response.Timestamp)
	}

	// Create the HTML page with the route list, the route status table and the pending responses table
	page := fmt.Sprintf("<html><head><title>Harp Status Info</title></head><body><h1>Harp Status Info</h1><h2>Routes</h2>%s<h2>Route Status</h2>%s%s</table></body></html>", routeList, table, pending)

	mutex.RUnlock()

	// Send the response
	fmt.Fprint(w, page)
}

// init initializes the application
func init() {
	// Initialize the routes and pendingResponses maps
	routes = make(map[string][]harp.Route)
	pendingResponses = make(map[string]pendingResponse)
	//pendingResponses = make(map[string]chan *harp.HTTPResponse)

	// cleanup cache periodically
	go cleanupCache()

	// Check WebSocket Connections in all routes periodically
	// for availability and remove the ones that are not available
	go func() {
		for {
			mutex.Lock()
			for path, route := range routes {
				for i, r := range route {
					err := r.Conn.WriteMessage(websocket.PingMessage, []byte{})
					if err != nil {
						// if route was already disconnected, remove it
						if !r.Status.Connected {
							routes[path] = append(routes[path][:i], routes[path][i+1:]...)
						}

						// set route status to disconnected
						r.Status.Connected = false
					}
				}
			}
			mutex.Unlock()
			time.Sleep(time.Second * 5)

		}
	}()
}

// flag variables
var port string

/*
var app string
var host string
var cacheDir string
var cacheTTL string
var cacheSize string
var cacheCleanupInterval string
*/

func main() {
	// Get Port, App and Host from command line arguments
	flag.StringVar(&port, "port", "8080", "Port to listen on")

	/*
		flag.StringVar(&app, "app", "", "Application to proxy to")
		flag.StringVar(&host, "host", "", "Host to proxy to")
		flag.StringVar(&cacheDir, "cache", "", "Directory to use for caching")
		flag.StringVar(&cacheTTL, "cachettl", "1h", "Cache TTL")
		flag.StringVar(&cacheSize, "cachesize", "100MB", "Cache size")
		flag.StringVar(&cacheCleanupInterval, "cachecleanupinterval", "1h", "Cache cleanup interval")
		flag.StringVar(&cacheCleanupMaxAge, "cachecleanupmaxage", "24h", "Cache cleanup max age")
		flag.StringVar(&cacheCleanupMaxSize, "cachecleanupmaxsize", "100MB", "Cache cleanup max size")
		flag.StringVar(&cacheCleanupMaxCount, "cachecleanupmaxcount", "1000", "Cache cleanup max count")
		flag.StringVar(&cacheCleanupMaxSizePerFile, "cachecleanupmaxsizeperfile", "10MB", "Cache cleanup max size per file")
		flag.StringVar(&cacheCleanupMaxAgePerFile, "cachecleanupmaxageperfile", "1h", "Cache cleanup max age per file")
	*/

	// Parse command line arguments
	flag.Parse()

	// Create a new router
	r := mux.NewRouter()

	// WebSocket endpoint
	r.HandleFunc("/ws", handleWebSocketConnections)

	// Status info page
	r.HandleFunc("/status", handleStatusInfoPage)

	// All other requests
	r.PathPrefix("/").HandlerFunc(handleHTTPRequest)

	srv := &http.Server{
		Handler:      r,
		Addr:         "127.0.0.1:" + port,
		WriteTimeout: 30 * time.Second,
		ReadTimeout:  30 * time.Second,
	}

	fmt.Println("Listening on 127.0.0.1:" + port)
	srv.ListenAndServe()
}
