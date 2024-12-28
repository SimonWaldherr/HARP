// proxy.go
package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	pb "github.com/SimonWaldherr/HARP/harp"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/quic-go/quic-go/http3"
)

// ------------------------------
// Configuration via flags
var (
	grpcPort      = flag.String("grpc-port", ":50051", "gRPC server listen address for backend registration")
	enableGRPCTLS = flag.Bool("grpc-tls", false, "Enable TLS for gRPC server")
	grpcTLSCert   = flag.String("grpc-cert", "", "Path to TLS certificate for gRPC")
	grpcTLSKey    = flag.String("grpc-key", "", "Path to TLS key for gRPC")

	httpPort    = flag.String("http-port", ":8080", "HTTP server listen address")
	enableHTTPS = flag.Bool("https", false, "Enable HTTPS for client connections")
	httpsCert   = flag.String("https-cert", "", "Path to TLS certificate for HTTPS")
	httpsKey    = flag.String("https-key", "", "Path to TLS key for HTTPS")

	enableHTTP3 = flag.Bool("http3", false, "Enable HTTP/3 (requires HTTPS)")
	http3Port   = flag.String("http3-port", ":8443", "HTTP/3 server listen address")

	enableCache  = flag.Bool("enable-cache", true, "Enable response caching")
	cacheType    = flag.String("cache-type", "memory", "Cache type: memory | disk | none")
	diskCacheDir = flag.String("disk-cache-dir", "./cache", "Directory for disk cache (if using disk cache)")
	cacheTTL     = flag.Duration("cache-ttl", 30*time.Minute, "Default cache TTL for responses")

	logLevel = flag.String("log-level", "INFO", "Log level: DEBUG, INFO, WARN, ERROR")
)

// ------------------------------
// Global registries
type backendConn struct {
	stream pb.HarpService_ProxyServer
	routes []registeredRoute
	mu     sync.Mutex // protects stream writes
}

type registeredRoute struct {
	name    string
	pattern *regexp.Regexp
	domain  string
	path    string
}

var (
	backendsMu sync.RWMutex
	// For simplicity, map route path to backend connection.
	backends = make(map[string]*backendConn)

	// Pending responses keyed by request ID.
	pendingMu       sync.RWMutex
	pendingResponse = make(map[string]chan *pb.HTTPResponse)
)

// ------------------------------
// Cache interface and implementations
type Cache interface {
	Get(key string) (*pb.HTTPResponse, bool)
	Set(key string, resp *pb.HTTPResponse)
}

// MemoryCache implementation.
type MemoryCache struct {
	mu    sync.RWMutex
	items map[string]cacheItem
}

type cacheItem struct {
	resp      *pb.HTTPResponse
	expiresAt time.Time
}

func NewMemoryCache() *MemoryCache {
	return &MemoryCache{items: make(map[string]cacheItem)}
}

func (mc *MemoryCache) Get(key string) (*pb.HTTPResponse, bool) {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	item, ok := mc.items[key]
	if !ok || time.Now().After(item.expiresAt) {
		return nil, false
	}
	return item.resp, true
}

func (mc *MemoryCache) Set(key string, resp *pb.HTTPResponse) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.items[key] = cacheItem{
		resp:      resp,
		expiresAt: time.Now().Add(*cacheTTL),
	}
}

// DiskCache implementation.
type DiskCache struct {
	dir string
}

func NewDiskCache(dir string) *DiskCache {
	os.MkdirAll(dir, 0755)
	return &DiskCache{dir: dir}
}

func (dc *DiskCache) cacheFile(key string) string {
	return filepath.Join(dc.dir, key+".json")
}

func (dc *DiskCache) Get(key string) (*pb.HTTPResponse, bool) {
	filename := dc.cacheFile(key)
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, false
	}
	var item cacheItem
	if err := json.Unmarshal(data, &item); err != nil {
		return nil, false
	}
	if time.Now().After(item.expiresAt) {
		os.Remove(filename)
		return nil, false
	}
	return item.resp, true
}

func (dc *DiskCache) Set(key string, resp *pb.HTTPResponse) {
	item := cacheItem{
		resp:      resp,
		expiresAt: time.Now().Add(*cacheTTL),
	}
	data, err := json.Marshal(item)
	if err != nil {
		log.Printf("Error marshaling cache item: %v", err)
		return
	}
	filename := dc.cacheFile(key)
	if err := ioutil.WriteFile(filename, data, 0644); err != nil {
		log.Printf("Error writing cache file: %v", err)
	}
}

var cacheStore Cache

// getCacheKey computes a cache key from request details.
func getCacheKey(method, url string, headers map[string]string, body string) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte(url))
	for k, v := range headers {
		h.Write([]byte(k))
		h.Write([]byte(v))
	}
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}

// ------------------------------
// gRPC Service Implementation (Proxy)
type harpService struct {
	pb.UnimplementedHarpServiceServer
}

func (s *harpService) Proxy(stream pb.HarpService_ProxyServer) error {
	// Expect first message to be registration.
	msg, err := stream.Recv()
	if err != nil {
		return err
	}
	reg := msg.GetRegistration()
	if reg == nil {
		return fmt.Errorf("expected registration message")
	}
	// Create backend connection.
	conn := &backendConn{stream: stream}
	for _, r := range reg.Routes {
		patternStr := reg.Domain + r.Path
		pattern, err := regexp.Compile(patternStr)
		if err != nil {
			log.Printf("Error compiling regexp for route %s: %v", r.Path, err)
			continue
		}
		conn.routes = append(conn.routes, registeredRoute{
			name:    r.Name,
			pattern: pattern,
			domain:  reg.Domain,
			path:    r.Path,
		})
		backendsMu.Lock()
		backends[r.Path] = conn
		backendsMu.Unlock()
	}

	// Receive HTTP responses from the backend.
	go func() {
		for {
			clientMsg, err := stream.Recv()
			if err != nil {
				log.Printf("Backend disconnected: %v", err)
				return
			}
			httpResp := clientMsg.GetHttpResponse()
			if httpResp == nil {
				continue
			}
			pendingMu.RLock()
			ch, ok := pendingResponse[httpResp.RequestId]
			pendingMu.RUnlock()
			if ok {
				ch <- httpResp
			}
		}
	}()

	<-stream.Context().Done()
	return nil
}

// ------------------------------
// HTTP Handler for Client Requests
func httpHandler(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading body", http.StatusBadRequest)
		return
	}
	r.Body.Close()
	bodyStr := string(bodyBytes)

	reqID := uuid.New().String()

	headers := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	cacheKey := getCacheKey(r.Method, r.URL.String(), headers, bodyStr)
	if *enableCache && strings.ToUpper(r.Method) == "GET" && *cacheType != "none" {
		if resp, ok := cacheStore.Get(cacheKey); ok {
			log.Printf("Cache hit for %s", r.URL.String())
			for k, v := range resp.Headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(int(resp.Status))
			fmt.Fprint(w, resp.Body)
			return
		}
	}

	// Build an HTTPRequest message.
	httpReq := &pb.HTTPRequest{
		Method:    r.Method,
		Url:       r.URL.String(),
		Headers:   headers,
		Body:      bodyStr,
		RequestId: reqID,
		Timestamp: time.Now().UnixNano(),
	}

	// Lookup matching backend.
	var chosen *backendConn
	backendsMu.RLock()
	for _, conn := range backends {
		for _, route := range conn.routes {
			if route.pattern.MatchString(r.URL.Path) {
				chosen = conn
				break
			}
		}
		if chosen != nil {
			break
		}
	}
	backendsMu.RUnlock()

	if chosen == nil {
		http.NotFound(w, r)
		return
	}

	// Prepare channel for response.
	respCh := make(chan *pb.HTTPResponse, 1)
	pendingMu.Lock()
	pendingResponse[reqID] = respCh
	pendingMu.Unlock()

	// Build ServerMessage containing the HTTPRequest.
	serverMsg := &pb.ServerMessage{
		HttpRequest: httpReq,
	}

	// Send the request to the chosen backend.
	chosen.mu.Lock()
	err = chosen.stream.Send(serverMsg)
	chosen.mu.Unlock()
	if err != nil {
		http.Error(w, "Error forwarding request", http.StatusBadGateway)
		return
	}

	// Wait for the HTTP response.
	select {
	case resp := <-respCh:
		for k, v := range resp.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(int(resp.Status))
		fmt.Fprint(w, resp.Body)
		if *enableCache && strings.ToUpper(r.Method) == "GET" && *cacheType != "none" {
			cacheStore.Set(cacheKey, resp)
		}
	case <-time.After(30 * time.Second):
		http.Error(w, "Timeout waiting for backend", http.StatusGatewayTimeout)
	}

	pendingMu.Lock()
	delete(pendingResponse, reqID)
	pendingMu.Unlock()
}

// ------------------------------
// Main: Start gRPC and HTTP servers
func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("Log level: %s", *logLevel)

	switch strings.ToLower(*cacheType) {
	case "memory":
		cacheStore = NewMemoryCache()
	case "disk":
		cacheStore = NewDiskCache(*diskCacheDir)
	default:
		cacheStore = nil
		*enableCache = false
	}

	go startGRPCServer()

	if *enableHTTPS {
		if *enableHTTP3 {
			go startHTTP3Server()
		}
		startHTTPSServer()
	} else {
		startHTTPServer()
	}
}

func startGRPCServer() {
	var opts []grpc.ServerOption
	if *enableGRPCTLS {
		if *grpcTLSCert == "" || *grpcTLSKey == "" {
			log.Fatal("grpc-tls enabled but cert or key not provided")
		}
		creds, err := credentials.NewServerTLSFromFile(*grpcTLSCert, *grpcTLSKey)
		if err != nil {
			log.Fatalf("Failed to load gRPC TLS credentials: %v", err)
		}
		opts = append(opts, grpc.Creds(creds))
	}
	grpcServer := grpc.NewServer(opts...)
	pb.RegisterHarpServiceServer(grpcServer, &harpService{})
	lis, err := net.Listen("tcp", *grpcPort)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", *grpcPort, err)
	}
	log.Printf("gRPC server listening on %s", *grpcPort)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("gRPC server error: %v", err)
	}
}

func startHTTPServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", httpHandler)
	server := &http.Server{
		Addr:         *httpPort,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	log.Printf("HTTP server listening on %s", *httpPort)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("HTTP server error: %v", err)
	}
}

func startHTTPSServer() {
	if *httpsCert == "" || *httpsKey == "" {
		log.Fatal("https enabled but cert or key not provided")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", httpHandler)
	server := &http.Server{
		Addr:         *httpPort,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	log.Printf("HTTPS server listening on %s", *httpPort)
	if err := server.ListenAndServeTLS(*httpsCert, *httpsKey); err != nil {
		log.Fatalf("HTTPS server error: %v", err)
	}
}

func startHTTP3Server() {
	if *httpsCert == "" || *httpsKey == "" {
		log.Fatal("http3 enabled but https cert or key not provided")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", httpHandler)
	server := &http3.Server{
		Addr:      *http3Port,
		Handler:   mux,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13},
	}
	log.Printf("HTTP/3 server listening on %s", *http3Port)
	if err := server.ListenAndServeTLS(*httpsCert, *httpsKey); err != nil {
		log.Fatalf("HTTP/3 server error: %v", err)
	}
}
