// Description: A simple HTTP reverse proxy that forwards requests to registered backends.
package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	pb "github.com/SimonWaldherr/HARP/harp"
	"github.com/google/uuid"
	"github.com/quic-go/quic-go/http3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
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

	// New optimization settings
	MaxConcurrentRequests int    `json:"maxConcurrentRequests"`
	RequestTimeout        string `json:"requestTimeout"`
	EnableMetrics         bool   `json:"enableMetrics"`
	MetricsPort           string `json:"metricsPort"`
	EnableHealthCheck     bool   `json:"enableHealthCheck"`
	HealthCheckPath       string `json:"healthCheckPath"`
	ConnectionPoolSize    int    `json:"connectionPoolSize"`
	EnableRateLimit       bool   `json:"enableRateLimit"`
	RateLimitPerSecond    int    `json:"rateLimitPerSecond"`
	EnableCompression     bool   `json:"enableCompression"`
	MaxHeaderSize         int    `json:"maxHeaderSize"`
	ReadTimeout           string `json:"readTimeout"`
	WriteTimeout          string `json:"writeTimeout"`
	IdleTimeout           string `json:"idleTimeout"`
}

type AllowedRegistrationRule struct {
	Route string `json:"route"`
	Key   string `json:"key"`
}

// Metrics structure for monitoring
type Metrics struct {
	RequestsTotal      *expvar.Int
	RequestDuration    *expvar.Map
	CacheHits          *expvar.Int
	CacheMisses        *expvar.Int
	BackendErrors      *expvar.Int
	ActiveConnections  *expvar.Int
	BackendsRegistered *expvar.Int
}

var (
	config  Config
	metrics *Metrics
)

// Initialize metrics
func initMetrics() {
	metrics = &Metrics{
		RequestsTotal:      expvar.NewInt("requests_total"),
		RequestDuration:    expvar.NewMap("request_duration_seconds"),
		CacheHits:          expvar.NewInt("cache_hits_total"),
		CacheMisses:        expvar.NewInt("cache_misses_total"),
		BackendErrors:      expvar.NewInt("backend_errors_total"),
		ActiveConnections:  expvar.NewInt("active_connections"),
		BackendsRegistered: expvar.NewInt("backends_registered"),
	}
}

// Logging helpers.
func logDebug(format string, v ...interface{}) {
	if strings.ToUpper(config.LogLevel) == "DEBUG" {
		log.Printf("[DEBUG] "+format, v...)
	}
}
func logInfo(format string, v ...interface{})  { log.Printf("[INFO] "+format, v...) }
func logWarn(format string, v ...interface{})  { log.Printf("[WARN] "+format, v...) }
func logError(format string, v ...interface{}) { log.Printf("[ERROR] "+format, v...) }

// --- Enhanced Cache interfaces and implementations ---
type Cache interface {
	Get(key string) (*pb.HTTPResponse, bool)
	Set(key string, resp *pb.HTTPResponse)
	Delete(key string)
	Clear()
	Size() int
}

type MemoryCache struct {
	mu       sync.RWMutex
	items    map[string]cacheItem
	maxItems int
}

type cacheItem struct {
	resp      *pb.HTTPResponse
	expiresAt time.Time
}

func NewMemoryCache() *MemoryCache {
	maxItems := 1000 // Default max items
	if config.ConnectionPoolSize > 0 {
		maxItems = config.ConnectionPoolSize * 10
	}
	return &MemoryCache{
		items:    make(map[string]cacheItem),
		maxItems: maxItems,
	}
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

	// Simple eviction if cache is full
	if len(mc.items) >= mc.maxItems {
		// Remove first item (simple FIFO)
		for k := range mc.items {
			delete(mc.items, k)
			break
		}
	}

	ttl, err := time.ParseDuration(config.CacheTTL)
	if err != nil {
		ttl = 30 * time.Minute
	}
	mc.items[key] = cacheItem{
		resp:      resp,
		expiresAt: time.Now().Add(ttl),
	}
}

func (mc *MemoryCache) Delete(key string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	delete(mc.items, key)
}

func (mc *MemoryCache) Clear() {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.items = make(map[string]cacheItem)
}

func (mc *MemoryCache) Size() int {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	return len(mc.items)
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

func (dc *DiskCache) Delete(key string) {
	os.Remove(dc.cacheFile(key))
}

func (dc *DiskCache) Clear() {
	files, err := filepath.Glob(filepath.Join(dc.dir, "*.json"))
	if err != nil {
		return
	}
	for _, file := range files {
		os.Remove(file)
	}
}

func (dc *DiskCache) Size() int {
	files, err := filepath.Glob(filepath.Join(dc.dir, "*.json"))
	if err != nil {
		return 0
	}
	return len(files)
}

var cacheStore Cache

// Rate limiter
type RateLimiter struct {
	mu       sync.Mutex
	requests map[string][]time.Time
	limit    int
}

func NewRateLimiter(limit int) *RateLimiter {
	return &RateLimiter{
		requests: make(map[string][]time.Time),
		limit:    limit,
	}
}

func (rl *RateLimiter) Allow(clientIP string) bool {
	if !config.EnableRateLimit {
		return true
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	requests := rl.requests[clientIP]

	// Remove old requests (older than 1 second)
	var validRequests []time.Time
	for _, t := range requests {
		if now.Sub(t) < time.Second {
			validRequests = append(validRequests, t)
		}
	}

	if len(validRequests) >= rl.limit {
		return false
	}

	validRequests = append(validRequests, now)
	rl.requests[clientIP] = validRequests

	return true
}

var rateLimiter *RateLimiter

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
	startTime       time.Time
)

// Health check endpoint
func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	health := map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now().Unix(),
		"backends":  len(backends),
		"uptime":    time.Since(startTime).Seconds(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(health)
}

// Metrics endpoint
func metricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	stats := map[string]interface{}{
		"requests_total":      metrics.RequestsTotal.Value(),
		"cache_hits":          metrics.CacheHits.Value(),
		"cache_misses":        metrics.CacheMisses.Value(),
		"backend_errors":      metrics.BackendErrors.Value(),
		"active_connections":  metrics.ActiveConnections.Value(),
		"backends_registered": metrics.BackendsRegistered.Value(),
		"memory_stats": map[string]interface{}{
			"alloc":       memStats.Alloc,
			"total_alloc": memStats.TotalAlloc,
			"sys":         memStats.Sys,
			"num_gc":      memStats.NumGC,
		},
	}

	if cacheStore != nil {
		stats["cache_stats"] = map[string]interface{}{
			"size": cacheStore.Size(),
		}
	}

	json.NewEncoder(w).Encode(stats)
}

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

	metrics.BackendsRegistered.Add(1)
	metrics.ActiveConnections.Add(1)
	defer metrics.ActiveConnections.Add(-1)

	// Launch a goroutine to receive HTTP responses from the backend.
	go func() {
		for {
			clientMsg, err := stream.Recv()
			if err != nil {
				logWarn("Backend %s disconnected: %v", reg.Name, err)
				// Cleanup backend registration
				backendsMu.Lock()
				for _, route := range conn.routes {
					delete(backends, route.path)
				}
				backendsMu.Unlock()
				metrics.BackendsRegistered.Add(-1)
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
	start := time.Now()
	defer func() {
		duration := time.Since(start)
		metrics.RequestsTotal.Add(1)
		metrics.RequestDuration.Add(r.URL.Path, int64(duration/time.Millisecond))
	}()

	// Rate limiting
	clientIP := r.RemoteAddr
	if !rateLimiter.Allow(clientIP) {
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

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
			metrics.CacheHits.Add(1)
			for k, v := range resp.Headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(int(resp.Status))
			fmt.Fprint(w, resp.Body)
			return
		}
		metrics.CacheMisses.Add(1)
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
		metrics.BackendErrors.Add(1)
		pendingMu.Lock()
		delete(pendingResponse, reqID)
		pendingMu.Unlock()
		return
	}

	// Wait for response with configurable timeout
	timeout := 30 * time.Second
	if config.RequestTimeout != "" {
		if parsedTimeout, err := time.ParseDuration(config.RequestTimeout); err == nil {
			timeout = parsedTimeout
		}
	}

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
	case <-time.After(timeout):
		http.Error(w, "Timeout waiting for backend", http.StatusGatewayTimeout)
		metrics.BackendErrors.Add(1)
	}
	pendingMu.Lock()
	delete(pendingResponse, reqID)
	pendingMu.Unlock()
}

// --- Server Starters ---
func startGRPCServer() {
	var opts []grpc.ServerOption

	// Add keepalive parameters with more stable settings
	opts = append(opts, grpc.KeepaliveParams(keepalive.ServerParameters{
		MaxConnectionIdle:     5 * time.Minute,  // Increased from 15s
		MaxConnectionAge:      30 * time.Minute, // Increased from 30s
		MaxConnectionAgeGrace: 30 * time.Second, // Increased from 5s
		Time:                  30 * time.Second, // Increased from 5s
		Timeout:               5 * time.Second,  // Increased from 1s
	}))

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

	// Add health check endpoint
	if config.EnableHealthCheck {
		healthPath := config.HealthCheckPath
		if healthPath == "" {
			healthPath = "/health"
		}
		mux.HandleFunc(healthPath, healthCheckHandler)
	}

	// Parse timeouts
	readTimeout := 30 * time.Second
	writeTimeout := 30 * time.Second
	idleTimeout := 120 * time.Second

	if config.ReadTimeout != "" {
		if parsed, err := time.ParseDuration(config.ReadTimeout); err == nil {
			readTimeout = parsed
		}
	}
	if config.WriteTimeout != "" {
		if parsed, err := time.ParseDuration(config.WriteTimeout); err == nil {
			writeTimeout = parsed
		}
	}
	if config.IdleTimeout != "" {
		if parsed, err := time.ParseDuration(config.IdleTimeout); err == nil {
			idleTimeout = parsed
		}
	}

	server := &http.Server{
		Addr:         config.HTTPPort,
		Handler:      mux,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
	}
	if config.MaxHeaderSize > 0 {
		server.MaxHeaderBytes = config.MaxHeaderSize
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

	if config.EnableHealthCheck {
		healthPath := config.HealthCheckPath
		if healthPath == "" {
			healthPath = "/health"
		}
		mux.HandleFunc(healthPath, healthCheckHandler)
	}

	server := &http.Server{
		Addr:         config.HTTPPort,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
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

	if config.EnableHealthCheck {
		healthPath := config.HealthCheckPath
		if healthPath == "" {
			healthPath = "/health"
		}
		mux.HandleFunc(healthPath, healthCheckHandler)
	}

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

func startMetricsServer() {
	if !config.EnableMetrics {
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", metricsHandler)
	mux.Handle("/debug/pprof/", http.HandlerFunc(pprof.Index))
	mux.Handle("/debug/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
	mux.Handle("/debug/pprof/profile", http.HandlerFunc(pprof.Profile))
	mux.Handle("/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
	mux.Handle("/debug/pprof/trace", http.HandlerFunc(pprof.Trace))
	mux.Handle("/debug/vars", expvar.Handler())

	metricsPort := config.MetricsPort
	if metricsPort == "" {
		metricsPort = ":9090"
	}

	server := &http.Server{
		Addr:    metricsPort,
		Handler: mux,
	}

	logInfo("Metrics server listening on %s", metricsPort)
	if err := server.ListenAndServe(); err != nil {
		logError("Metrics server error: %v", err)
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

	// Set defaults for new options
	if config.MaxConcurrentRequests == 0 {
		config.MaxConcurrentRequests = 1000
	}
	if config.RequestTimeout == "" {
		config.RequestTimeout = "30s"
	}
	if config.ConnectionPoolSize == 0 {
		config.ConnectionPoolSize = 100
	}
	if config.RateLimitPerSecond == 0 {
		config.RateLimitPerSecond = 100
	}
	if config.MaxHeaderSize == 0 {
		config.MaxHeaderSize = 8192
	}
}

func main() {
	configPath := flag.String("config", "config.json", "Path to configuration file")
	flag.Parse()

	startTime = time.Now()
	loadConfig(*configPath)
	logInfo("Enhanced HARP proxy starting with configuration: %s", *configPath)

	// Initialize metrics if enabled
	if config.EnableMetrics {
		initMetrics()
	}

	// Initialize rate limiter
	rateLimiter = NewRateLimiter(config.RateLimitPerSecond)

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

	// Start metrics server if enabled
	if config.EnableMetrics {
		go startMetricsServer()
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
