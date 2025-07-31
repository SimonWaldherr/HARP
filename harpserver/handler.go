// harpserver/handler.go
package harpserver

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/url"
	"time"

	pb "github.com/SimonWaldherr/HARP/harp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// BackendServer wraps an HTTP handler so that it can register with HARP.
type BackendServer struct {
	Name     string
	Domain   string
	Route    string
	Key      string
	Handler  http.Handler
	ProxyURL string
}

// ListenAndServeHarp connects to the HARP proxy, registers the backend,
// and listens for forwarded HTTP requests, dispatching them to the wrapped handler.
func (s *BackendServer) ListenAndServeHarp() error {
	// Create connection with proper keepalive settings
	opts := []grpc.DialOption{
		grpc.WithInsecure(),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,  // Ping interval
			Timeout:             5 * time.Second,   // Ping timeout
			PermitWithoutStream: true,              // Allow pings without streams
		}),
	}
	
	conn, err := grpc.Dial(s.ProxyURL, opts...)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pb.NewHarpServiceClient(conn)
	ctx := context.Background()
	stream, err := client.Proxy(ctx)
	if err != nil {
		return err
	}

	// Send registration request.
	reg := &pb.Registration{
		Name:   s.Name,
		Domain: s.Domain,
		Key:    s.Key,
		Routes: []*pb.Route{
			{
				Name:   s.Name,
				Path:   s.Route,
				Domain: s.Domain,
			},
		},
	}
	if err := stream.Send(&pb.ClientMessage{
		Payload: &pb.ClientMessage_Registration{Registration: reg},
	}); err != nil {
		return err
	}
	log.Printf("Backend %s registered with route %s", s.Name, s.Route)

	// Listen for forwarded HTTP requests.
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		reqProto := msg.GetHttpRequest()
		if reqProto == nil {
			continue
		}
		log.Printf("Backend %s received request for %s", s.Name, reqProto.Url)

		// Convert proto HTTPRequest to http.Request.
		req, err := convertProtoToHTTPRequest(reqProto)
		if err != nil {
			log.Printf("Error converting request: %v", err)
			continue
		}

		// Create a response recorder.
		recorder := newResponseRecorder()
		s.Handler.ServeHTTP(recorder, req)

		// Build HTTPResponse proto.
		respProto := &pb.HTTPResponse{
			Status:    int32(recorder.code),
			Headers:   headerToMap(recorder.HeaderMap),
			Body:      recorder.Body.String(),
			RequestId: reqProto.RequestId,
			Timestamp: time.Now().UnixNano(),
			Cacheable: false,
			Latency:   time.Since(time.Unix(0, reqProto.Timestamp)).Nanoseconds(),
		}

		if err := stream.Send(&pb.ClientMessage{
			Payload: &pb.ClientMessage_HttpResponse{HttpResponse: respProto},
		}); err != nil {
			log.Printf("Error sending response: %v", err)
		}
	}
}

// convertProtoToHTTPRequest converts a pb.HTTPRequest to an *http.Request.
func convertProtoToHTTPRequest(protoReq *pb.HTTPRequest) (*http.Request, error) {
	body := bytes.NewBufferString(protoReq.Body)
	parsedURL, err := url.Parse(protoReq.Url)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(protoReq.Method, protoReq.Url, body)
	if err != nil {
		return nil, err
	}
	// Populate headers.
	for k, v := range protoReq.Headers {
		req.Header.Set(k, v)
	}
	// You can set other fields as needed.
	req.URL = parsedURL
	return req, nil
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

// headerToMap converts http.Header to a simple map[string]string.
func headerToMap(h http.Header) map[string]string {
	m := make(map[string]string)
	for k, v := range h {
		if len(v) > 0 {
			m[k] = v[0]
		}
	}
	return m
}
