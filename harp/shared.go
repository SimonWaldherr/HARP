package harp

import (
	"net/http"
	"regexp"
	"time"

	"github.com/gorilla/websocket"
)

// HTTPRequest is a HTTP request from Harp to the app enclosed in a WebSocket message
type HTTPRequest struct {
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	HeadersHash string            `json:"headersHash"`
	Body        string            `json:"body"`
	ResponseId  string            `json:"responseId"`
	Timestamp   time.Time         `json:"timestamp"`
}

// HTTPResponse is a HTTP response from the app to Harp enclosed in a WebSocket message
type HTTPResponse struct {
	Status     int               `json:"status"`
	StatusCode int               `json:"statusCode"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	ResponseId string            `json:"responseId"`
	Timestamp  time.Time         `json:"timestamp"`
	Cacheable  bool              `json:"cacheable"`
	Latency    time.Duration     `json:"latency"`
}

// Route is a route that Harp will proxy to the app
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
	//Handler func(*HTTPRequest) `json:"-"` // We store the handler here
	//Handler HandlerFunc `json:"-"`
	Handler http.HandlerFunc `json:"-"`
}

// Registration is a registration message from an app to Harp enclosed in a WebSocket message
type Registration struct {
	Name   string  `json:"name"`
	Domain string  `json:"domain"`
	Key    string  `json:"key"`
	Routes []Route `json:"routes"`
}

// Status is the status of a route
type Status struct {
	Online       bool      `json:"online"`
	LastRequest  time.Time `json:"lastRequest"`
	LastResponse time.Time `json:"lastResponse"`
}
