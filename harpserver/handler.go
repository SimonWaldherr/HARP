// harpserver/handler.go
package harpserver

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	pb "github.com/SimonWaldherr/HARP/harp"
	"google.golang.org/grpc"
)

// RouteConfig binds a route path to a dedicated HTTP handler.
// It is used by BackendServer to register multiple routes, each served
// by a different http.Handler.
type RouteConfig struct {
	Name       string
	Path       string
	Handler    http.Handler
	Streaming  bool
	StreamType string
}

// BackendServer wraps an HTTP handler so that it can register with HARP.
// For multi-route setups populate Routes instead of the single Route/Handler pair.
// Set ReconnectInterval > 0 to enable automatic reconnection on stream failure.
type BackendServer struct {
	Name     string
	Domain   string
	Route    string
	Key      string
	Handler  http.Handler
	ProxyURL string
	// Streaming enables flush-aware response streaming for the single
	// Route/Handler configuration.
	Streaming bool
	// StreamType selects response defaults for streaming responses.
	StreamType string
	// Routes registers multiple path→handler mappings.
	// When set, the single Route/Handler fields are ignored.
	Routes []RouteConfig
	// ReconnectInterval is the initial delay for jittered exponential reconnects.
	// Zero (default) means no automatic reconnection.
	ReconnectInterval time.Duration
}

// ListenAndServeHarp connects to the HARP proxy, registers the backend,
// and listens for forwarded HTTP requests, dispatching them to the wrapped handler.
// If ReconnectInterval > 0, it automatically reconnects on stream failure
// while retaining the underlying gRPC ClientConn.
func (s *BackendServer) ListenAndServeHarp() error {
	return s.ListenAndServeHarpContext(context.Background())
}

// ListenAndServeHarpContext is the context-cancellable variant of
// ListenAndServeHarp. One gRPC ClientConn is retained across stream reconnects.
func (s *BackendServer) ListenAndServeHarpContext(ctx context.Context) error {
	conn, err := newHarpClientConn(s.ProxyURL, s.ReconnectInterval)
	if err != nil {
		return err
	}
	defer conn.Close()
	if s.ReconnectInterval <= 0 {
		_, err = s.serveConnection(ctx, conn)
		return err
	}

	retry := newReconnectBackoff(s.ReconnectInterval, s.Name+"\x00"+s.ProxyURL)
	for {
		connectedFor, serveErr := s.serveConnection(ctx, conn)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if connectedFor >= stableConnectionDuration {
			retry.Reset()
		}
		delay := retry.Next()
		log.Printf("Backend %s disconnected: %v. Reconnecting in %s...", s.Name, serveErr, delay)
		if err := waitForReconnect(ctx, delay); err != nil {
			return err
		}
	}
}

func (s *BackendServer) connect() error {
	conn, err := newHarpClientConn(s.ProxyURL, s.ReconnectInterval)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = s.serveConnection(context.Background(), conn)
	return err
}

func (s *BackendServer) serveConnection(ctx context.Context, conn *grpc.ClientConn) (time.Duration, error) {
	client := pb.NewHarpServiceClient(conn)
	stream, err := client.Proxy(ctx, grpc.WaitForReady(true))
	if err != nil {
		return 0, err
	}

	// Build route list and per-path handler map.
	routeMap := make(map[string]RouteConfig)
	var protoRoutes []*pb.Route
	if len(s.Routes) > 0 {
		for _, r := range s.Routes {
			protoRoutes = append(protoRoutes, &pb.Route{
				Name:   r.Name,
				Path:   r.Path,
				Domain: s.Domain,
			})
			r.StreamType = normalizeStreamType(r.StreamType)
			routeMap[r.Path] = r
		}
	} else {
		protoRoutes = []*pb.Route{{
			Name:   s.Name,
			Path:   s.Route,
			Domain: s.Domain,
		}}
		routeMap[s.Route] = RouteConfig{
			Name:       s.Name,
			Path:       s.Route,
			Handler:    s.Handler,
			Streaming:  s.Streaming,
			StreamType: normalizeStreamType(s.StreamType),
		}
	}

	reg := &pb.Registration{
		Name:   s.Name,
		Domain: s.Domain,
		Key:    s.Key,
		Routes: protoRoutes,
	}
	if err := stream.Send(&pb.ClientMessage{
		Payload: &pb.ClientMessage_Registration{Registration: reg},
	}); err != nil {
		return 0, err
	}
	registeredAt := time.Now()
	log.Printf("Backend %s registered with %d route(s)", s.Name, len(protoRoutes))

	var sendMu sync.Mutex
	wsTunnels := make(map[string]*webSocketTunnel)
	var wsMu sync.RWMutex

	// Listen for forwarded HTTP requests.
	for {
		msg, err := stream.Recv()
		if err != nil {
			return time.Since(registeredAt), err
		}
		if wsData := msg.GetWebsocketData(); wsData != nil {
			wsMu.RLock()
			tunnel, ok := wsTunnels[wsData.RequestId]
			wsMu.RUnlock()
			if ok {
				select {
				case tunnel.ch <- wsData:
				case <-tunnel.done:
				}
			}
			continue
		}
		reqProto := msg.GetHttpRequest()
		if reqProto == nil {
			continue
		}
		log.Printf("Backend %s received request for %s", s.Name, reqProto.Url)
		go s.handleRequest(stream, reqProto, routeMap, &sendMu, wsTunnels, &wsMu)
	}
}

func (s *BackendServer) handleRequest(
	stream pb.HarpService_ProxyClient,
	reqProto *pb.HTTPRequest,
	routeMap map[string]RouteConfig,
	sendMu *sync.Mutex,
	wsTunnels map[string]*webSocketTunnel,
	wsMu *sync.RWMutex,
) {
	// Convert proto HTTPRequest to http.Request.
	req, err := convertProtoToHTTPRequest(reqProto)
	if err != nil {
		log.Printf("Error converting request: %v", err)
		return
	}

	// Dispatch to the best-matching handler (longest prefix wins).
	route := RouteConfig{
		Name:       s.Name,
		Path:       s.Route,
		Handler:    s.Handler,
		Streaming:  s.Streaming,
		StreamType: normalizeStreamType(s.StreamType),
	}
	if len(routeMap) > 0 {
		var bestLen int
		for path, candidate := range routeMap {
			if matchesRoutePrefix(req.URL.Path, path) && len(path) > bestLen {
				route = candidate
				bestLen = len(path)
			}
		}
	}

	if isWebSocketUpgrade(req) {
		s.handleWebSocketRequest(stream, reqProto, req, route, sendMu, wsTunnels, wsMu)
		return
	}
	if route.Streaming {
		s.handleStreamingRequest(stream, reqProto, req, route, sendMu)
		return
	}

	// Create a response recorder.
	recorder := newResponseRecorder()
	if route.Handler != nil {
		route.Handler.ServeHTTP(recorder, req)
	} else {
		recorder.WriteHeader(http.StatusNotFound)
	}

	// Build HTTPResponse proto.
	respProto := &pb.HTTPResponse{
		Status:       int32(recorder.code),
		Headers:      headerToMap(recorder.HeaderMap),
		HeaderValues: pb.HeaderValuesFromHTTP(recorder.HeaderMap),
		Body:         pb.BodyStringForLegacy(recorder.Body.Bytes()),
		BodyBytes:    append([]byte(nil), recorder.Body.Bytes()...),
		RequestId:    reqProto.RequestId,
		Timestamp:    time.Now().UnixNano(),
		Cacheable:    false,
		Latency:      time.Since(time.Unix(0, reqProto.Timestamp)).Nanoseconds(),
	}

	sendMu.Lock()
	defer sendMu.Unlock()
	if err := stream.Send(&pb.ClientMessage{
		Payload: &pb.ClientMessage_HttpResponse{HttpResponse: respProto},
	}); err != nil {
		log.Printf("Error sending response: %v", err)
	}
}

// matchesRoutePrefix reports whether requestPath belongs to a route prefix.
// A route such as "/api" must match "/api" and "/api/users", but not
// unrelated paths such as "/apix". Routes ending in a slash retain their
// natural prefix semantics, and "/" matches every absolute request path.
func matchesRoutePrefix(requestPath, routePath string) bool {
	if routePath == "/" {
		return strings.HasPrefix(requestPath, "/")
	}
	if !strings.HasPrefix(requestPath, routePath) {
		return false
	}
	return strings.HasSuffix(routePath, "/") || len(requestPath) == len(routePath) || requestPath[len(routePath)] == '/'
}

func (s *BackendServer) handleStreamingRequest(
	stream pb.HarpService_ProxyClient,
	reqProto *pb.HTTPRequest,
	req *http.Request,
	route RouteConfig,
	sendMu *sync.Mutex,
) {
	requestStart := time.Unix(0, reqProto.Timestamp)
	send := func(statusCode int, headers map[string]string, body string, end bool) error {
		if headers == nil {
			headers = make(map[string]string)
		}
		headers[pb.StreamHeader] = "1"
		headers[pb.StreamTypeHeader] = normalizeStreamType(route.StreamType)
		if end {
			headers[pb.StreamEndHeader] = "1"
		}
		respProto := &pb.HTTPResponse{
			Status:       int32(statusCode),
			Headers:      headers,
			HeaderValues: pb.HeaderValuesFromHTTP(pb.HTTPHeaderFromProto(headers, nil)),
			Body:         body,
			BodyBytes:    []byte(body),
			RequestId:    reqProto.RequestId,
			Timestamp:    time.Now().UnixNano(),
			Cacheable:    false,
			Latency:      time.Since(requestStart).Nanoseconds(),
		}
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(&pb.ClientMessage{
			Payload: &pb.ClientMessage_HttpResponse{HttpResponse: respProto},
		})
	}
	writer := newStreamingResponseWriter(send)
	if route.Handler != nil {
		route.Handler.ServeHTTP(writer, req)
	} else {
		writer.WriteHeader(http.StatusNotFound)
	}
	if err := writer.Close(); err != nil {
		log.Printf("Error sending streaming response: %v", err)
	}
}

// convertProtoToHTTPRequest converts a pb.HTTPRequest to an *http.Request.
func convertProtoToHTTPRequest(protoReq *pb.HTTPRequest) (*http.Request, error) {
	body := bytes.NewReader(pb.BodyBytesFromProto(protoReq.Body, protoReq.BodyBytes))
	parsedURL, err := url.Parse(protoReq.Url)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(protoReq.Method, protoReq.Url, body)
	if err != nil {
		return nil, err
	}
	req.Header = pb.HTTPHeaderFromProto(protoReq.Headers, protoReq.HeaderValues)
	if host := forwardedHost(req.Header); host != "" {
		req.Host = host
	}
	req.URL = parsedURL
	return req, nil
}

func forwardedHost(headers http.Header) string {
	if host := firstHeaderValue(headers.Get("X-Forwarded-Host")); host != "" {
		return host
	}
	return firstHeaderValue(headers.Get("Host"))
}

func firstHeaderValue(value string) string {
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ResponseRecorder is a minimal implementation of http.ResponseWriter.
type ResponseRecorder struct {
	HeaderMap http.Header
	Body      *bytes.Buffer
	code      int
}

func newResponseRecorder() *ResponseRecorder {
	return &ResponseRecorder{
		HeaderMap: make(http.Header),
		Body:      new(bytes.Buffer),
		code:      http.StatusOK,
	}
}

func (rr *ResponseRecorder) Header() http.Header {
	return rr.HeaderMap
}

func (rr *ResponseRecorder) Write(b []byte) (int, error) {
	return rr.Body.Write(b)
}

func (rr *ResponseRecorder) WriteHeader(statusCode int) {
	rr.code = statusCode
}

func headerToMap(h http.Header) map[string]string {
	return pb.HeaderMapFromHTTP(h)
}
