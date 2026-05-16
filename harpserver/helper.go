// harpserver/helper.go
// RemoteHelper enables lightweight, function-based backend helpers to register
// with a HARP proxy and handle requests locally. It is designed for helpers
// running behind NAT or on home networks (e.g. a MacBook running Home Assistant
// integrations) that expose local resources to a publicly hosted HARP server.
package harpserver

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	pb "github.com/SimonWaldherr/HARP/harp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// HelperHandlerFunc processes an incoming HTTP request and returns a status
// code, response headers, and response body string. Use it to implement
// lightweight route handlers without standing up a full HTTP server.
type HelperHandlerFunc func(r *http.Request) (statusCode int, headers map[string]string, body string)

// HelperStreamHandlerFunc processes an incoming request and can send multiple
// response chunks for long-lived/streaming upstreams (e.g. SSE or token streams).
type HelperStreamHandlerFunc func(
	r *http.Request,
	send func(statusCode int, headers map[string]string, body string, end bool) error,
) error

// HelperRoute binds a URL path prefix to a HelperHandlerFunc.
type HelperRoute struct {
	// Name is a human-readable identifier for this route.
	Name string
	// Path is the URL path prefix that will be registered with the HARP proxy.
	Path string
	// Handler is called for every request whose URL path matches this route.
	Handler HelperHandlerFunc
	// StreamHandler is called for streaming routes and may emit multiple chunks.
	StreamHandler HelperStreamHandlerFunc
}

// RemoteHelper connects to a HARP proxy and dispatches incoming HTTP requests
// to registered handler functions. It is optimised for "remote helper" use
// cases where a small agent on a private network (e.g. a home MacBook) needs
// to expose local services (Home Assistant, local scripts, IoT devices, …) to
// a publicly reachable HARP instance.
//
// Auto-reconnect is built in: if the gRPC stream drops the helper will wait
// ReconnectInterval and then re-register all routes transparently.
type RemoteHelper struct {
	// Name identifies this helper on the HARP proxy.
	Name string
	// ProxyURL is the address of the HARP proxy gRPC server, e.g. "example.com:50054".
	ProxyURL string
	// Key is the authentication key used during registration.
	Key string
	// Domain is the regex matched against the request's domain (e.g. ".*").
	Domain string
	// Routes contains the registered path→handler mappings.
	Routes []HelperRoute
	// ReconnectInterval is the delay between reconnect attempts (default: 5s).
	ReconnectInterval time.Duration
}

// Register adds a route handler to the RemoteHelper.
// path is the URL path prefix, name is a descriptive label, and fn handles
// each incoming request.
func (h *RemoteHelper) Register(path, name string, fn HelperHandlerFunc) {
	h.Routes = append(h.Routes, HelperRoute{Name: name, Path: path, Handler: fn})
}

// RegisterStream adds a streaming route handler that can emit multiple response
// chunks for a single request.
func (h *RemoteHelper) RegisterStream(path, name string, fn HelperStreamHandlerFunc) {
	h.Routes = append(h.Routes, HelperRoute{Name: name, Path: path, StreamHandler: fn})
}

// ListenAndServe connects to the HARP proxy, registers all routes, and
// processes incoming requests. It blocks indefinitely and automatically
// reconnects on stream failure.
func (h *RemoteHelper) ListenAndServe() error {
	interval := h.ReconnectInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		if err := h.connectOnce(); err != nil {
			log.Printf("RemoteHelper %s disconnected: %v. Reconnecting in %s...", h.Name, err, interval)
		}
		time.Sleep(interval)
	}
}

// connectOnce performs a single connection attempt, registers all routes and
// serves requests until the stream is closed or an error occurs.
func (h *RemoteHelper) connectOnce() error {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	}

	conn, err := grpc.NewClient(h.ProxyURL, opts...)
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := pb.NewHarpServiceClient(conn).Proxy(context.Background())
	if err != nil {
		return err
	}

	// Build route list for registration.
	protoRoutes := make([]*pb.Route, 0, len(h.Routes))
	for _, r := range h.Routes {
		protoRoutes = append(protoRoutes, &pb.Route{
			Name:   r.Name,
			Path:   r.Path,
			Domain: h.Domain,
		})
	}

	reg := &pb.Registration{
		Name:   h.Name,
		Domain: h.Domain,
		Key:    h.Key,
		Routes: protoRoutes,
	}
	if err := stream.Send(&pb.ClientMessage{
		Payload: &pb.ClientMessage_Registration{Registration: reg},
	}); err != nil {
		return err
	}
	log.Printf("RemoteHelper %s registered with %d route(s)", h.Name, len(protoRoutes))

	// Build a fast path→handler lookup map.
	routeMap := make(map[string]HelperRoute, len(h.Routes))
	for _, r := range h.Routes {
		routeMap[r.Path] = r
	}
	var sendMu sync.Mutex

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		reqProto := msg.GetHttpRequest()
		if reqProto == nil {
			continue
		}
		// Handle each request in its own goroutine to avoid head-of-line blocking.
		go h.handleRequest(stream, reqProto, routeMap, &sendMu)
	}
}

// handleRequest dispatches a single incoming request to the best-matching
// handler and sends the response back over the gRPC stream.
func (h *RemoteHelper) handleRequest(
	stream pb.HarpService_ProxyClient,
	reqProto *pb.HTTPRequest,
	routeMap map[string]HelperRoute,
	sendMu *sync.Mutex,
) {
	req, err := convertProtoToHTTPRequest(reqProto)
	if err != nil {
		log.Printf("RemoteHelper %s: error converting request: %v", h.Name, err)
		return
	}

	// Find the longest matching path prefix.
	var matched HelperRoute
	var hasRoute bool
	var bestLen int
	for path, route := range routeMap {
		if strings.HasPrefix(req.URL.Path, path) && len(path) > bestLen {
			matched = route
			hasRoute = true
			bestLen = len(path)
		}
	}

	if hasRoute && matched.StreamHandler != nil {
		err := matched.StreamHandler(req, func(statusCode int, headers map[string]string, body string, end bool) error {
			if headers == nil {
				headers = make(map[string]string)
			}
			headers[pb.StreamHeader] = "1"
			if end {
				headers[pb.StreamEndHeader] = "1"
			}
			return h.sendResponse(stream, reqProto, statusCode, headers, body, sendMu)
		})
		if err != nil {
			log.Printf("RemoteHelper %s: stream handler error: %v", h.Name, err)
			_ = h.sendResponse(stream, reqProto, http.StatusBadGateway, map[string]string{
				"Content-Type":     "text/plain",
				pb.StreamHeader:    "1",
				pb.StreamEndHeader: "1",
			}, "stream handler error: "+err.Error(), sendMu)
		}
		return
	}

	var statusCode int
	var headers map[string]string
	var body string
	if hasRoute && matched.Handler != nil {
		statusCode, headers, body = matched.Handler(req)
	} else {
		statusCode = http.StatusNotFound
		headers = map[string]string{"Content-Type": "text/plain"}
		body = "no handler registered for " + req.URL.Path
	}
	returnHeaders := headers
	if returnHeaders == nil {
		returnHeaders = make(map[string]string)
	}
	_ = h.sendResponse(stream, reqProto, statusCode, returnHeaders, body, sendMu)
}

func (h *RemoteHelper) sendResponse(
	stream pb.HarpService_ProxyClient,
	reqProto *pb.HTTPRequest,
	statusCode int,
	headers map[string]string,
	body string,
	sendMu *sync.Mutex,
) error {
	respProto := &pb.HTTPResponse{
		Status:    int32(statusCode),
		Headers:   headers,
		Body:      body,
		RequestId: reqProto.RequestId,
		Timestamp: time.Now().UnixNano(),
		Latency:   time.Since(time.Unix(0, reqProto.Timestamp)).Nanoseconds(),
	}

	if sendMu != nil {
		sendMu.Lock()
		defer sendMu.Unlock()
	}
	if err := stream.Send(&pb.ClientMessage{
		Payload: &pb.ClientMessage_HttpResponse{HttpResponse: respProto},
	}); err != nil {
		log.Printf("RemoteHelper %s: error sending response: %v", h.Name, err)
		return err
	}
	return nil
}
