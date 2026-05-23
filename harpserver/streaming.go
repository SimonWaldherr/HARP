package harpserver

import (
	"net/http"
	"strings"
	"sync"

	pb "github.com/SimonWaldherr/HARP/harp"
)

func normalizeStreamType(streamType string) string {
	switch strings.ToLower(strings.TrimSpace(streamType)) {
	case pb.StreamTypeSSE:
		return pb.StreamTypeSSE
	case pb.StreamTypeNDJSON:
		return pb.StreamTypeNDJSON
	case pb.StreamTypeText:
		return pb.StreamTypeText
	default:
		return pb.StreamTypeChunked
	}
}

type streamingResponseWriter struct {
	headers     http.Header
	code        int
	wroteHeader bool
	sentHeaders bool
	send        func(statusCode int, headers map[string]string, body string, end bool) error
	sendErr     error
	mu          sync.Mutex
}

func newStreamingResponseWriter(send func(statusCode int, headers map[string]string, body string, end bool) error) *streamingResponseWriter {
	return &streamingResponseWriter{
		headers: make(http.Header),
		code:    http.StatusOK,
		send:    send,
	}
}

func (w *streamingResponseWriter) Header() http.Header {
	return w.headers
}

func (w *streamingResponseWriter) WriteHeader(statusCode int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wroteHeader {
		return
	}
	w.code = statusCode
	w.wroteHeader = true
}

func (w *streamingResponseWriter) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sendErr != nil {
		return 0, w.sendErr
	}
	headers := map[string]string(nil)
	if !w.sentHeaders {
		headers = headerToMap(w.headers)
		w.sentHeaders = true
	}
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	w.sendErr = w.send(w.code, headers, string(b), false)
	if w.sendErr != nil {
		return 0, w.sendErr
	}
	return len(b), nil
}

func (w *streamingResponseWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sendErr != nil || w.sentHeaders {
		return
	}
	headers := headerToMap(w.headers)
	w.wroteHeader = true
	w.sentHeaders = true
	w.sendErr = w.send(w.code, headers, "", false)
}

func (w *streamingResponseWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sendErr != nil {
		return w.sendErr
	}
	headers := map[string]string(nil)
	if !w.sentHeaders {
		headers = headerToMap(w.headers)
		w.sentHeaders = true
	}
	w.wroteHeader = true
	w.sendErr = w.send(w.code, headers, "", true)
	return w.sendErr
}
