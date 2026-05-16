package harpserver

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	pb "github.com/SimonWaldherr/HARP/harp"
	"google.golang.org/grpc/metadata"
)

// stubProxyClient is a minimal mock of pb.HarpService_ProxyClient used in tests.
type stubProxyClient struct {
	sent *pb.ClientMessage
}

func (s *stubProxyClient) Send(m *pb.ClientMessage) error   { s.sent = m; return nil }
func (s *stubProxyClient) Recv() (*pb.ServerMessage, error) { return nil, nil }
func (s *stubProxyClient) CloseSend() error                 { return nil }
func (s *stubProxyClient) Context() context.Context         { return context.Background() }
func (s *stubProxyClient) SendMsg(m interface{}) error      { return nil }
func (s *stubProxyClient) RecvMsg(m interface{}) error      { return nil }
func (s *stubProxyClient) Header() (metadata.MD, error)     { return nil, nil }
func (s *stubProxyClient) Trailer() metadata.MD             { return nil }

// TestHelperHandlerFuncSignature verifies that HelperHandlerFunc can be used
// with the expected signature.
func TestHelperHandlerFuncSignature(t *testing.T) {
	var fn HelperHandlerFunc = func(r *http.Request) (int, map[string]string, string) {
		return http.StatusOK, map[string]string{"Content-Type": "text/plain"}, "hello"
	}

	req, _ := http.NewRequest("GET", "/test", nil)
	status, headers, body := fn(req)

	if status != http.StatusOK {
		t.Errorf("expected status 200, got %d", status)
	}
	if headers["Content-Type"] != "text/plain" {
		t.Errorf("unexpected Content-Type: %s", headers["Content-Type"])
	}
	if body != "hello" {
		t.Errorf("expected body 'hello', got '%s'", body)
	}
}

// TestRemoteHelperRegister verifies that routes are appended via Register.
func TestRemoteHelperRegister(t *testing.T) {
	h := &RemoteHelper{Name: "test", ProxyURL: "localhost:50054", Key: "key", Domain: ".*"}

	if len(h.Routes) != 0 {
		t.Fatalf("expected 0 routes initially, got %d", len(h.Routes))
	}

	h.Register("/foo", "Foo", func(r *http.Request) (int, map[string]string, string) {
		return 200, nil, "foo"
	})
	h.Register("/bar", "Bar", func(r *http.Request) (int, map[string]string, string) {
		return 200, nil, "bar"
	})

	if len(h.Routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(h.Routes))
	}
	if h.Routes[0].Path != "/foo" {
		t.Errorf("expected first path '/foo', got '%s'", h.Routes[0].Path)
	}
	if h.Routes[1].Name != "Bar" {
		t.Errorf("expected second name 'Bar', got '%s'", h.Routes[1].Name)
	}
}

// TestRemoteHelperHandleRequest exercises route dispatch in handleRequest.
func TestRemoteHelperHandleRequest(t *testing.T) {
	h := &RemoteHelper{Name: "dispatch-test"}

	routeMap := map[string]HelperRoute{
		"/api": {
			Handler: func(r *http.Request) (int, map[string]string, string) {
				return 200, map[string]string{"X-From": "api"}, "api-response"
			},
		},
		"/other": {
			Handler: func(r *http.Request) (int, map[string]string, string) {
				return 201, nil, "other"
			},
		},
	}

	tests := []struct {
		path       string
		wantStatus int32
		wantBody   string
	}{
		{"/api/users", 200, "api-response"},
		{"/other", 201, "other"},
		{"/unknown", 404, "no handler registered for /unknown"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			reqProto := &pb.HTTPRequest{
				Method:    "GET",
				Url:       fmt.Sprintf("http://localhost%s", tc.path),
				RequestId: "req" + tc.path,
				Timestamp: time.Now().UnixNano(),
			}

			stub := &stubProxyClient{}
			h.handleRequest(stub, reqProto, routeMap, nil)

			if stub.sent == nil {
				t.Fatal("expected a response to be sent")
			}
			resp := stub.sent.GetHttpResponse()
			if resp == nil {
				t.Fatal("expected HTTPResponse payload in sent message")
			}
			if resp.Status != tc.wantStatus {
				t.Errorf("path %s: expected status %d, got %d", tc.path, tc.wantStatus, resp.Status)
			}
			if resp.Body != tc.wantBody {
				t.Errorf("path %s: expected body %q, got %q", tc.path, tc.wantBody, resp.Body)
			}
			if resp.RequestId != reqProto.RequestId {
				t.Errorf("path %s: RequestId mismatch: got %s, want %s", tc.path, resp.RequestId, reqProto.RequestId)
			}
		})
	}
}

func TestRemoteHelperRegisterStream(t *testing.T) {
	h := &RemoteHelper{Name: "stream-test"}

	h.RegisterStream("/stream", "Stream", func(
		r *http.Request,
		send func(statusCode int, headers map[string]string, body string, end bool) error,
	) error {
		return send(http.StatusOK, map[string]string{"Content-Type": "text/plain"}, "chunk", true)
	})

	if len(h.Routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(h.Routes))
	}
	if h.Routes[0].StreamHandler == nil {
		t.Fatal("expected StreamHandler to be set")
	}
}

// TestBackendServerRouteConfig checks that RouteConfig fields are stored correctly.
func TestBackendServerRouteConfig(t *testing.T) {
	h1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) })

	srv := &BackendServer{
		Name:     "multi-route",
		ProxyURL: "localhost:50054",
		Key:      "key",
		Domain:   ".*",
		Routes: []RouteConfig{
			{Name: "Route1", Path: "/one", Handler: h1},
			{Name: "Route2", Path: "/two", Handler: h2},
		},
		ReconnectInterval: 2 * time.Second,
	}

	if len(srv.Routes) != 2 {
		t.Fatalf("expected 2 RouteConfigs, got %d", len(srv.Routes))
	}
	if srv.Routes[0].Path != "/one" {
		t.Errorf("unexpected first path: %s", srv.Routes[0].Path)
	}
	if srv.ReconnectInterval != 2*time.Second {
		t.Errorf("unexpected ReconnectInterval: %v", srv.ReconnectInterval)
	}
}
