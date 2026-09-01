package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	pb "github.com/SimonWaldherr/HARP/harp"
	"github.com/google/uuid"
)

const websocketTunnelBufferSize = 64

type proxyWebSocketTunnel struct {
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

func handleWebSocket(w http.ResponseWriter, r *http.Request, chosen *backendConn, headers map[string]string, headerValues []*pb.HTTPHeader) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket upgrade not supported", http.StatusInternalServerError)
		return
	}

	reqID := uuid.New().String()
	respCh := make(chan *pb.HTTPResponse, 1)
	wsTunnel := &proxyWebSocketTunnel{
		ch:   make(chan *pb.WebSocketData, websocketTunnelBufferSize),
		done: make(chan struct{}),
	}

	pendingResponses.Set(reqID, respCh)
	pendingWebSockets.Set(reqID, wsTunnel)
	defer func() {
		pendingResponses.Delete(reqID)
		pendingWebSockets.Delete(reqID)
		close(wsTunnel.done)
	}()

	httpReq := &pb.HTTPRequest{
		Method:       r.Method,
		Url:          r.URL.String(),
		Headers:      headers,
		HeaderValues: headerValues,
		RequestId:    reqID,
		Timestamp:    time.Now().UnixNano(),
	}

	chosen.mu.Lock()
	err := chosen.stream.Send(&pb.ServerMessage{Payload: &pb.ServerMessage_HttpRequest{HttpRequest: httpReq}})
	chosen.mu.Unlock()
	if err != nil {
		http.Error(w, "Error forwarding WebSocket upgrade", http.StatusBadGateway)
		logError("Error sending WebSocket upgrade to backend: %v", err)
		metrics.BackendErrors.Add(1)
		return
	}

	timer := time.NewTimer(configuredRequestTimeout())
	defer timer.Stop()

	select {
	case resp := <-respCh:
		writeHTTPResponse(w, resp)
		return
	case first := <-wsTunnel.ch:
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			logError("Error hijacking WebSocket client connection: %v", err)
			metrics.BackendErrors.Add(1)
			_ = sendWebSocketClose(chosen, reqID)
			return
		}
		_ = conn.SetDeadline(time.Time{})
		tunnelWebSocket(conn, rw, chosen, reqID, first, wsTunnel)
	case <-timer.C:
		http.Error(w, "Timeout waiting for WebSocket backend", http.StatusGatewayTimeout)
		metrics.BackendErrors.Add(1)
		_ = sendWebSocketClose(chosen, reqID)
	case <-r.Context().Done():
		_ = sendWebSocketClose(chosen, reqID)
	}
}

func writeHTTPResponse(w http.ResponseWriter, resp *pb.HTTPResponse) {
	headers := filterInternalHTTPHeaders(pb.HTTPHeaderFromProto(resp.Headers, resp.HeaderValues))
	appendVia(headers)
	copyHTTPHeaders(w.Header(), headers)
	status := int(resp.Status)
	if status == 0 {
		status = http.StatusBadGateway
	}
	w.WriteHeader(status)
	if body := pb.BodyBytesFromProto(resp.Body, resp.BodyBytes); len(body) > 0 {
		_, _ = w.Write(body)
	}
}

func tunnelWebSocket(
	conn net.Conn,
	rw *bufio.ReadWriter,
	chosen *backendConn,
	reqID string,
	first *pb.WebSocketData,
	wsTunnel *proxyWebSocketTunnel,
) {
	defer conn.Close()

	var closeOnce sync.Once
	closeBackend := func() {
		closeOnce.Do(func() {
			_ = sendWebSocketClose(chosen, reqID)
		})
	}
	defer closeBackend()

	if first != nil {
		if first.Close {
			return
		}
		if len(first.Data) > 0 {
			if _, err := conn.Write(first.Data); err != nil {
				logDebug("Error writing initial WebSocket data to client: %v", err)
				return
			}
		}
	}

	if rw.Reader.Buffered() > 0 {
		buffered := make([]byte, rw.Reader.Buffered())
		if _, err := io.ReadFull(rw.Reader, buffered); err == nil && len(buffered) > 0 {
			if err := sendWebSocketData(chosen, reqID, buffered); err != nil {
				logDebug("Error forwarding buffered WebSocket client data: %v", err)
				return
			}
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case data := <-wsTunnel.ch:
				if data.Close {
					return
				}
				if len(data.Data) == 0 {
					continue
				}
				if _, err := conn.Write(data.Data); err != nil {
					logDebug("Error writing WebSocket data to client: %v", err)
					return
				}
			case <-wsTunnel.done:
				return
			}
		}
	}()

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := rw.Reader.Read(buf)
			if n > 0 {
				if sendErr := sendWebSocketData(chosen, reqID, buf[:n]); sendErr != nil {
					logDebug("Error forwarding WebSocket client data: %v", sendErr)
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					logDebug("Error reading WebSocket client data: %v", err)
				}
				return
			}
		}
	}()

	select {
	case <-done:
	case <-readDone:
	}
}

func sendWebSocketData(chosen *backendConn, reqID string, data []byte) error {
	chosen.mu.Lock()
	defer chosen.mu.Unlock()
	return chosen.stream.Send(&pb.ServerMessage{Payload: &pb.ServerMessage_WebsocketData{WebsocketData: &pb.WebSocketData{
		RequestId: reqID,
		Data:      append([]byte(nil), data...),
	}}})
}

func sendWebSocketClose(chosen *backendConn, reqID string) error {
	chosen.mu.Lock()
	defer chosen.mu.Unlock()
	return chosen.stream.Send(&pb.ServerMessage{Payload: &pb.ServerMessage_WebsocketData{WebsocketData: &pb.WebSocketData{
		RequestId: reqID,
		Close:     true,
	}}})
}
