package harpserver

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	pb "github.com/SimonWaldherr/HARP/harp"
)

const websocketTunnelBufferSize = 64

type webSocketTunnel struct {
	ch   chan *pb.WebSocketData
	done chan struct{}
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		headerTokenContains(r.Header.Get("Connection"), "upgrade")
}

func headerTokenContains(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

type websocketResponseWriter struct {
	header     http.Header
	body       bytes.Buffer
	code       int
	conn       net.Conn
	rw         *bufio.ReadWriter
	hijacked   bool
	headerSent bool
}

func newWebSocketResponseWriter(conn net.Conn) *websocketResponseWriter {
	return &websocketResponseWriter{
		header: make(http.Header),
		code:   http.StatusOK,
		conn:   conn,
		rw:     bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn)),
	}
}

func (w *websocketResponseWriter) Header() http.Header {
	return w.header
}

func (w *websocketResponseWriter) WriteHeader(statusCode int) {
	if w.hijacked || w.headerSent {
		return
	}
	w.code = statusCode
	w.headerSent = true
}

func (w *websocketResponseWriter) Write(b []byte) (int, error) {
	if w.hijacked {
		return 0, http.ErrHijacked
	}
	if !w.headerSent {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(b)
}

func (w *websocketResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.hijacked {
		return nil, nil, errors.New("connection already hijacked")
	}
	w.hijacked = true
	return w.conn, w.rw, nil
}

func (w *websocketResponseWriter) Hijacked() bool {
	return w.hijacked
}

func (w *websocketResponseWriter) Response() *pb.HTTPResponse {
	return &pb.HTTPResponse{
		Status:  int32(w.code),
		Headers: headerToMap(w.header),
		Body:    w.body.String(),
	}
}

func (s *BackendServer) handleWebSocketRequest(
	stream pb.HarpService_ProxyClient,
	reqProto *pb.HTTPRequest,
	req *http.Request,
	route RouteConfig,
	sendMu *sync.Mutex,
	wsTunnels map[string]*webSocketTunnel,
	wsMu *sync.RWMutex,
) {
	tunnelConn, handlerConn := net.Pipe()
	inbound := &webSocketTunnel{
		ch:   make(chan *pb.WebSocketData, websocketTunnelBufferSize),
		done: make(chan struct{}),
	}

	wsMu.Lock()
	wsTunnels[reqProto.RequestId] = inbound
	wsMu.Unlock()
	defer func() {
		wsMu.Lock()
		delete(wsTunnels, reqProto.RequestId)
		wsMu.Unlock()
		close(inbound.done)
		_ = tunnelConn.Close()
		_ = handlerConn.Close()
	}()

	writer := newWebSocketResponseWriter(handlerConn)
	done := make(chan struct{})
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = tunnelConn.Close()
			_ = handlerConn.Close()
		})
	}

	go func() {
		defer closeBoth()
		for {
			select {
			case data := <-inbound.ch:
				if data.Close {
					return
				}
				if len(data.Data) == 0 {
					continue
				}
				if _, err := tunnelConn.Write(data.Data); err != nil {
					log.Printf("Backend %s: error writing WebSocket client data: %v", s.Name, err)
					return
				}
			case <-inbound.done:
				return
			}
		}
	}()

	go func() {
		defer close(done)
		defer closeBoth()
		buf := make([]byte, 32*1024)
		for {
			n, err := tunnelConn.Read(buf)
			if n > 0 {
				if sendErr := sendWebSocketData(stream, sendMu, reqProto.RequestId, buf[:n], false); sendErr != nil {
					log.Printf("Backend %s: error sending WebSocket data: %v", s.Name, sendErr)
					return
				}
			}
			if err != nil {
				if err != io.EOF && !errors.Is(err, net.ErrClosed) {
					log.Printf("Backend %s: error reading WebSocket backend data: %v", s.Name, err)
				}
				_ = sendWebSocketData(stream, sendMu, reqProto.RequestId, nil, true)
				return
			}
		}
	}()

	if route.Handler != nil {
		route.Handler.ServeHTTP(writer, req)
	} else {
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte("no handler registered for " + req.URL.Path))
	}

	if !writer.Hijacked() {
		resp := writer.Response()
		resp.RequestId = reqProto.RequestId
		resp.Timestamp = time.Now().UnixNano()
		resp.Latency = time.Since(time.Unix(0, reqProto.Timestamp)).Nanoseconds()
		sendMu.Lock()
		if err := stream.Send(&pb.ClientMessage{
			Payload: &pb.ClientMessage_HttpResponse{HttpResponse: resp},
		}); err != nil {
			log.Printf("Backend %s: error sending WebSocket fallback response: %v", s.Name, err)
		}
		sendMu.Unlock()
		closeBoth()
		return
	}

	closeBoth()
	<-done
	_ = sendWebSocketData(stream, sendMu, reqProto.RequestId, nil, true)
}

func sendWebSocketData(
	stream pb.HarpService_ProxyClient,
	sendMu *sync.Mutex,
	reqID string,
	data []byte,
	close bool,
) error {
	sendMu.Lock()
	defer sendMu.Unlock()
	return stream.Send(&pb.ClientMessage{Payload: &pb.ClientMessage_WebsocketData{WebsocketData: &pb.WebSocketData{
		RequestId: reqID,
		Data:      append([]byte(nil), data...),
		Close:     close,
	}}})
}
