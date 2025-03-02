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
	"github.com/quic-go/quic-go/http3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Config holds proxy configuration parameters.
type Config struct {
	GRPCPort            string                    `json:"grpcPort"`
	HTTPPort            string                    `json:"httpPort"`
	HTTP3Port           string                    `json:"http3Port"`
	EnableGRPCTLS       bool                      `json:"enableGRPCTLS"`
	GRPCTLSCert         string                    `json:"grpcTLSCert"`
	GRPCTLSKey          string                    `json:"grpcTLSKey"`
	EnableHTTPS         bool                      `json:"enableHTTPS"`
	HTTPSCert           string                    `json:"httpsCert"`
	HTTPSKey            string                    `json:"httpsKey"`
	EnableHTTP3         bool                      `json:"enableHTTP3"`
	EnableCache         bool                      `json:"enableCache"`
	CacheType           string                    `json:"cacheType"`
	DiskCacheDir        string                    `json:"diskCacheDir"`
	CacheTTL            string                    `json:"cacheTTL"`
	AllowedRegistration []AllowedRegistrationRule `json:"allowedRegistration"`
	LogLevel            string                    `json:"logLevel"`
}

type AllowedRegistrationRule struct {
	Route string `json:"route"`
	Key   string `json:"key"`
}

var config Config

// Logging helpers.
func logDebug(format string, v ...interface{}) {
	if strings.ToUpper(config.LogLevel) == "DEBUG" {
		log.Printf("[DEBUG] "+format, v...)
	}
}
func logInfo(format string, v ...interface{})  { log.Printf("[INFO] "+format, v...) }
func logWarn(format string, v ...interface{})  { log.Printf("[WARN] "+format, v...) }
func logError(format string, v ...interface{}) { log.Printf("[ERROR] "+format, v...) }

// --- Cache interfaces and implementations ---
type Cache interface {
	Get(key string) (*pb.HTTPResponse, bool)
	Set(key string, resp *pb.HTTPResponse)
}

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
	ttl, err := time.ParseDuration(config.CacheTTL)
	if err != nil {
		ttl = 30 * time.Minute
	}
	mc.items[key] = cacheItem{
		resp:      resp,
		expiresAt: time.Now().Add(ttl),
	}
}

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
	var item struct {
		Resp      *pb.HTTPResponse `json:"resp"`
		ExpiresAt time.Time        `json:"expiresAt"`
	}
	if err := json.Unmarshal(data, &item); err != nil {
		return nil, false
	}
	if time.Now().After(item.ExpiresAt) {
		os.Remove(filename)
		return nil, false
	}
	return item.Resp, true
}

func (dc *DiskCache) Set(key string, resp *pb.HTTPResponse) {
	ttl, err := time.ParseDuration(config.CacheTTL)
	if err != nil {
		ttl = 30 * time.Minute
	}
	item := struct {
		Resp      *pb.HTTPResponse `json:"resp"`
		ExpiresAt time.Time        `json:"expiresAt"`
	}{
		Resp:      resp,
		ExpiresAt: time.Now().Add(ttl),
	}
	data, err := json.Marshal(item)
	if err != nil {
		logError("Error marshaling cache item: %v", err)
		return
	}
	filename := dc.cacheFile(key)
	if err := ioutil.WriteFile(filename, data, 0644); err != nil {
		logError("Error writing cache file: %v", err)
	}
}

var cacheStore Cache

// getCacheKey computes a key for caching.
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

// --- Global registries for backends and pending responses ---
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
	// Map route path to backend connection.
	backends = make(map[string]*backendConn)
	// Pending responses keyed by request ID.
	pendingMu       sync.RWMutex
	pendingResponse = make(map[string]chan *pb.HTTPResponse)
)

// --- gRPC Service Implementation ---
type harpService struct {
	pb.UnimplementedHarpServiceServer
}

func (s *harpService) Proxy(stream pb.HarpService_ProxyServer) error {
	// Expect first message to be a Registration.
	msg, err := stream.Recv()
	if err != nil {
		logError("Error receiving registration: %v", err)
		return err
	}
	reg := msg.GetRegistration()
	if reg == nil {
		return fmt.Errorf("expected registration message")
	}

	// Check each route against allowedRegistration rules.
	allowedRoutes := []*pb.Route{}
	for _, r := range reg.Routes {
		allowed := false
		for _, rule := range config.AllowedRegistration {
			matched, err := regexp.MatchString(rule.Route, r.Path)
			if err != nil {
				logError("Error matching regex: %v", err)
				continue
			}
			if matched && reg.Key == rule.Key {
				allowed = true
				break
			}
		}
		if allowed {
			allowedRoutes = append(allowedRoutes, r)
		}
	}
	if len(allowedRoutes) == 0 {
		logWarn("Rejected registration from %s: no allowed routes", reg.Name)
		return fmt.Errorf("authentication failed: no allowed routes")
	}
	reg.Routes = allowedRoutes
	logInfo("Accepted registration from backend: %s (routes: %d)", reg.Name, len(reg.Routes))

	// Create backend connection.
	conn := &backendConn{stream: stream}
	for _, r := range reg.Routes {
		patternStr := reg.Domain + r.Path
		pattern, err := regexp.Compile(patternStr)
		if err != nil {
			logError("Error compiling regexp for route %s: %v", r.Path, err)
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
		logDebug("Registered route: %s%s", reg.Domain, r.Path)
	}

	// Launch a goroutine to receive HTTP responses from the backend.
	go func() {
		for {
			clientMsg, err := stream.Recv()
			if err != nil {
				logWarn("Backend %s disconnected: %v", reg.Name, err)
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
			} else {
				logDebug("No pending request for response ID %s", httpResp.RequestId)
			}
		}
	}()

	<-stream.Context().Done()
	logInfo("gRPC stream closed for backend: %s", reg.Name)
	return nil
}

// --- HTTP Handler for Client Requests ---
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
	if config.EnableCache && strings.ToUpper(r.Method) == "GET" && config.CacheType != "none" {
		if resp, ok := cacheStore.Get(cacheKey); ok {
			logDebug("Cache hit for %s", r.URL.String())
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

	// Find a matching backend.
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
		logWarn("No matching route for %s", r.URL.String())
		http.NotFound(w, r)
		return
	}

	// Prepare channel for response.
	respCh := make(chan *pb.HTTPResponse, 1)
	pendingMu.Lock()
	pendingResponse[reqID] = respCh
	pendingMu.Unlock()

	// Send the HTTPRequest to the chosen backend.
	serverMsg := &pb.ServerMessage{HttpRequest: httpReq}
	chosen.mu.Lock()
	err = chosen.stream.Send(serverMsg)
	chosen.mu.Unlock()
	if err != nil {
		http.Error(w, "Error forwarding request", http.StatusBadGateway)
		logError("Error sending to backend: %v", err)
		return
	}

	// Wait for response.
	select {
	case resp := <-respCh:
		for k, v := range resp.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(int(resp.Status))
		fmt.Fprint(w, resp.Body)
		if config.EnableCache && strings.ToUpper(r.Method) == "GET" && config.CacheType != "none" {
			cacheStore.Set(cacheKey, resp)
		}
	case <-time.After(30 * time.Second):
		http.Error(w, "Timeout waiting for backend", http.StatusGatewayTimeout)
	}
	pendingMu.Lock()
	delete(pendingResponse, reqID)
	pendingMu.Unlock()
}

// --- Server Starters ---
func startGRPCServer() {
	var opts []grpc.ServerOption
	if config.EnableGRPCTLS {
		if config.GRPCTLSCert == "" || config.GRPCTLSKey == "" {
			log.Fatal("grpc-tls enabled but cert or key not provided")
		}
		creds, err := credentials.NewServerTLSFromFile(config.GRPCTLSCert, config.GRPCTLSKey)
		if err != nil {
			log.Fatalf("Failed to load gRPC TLS credentials: %v", err)
		}
		opts = append(opts, grpc.Creds(creds))
	}
	grpcServer := grpc.NewServer(opts...)
	pb.RegisterHarpServiceServer(grpcServer, &harpService{})
	lis, err := net.Listen("tcp", config.GRPCPort)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", config.GRPCPort, err)
	}
	logInfo("gRPC server listening on %s", config.GRPCPort)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("gRPC server error: %v", err)
	}
}

func startHTTPServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", httpHandler)
	server := &http.Server{
		Addr:         config.HTTPPort,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	logInfo("HTTP server listening on %s", config.HTTPPort)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("HTTP server error: %v", err)
	}
}

func startHTTPSServer() {
	if config.HTTPSCert == "" || config.HTTPSKey == "" {
		log.Fatal("https enabled but cert or key not provided")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", httpHandler)
	server := &http.Server{
		Addr:         config.HTTPPort,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	logInfo("HTTPS server listening on %s", config.HTTPPort)
	if err := server.ListenAndServeTLS(config.HTTPSCert, config.HTTPSKey); err != nil {
		log.Fatalf("HTTPS server error: %v", err)
	}
}

func startHTTP3Server() {
	if config.HTTPSCert == "" || config.HTTPSKey == "" {
		log.Fatal("http3 enabled but https cert or key not provided")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", httpHandler)
	server := &http3.Server{
		Addr:      config.HTTP3Port,
		Handler:   mux,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13},
	}
	logInfo("HTTP/3 server listening on %s", config.HTTP3Port)
	if err := server.ListenAndServeTLS(config.HTTPSCert, config.HTTPSKey); err != nil {
		log.Fatalf("HTTP/3 server error: %v", err)
	}
}

func loadConfig(path string) {
	file, err := os.Open(path)
	if err != nil {
		log.Fatalf("Unable to open config file: %v", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&config); err != nil {
		log.Fatalf("Unable to decode config file: %v", err)
	}
}

func main() {
	configPath := flag.String("config", "config.json", "Path to configuration file")
	flag.Parse()
	loadConfig(*configPath)
	logInfo("Configuration loaded: %+v", config)

	// Initialize cache.
	switch strings.ToLower(config.CacheType) {
	case "memory":
		cacheStore = NewMemoryCache()
	case "disk":
		cacheStore = NewDiskCache(config.DiskCacheDir)
	default:
		cacheStore = nil
		config.EnableCache = false
	}

	// Start gRPC server.
	go startGRPCServer()

	// Start HTTP/HTTPS/HTTP3 server as configured.
	if config.EnableHTTPS {
		if config.EnableHTTP3 {
			go startHTTP3Server()
		}
		startHTTPSServer()
	} else {
		startHTTPServer()
	}
}
