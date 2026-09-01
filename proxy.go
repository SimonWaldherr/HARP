// Description: A simple HTTP reverse proxy that forwards requests to registered backends.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"expvar"
	"flag"
	"fmt"
	"hash/maphash"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	runtimemetrics "runtime/metrics"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	GRPCPort              string                    `json:"grpcPort"`
	HTTPPort              string                    `json:"httpPort"`
	HTTP3Port             string                    `json:"http3Port"`
	EnableGRPCTLS         bool                      `json:"enableGRPCTLS"`
	GRPCTLSCert           string                    `json:"grpcTLSCert"`
	GRPCTLSKey            string                    `json:"grpcTLSKey"`
	EnableHTTPS           bool                      `json:"enableHTTPS"`
	HTTPSCert             string                    `json:"httpsCert"`
	HTTPSKey              string                    `json:"httpsKey"`
	EnableHTTP3           bool                      `json:"enableHTTP3"`
	EnableCache           bool                      `json:"enableCache"`
	CacheType             string                    `json:"cacheType"`
	DiskCacheDir          string                    `json:"diskCacheDir"`
	CacheTTL              string                    `json:"cacheTTL"`
	AllowedRegistration   []AllowedRegistrationRule `json:"allowedRegistration"`
	LogLevel              string                    `json:"logLevel"`
	LoadBalancingStrategy string                    `json:"loadBalancingStrategy"`

	// New optimization settings
	MaxConcurrentRequests int      `json:"maxConcurrentRequests"`
	RequestTimeout        string   `json:"requestTimeout"`
	EnableMetrics         bool     `json:"enableMetrics"`
	MetricsPort           string   `json:"metricsPort"`
	EnableHealthCheck     bool     `json:"enableHealthCheck"`
	HealthCheckPath       string   `json:"healthCheckPath"`
	ConnectionPoolSize    int      `json:"connectionPoolSize"`
	EnableRateLimit       bool     `json:"enableRateLimit"`
	RateLimitPerSecond    int      `json:"rateLimitPerSecond"`
	EnableCompression     bool     `json:"enableCompression"`
	MaxHeaderSize         int      `json:"maxHeaderSize"`
	ReadTimeout           string   `json:"readTimeout"`
	WriteTimeout          string   `json:"writeTimeout"`
	IdleTimeout           string   `json:"idleTimeout"`
	MaxRequestBodySize    int64    `json:"maxRequestBodySize"`
	EnableCORS            bool     `json:"enableCORS"`
	CORSAllowedOrigins    string   `json:"corsAllowedOrigins"`
	GracefulShutdownDelay string   `json:"gracefulShutdownDelay"`
	EnableAdminUI         bool     `json:"enableAdminUI"`
	AdminPath             string   `json:"adminPath"`
	AdminUsername         string   `json:"adminUsername"`
	AdminPassword         string   `json:"adminPassword"`
	AdminInsecureSkipAuth bool     `json:"adminInsecureSkipAuth"`
	AdminAllowedCIDRs     []string `json:"adminAllowedCIDRs"`
}

type AllowedRegistrationRule struct {
	Route    string `json:"route"`
	Key      string `json:"key"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type compiledAllowedRegistrationRule struct {
	pattern  *regexp.Regexp
	route    string
	key      string
	username string
	password string
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
	RouteRequests      *expvar.Map
	RateLimited        *expvar.Int
}

var (
	config                   Config
	metrics                  *Metrics
	allowedRegistrationRules []compiledAllowedRegistrationRule
	adminAllowedPrefixes     []netip.Prefix
)

const (
	loadBalancingRoundRobin = "round_robin"
	loadBalancingFirst      = "first"
)

var schedulerMetricNames = []string{
	"/sched/gomaxprocs:threads",
	"/sched/goroutines:goroutines",
	"/sched/goroutines/not-in-go:goroutines",
	"/sched/goroutines/runnable:goroutines",
	"/sched/goroutines/running:goroutines",
	"/sched/goroutines/waiting:goroutines",
	"/sched/goroutines-created:goroutines",
	"/sched/threads/total:threads",
}

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
		RouteRequests:      expvar.NewMap("route_requests_total"),
		RateLimited:        expvar.NewInt("rate_limited_total"),
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
	ttl      time.Duration
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
		ttl:      configuredCacheTTL(),
	}
}

func (mc *MemoryCache) Get(key string) (*pb.HTTPResponse, bool) {
	mc.mu.RLock()
	item, ok := mc.items[key]
	if !ok {
		mc.mu.RUnlock()
		return nil, false
	}
	expired := time.Now().After(item.expiresAt)
	mc.mu.RUnlock()
	if expired {
		mc.mu.Lock()
		if current, ok := mc.items[key]; ok && time.Now().After(current.expiresAt) {
			delete(mc.items, key)
		}
		mc.mu.Unlock()
		return nil, false
	}
	return item.resp, true
}

func (mc *MemoryCache) Set(key string, resp *pb.HTTPResponse) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	// LRU eviction: remove the item closest to expiry
	if len(mc.items) >= mc.maxItems {
		var oldestKey string
		var oldestTime time.Time
		first := true
		for k, v := range mc.items {
			// Also evict already-expired entries first
			if time.Now().After(v.expiresAt) {
				delete(mc.items, k)
				first = false
				break
			}
			if first || v.expiresAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.expiresAt
				first = false
			}
		}
		if !first && oldestKey != "" && len(mc.items) >= mc.maxItems {
			delete(mc.items, oldestKey)
		}
	}

	ttl := mc.ttl
	if ttl <= 0 {
		ttl = configuredCacheTTL()
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
	ttl time.Duration
}

func NewDiskCache(dir string) *DiskCache {
	if err := os.MkdirAll(dir, 0755); err != nil {
		logError("Error creating disk cache directory %s: %v", dir, err)
	}
	return &DiskCache{dir: dir, ttl: configuredCacheTTL()}
}

func (dc *DiskCache) cacheFile(key string) string {
	return filepath.Join(dc.dir, key+".json")
}

func (dc *DiskCache) Get(key string) (*pb.HTTPResponse, bool) {
	filename := dc.cacheFile(key)
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, false
	}
	var item struct {
		Resp      *pb.HTTPResponse `json:"resp"`
		ExpiresAt time.Time        `json:"expiresAt"`
	}
	if err := json.Unmarshal(data, &item); err != nil {
		_ = os.Remove(filename)
		return nil, false
	}
	if time.Now().After(item.ExpiresAt) {
		os.Remove(filename)
		return nil, false
	}
	return item.Resp, true
}

func (dc *DiskCache) Set(key string, resp *pb.HTTPResponse) {
	item := struct {
		Resp      *pb.HTTPResponse `json:"resp"`
		ExpiresAt time.Time        `json:"expiresAt"`
	}{
		Resp:      resp,
		ExpiresAt: time.Now().Add(dc.ttl),
	}
	data, err := json.Marshal(item)
	if err != nil {
		logError("Error marshaling cache item: %v", err)
		return
	}
	temp, err := os.CreateTemp(dc.dir, ".harp-cache-*.tmp")
	if err != nil {
		logError("Error creating temporary cache file: %v", err)
		return
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		logError("Error writing temporary cache file: %v", err)
		return
	}
	if err := temp.Close(); err != nil {
		logError("Error closing temporary cache file: %v", err)
		return
	}
	if err := os.Rename(tempName, dc.cacheFile(key)); err != nil {
		logError("Error committing cache file: %v", err)
	}
}

func configuredCacheTTL() time.Duration {
	const defaultTTL = 30 * time.Minute
	ttl, err := time.ParseDuration(config.CacheTTL)
	if err != nil || ttl <= 0 {
		return defaultTTL
	}
	return ttl
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
const rateLimiterShardCount = 64

type RateLimiter struct {
	shards  [rateLimiterShardCount]rateLimiterShard
	limit   int
	now     func() time.Time
	seed    maphash.Seed
	enabled bool
}

type rateLimiterShard struct {
	mu       sync.Mutex
	requests map[string]*rateWindow
}

type rateWindow struct {
	timestamps []time.Time
	// next is the oldest timestamp and therefore the next slot replaced once
	// the bounded window reaches the configured request limit.
	next int
}

func NewRateLimiter(limit int) *RateLimiter {
	rl := &RateLimiter{
		limit:   limit,
		now:     time.Now,
		enabled: config.EnableRateLimit && limit > 0,
	}
	if !rl.enabled {
		return rl
	}
	rl.seed = maphash.MakeSeed()
	for i := range rl.shards {
		rl.shards[i].requests = make(map[string]*rateWindow)
	}
	return rl
}

func (rl *RateLimiter) Allow(clientIP string) bool {
	if !rl.enabled {
		return true
	}

	now := rl.now()
	shard := rl.shard(clientIP)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	window := shard.requests[clientIP]
	if window == nil {
		window = &rateWindow{timestamps: make([]time.Time, 0, min(rl.limit, 64))}
		shard.requests[clientIP] = window
	}

	if len(window.timestamps) < rl.limit {
		window.timestamps = append(window.timestamps, now)
		return true
	}
	if now.Sub(window.timestamps[window.next]) < time.Second {
		return false
	}

	window.timestamps[window.next] = now
	window.next = (window.next + 1) % rl.limit

	return true
}

// Cleanup removes stale entries from the rate limiter to prevent memory leaks.
func (rl *RateLimiter) Cleanup() {
	if !rl.enabled {
		return
	}
	now := rl.now()
	for i := range rl.shards {
		shard := &rl.shards[i]
		shard.mu.Lock()
		for ip, window := range shard.requests {
			if window == nil || len(window.timestamps) == 0 {
				delete(shard.requests, ip)
				continue
			}
			latest := len(window.timestamps) - 1
			if len(window.timestamps) == rl.limit {
				latest = (window.next + len(window.timestamps) - 1) % len(window.timestamps)
			}
			if now.Sub(window.timestamps[latest]) >= time.Second {
				delete(shard.requests, ip)
			}
		}
		shard.mu.Unlock()
	}
}

func (rl *RateLimiter) shard(clientIP string) *rateLimiterShard {
	index := maphash.String(rl.seed, clientIP) & (rateLimiterShardCount - 1)
	return &rl.shards[index]
}

func (rl *RateLimiter) trackedClients() int {
	total := 0
	for i := range rl.shards {
		shard := &rl.shards[i]
		shard.mu.Lock()
		total += len(shard.requests)
		shard.mu.Unlock()
	}
	return total
}

func (rl *RateLimiter) windowSize(clientIP string) int {
	if !rl.enabled {
		return 0
	}
	shard := rl.shard(clientIP)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if window := shard.requests[clientIP]; window != nil {
		return len(window.timestamps)
	}
	return 0
}

var rateLimiter *RateLimiter

// getCacheKey computes a key for caching.
func getCacheKey(method, url string, headers map[string]string, body string) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte(url))
	type cacheHeader struct {
		name  string
		value string
	}
	cacheHeaders := make([]cacheHeader, 0, len(headers))
	for key, value := range headers {
		normalizedKey := strings.ToLower(key)
		if isVolatileCacheHeader(normalizedKey) {
			continue
		}
		cacheHeaders = append(cacheHeaders, cacheHeader{name: normalizedKey, value: value})
	}
	slices.SortFunc(cacheHeaders, func(a, b cacheHeader) int {
		if cmp := strings.Compare(a.name, b.name); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.value, b.value)
	})
	for _, header := range cacheHeaders {
		h.Write([]byte(header.name))
		h.Write([]byte(header.value))
	}
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}

// isVolatileCacheHeader identifies proxy-generated request metadata that does
// not change the representation returned by a backend. Including these fields
// would create a unique cache entry per request or client address.
func isVolatileCacheHeader(name string) bool {
	switch name {
	case "forwarded", "via", "x-forwarded-for", "x-request-id":
		return true
	default:
		return false
	}
}

// --- Global registries for backends and pending responses ---
type backendConn struct {
	stream pb.HarpService_ProxyServer
	routes []registeredRoute
	mu     sync.Mutex // protects stream writes
}

type backendPool struct {
	route registeredRoute
	conns []*backendConn
	next  uint64
}

type registeredRoute struct {
	name     string
	pattern  *regexp.Regexp
	domain   string
	path     string
	username string
	password string
}

type routeSnapshot struct {
	Name      string `json:"name"`
	Domain    string `json:"domain"`
	Path      string `json:"path"`
	Protected bool   `json:"protected"`
	Backends  int    `json:"backends"`
}

const pendingStoreShardCount = 64

type shardedStore[V any] struct {
	shards [pendingStoreShardCount]shardedStoreShard[V]
	seed   maphash.Seed
}

type shardedStoreShard[V any] struct {
	mu    sync.RWMutex
	items map[string]V
}

func newShardedStore[V any]() *shardedStore[V] {
	store := &shardedStore[V]{seed: maphash.MakeSeed()}
	for i := range store.shards {
		store.shards[i].items = make(map[string]V)
	}
	return store
}

func (s *shardedStore[V]) Set(key string, value V) {
	shard := s.shard(key)
	shard.mu.Lock()
	shard.items[key] = value
	shard.mu.Unlock()
}

func (s *shardedStore[V]) Get(key string) (V, bool) {
	shard := s.shard(key)
	shard.mu.RLock()
	value, ok := shard.items[key]
	shard.mu.RUnlock()
	return value, ok
}

func (s *shardedStore[V]) Delete(key string) {
	shard := s.shard(key)
	shard.mu.Lock()
	delete(shard.items, key)
	shard.mu.Unlock()
}

func (s *shardedStore[V]) shard(key string) *shardedStoreShard[V] {
	index := maphash.String(s.seed, key) & (pendingStoreShardCount - 1)
	return &s.shards[index]
}

var (
	backendsMu sync.RWMutex
	// Map route key to a pool of backend connections serving that route.
	backends     = make(map[string]*backendPool)
	backendIndex []*backendPool
	// Pending responses keyed by request ID.
	pendingResponses  = newShardedStore[chan *pb.HTTPResponse]()
	pendingWebSockets = newShardedStore[*proxyWebSocketTunnel]()
	startTime         time.Time
	requestSem        chan struct{}
	servers           runningServerRegistry
	shuttingDown      atomic.Bool
)

type runningServerRegistry struct {
	mu    sync.Mutex
	http  []*http.Server
	http3 []*http3.Server
	grpc  []*grpc.Server
}

const (
	// Small burst buffer for streamed chunks to reduce head-of-line blocking
	// on backend receive loops while keeping per-request memory bounded.
	responseChannelBufferSize = 8
)

func healthSnapshot(status, check string) map[string]interface{} {
	backendsMu.RLock()
	backendCount := len(backends)
	backendsMu.RUnlock()

	uptime := 0.0
	if !startTime.IsZero() {
		uptime = time.Since(startTime).Seconds()
	}

	return map[string]interface{}{
		"status":    status,
		"check":     check,
		"timestamp": time.Now().Unix(),
		"backends":  backendCount,
		"uptime":    uptime,
	}
}

func writeHealthJSON(w http.ResponseWriter, statusCode int, payload map[string]interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		logError("Error writing health response: %v", err)
	}
}

// Health check endpoint kept for backwards compatibility.
func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	writeHealthJSON(w, http.StatusOK, healthSnapshot("healthy", "healthz"))
}

func livezHandler(w http.ResponseWriter, r *http.Request) {
	writeHealthJSON(w, http.StatusOK, healthSnapshot("alive", "livez"))
}

func readyzHandler(w http.ResponseWriter, r *http.Request) {
	if startTime.IsZero() {
		writeHealthJSON(w, http.StatusServiceUnavailable, healthSnapshot("not_ready", "readyz"))
		return
	}
	writeHealthJSON(w, http.StatusOK, healthSnapshot("ready", "readyz"))
}

func registerHealthHandlers(mux *http.ServeMux) {
	if !config.EnableHealthCheck {
		return
	}
	healthPath := config.HealthCheckPath
	if healthPath == "" {
		healthPath = "/health"
	}
	mux.HandleFunc(healthPath, healthCheckHandler)
	if healthPath != "/healthz" {
		mux.HandleFunc("/healthz", healthCheckHandler)
	}
	mux.HandleFunc("/livez", livezHandler)
	mux.HandleFunc("/readyz", readyzHandler)
}

func registeredRoutesSnapshot() []routeSnapshot {
	backendsMu.RLock()
	defer backendsMu.RUnlock()

	routes := make([]routeSnapshot, 0, len(backends))
	for _, pool := range backends {
		route := pool.route
		routes = append(routes, routeSnapshot{
			Name:      route.name,
			Domain:    route.domain,
			Path:      route.path,
			Protected: route.password != "",
			Backends:  len(pool.conns),
		})
	}
	slices.SortFunc(routes, func(a, b routeSnapshot) int {
		if cmp := strings.Compare(a.Domain, b.Domain); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Path, b.Path)
	})
	return routes
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
		"rate_limited":        metrics.RateLimited.Value(),
		"memory_stats": map[string]interface{}{
			"alloc":       memStats.Alloc,
			"total_alloc": memStats.TotalAlloc,
			"sys":         memStats.Sys,
			"num_gc":      memStats.NumGC,
		},
		"runtime_scheduler": runtimeMetricSnapshot(schedulerMetricNames),
	}

	if cacheStore != nil {
		stats["cache_stats"] = map[string]interface{}{
			"size": cacheStore.Size(),
		}
	}

	json.NewEncoder(w).Encode(stats)
}

func adminStatusHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdminAccess(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")

	var cacheSize int
	if cacheStore != nil {
		cacheSize = cacheStore.Size()
	}

	resp := map[string]interface{}{
		"status": "healthy",
		"uptime": time.Since(startTime).Seconds(),
		"proxy": map[string]interface{}{
			"grpcPort":              config.GRPCPort,
			"httpPort":              config.HTTPPort,
			"cacheEnabled":          config.EnableCache,
			"cacheType":             config.CacheType,
			"rateLimit":             config.EnableRateLimit,
			"maxConcurrent":         config.MaxConcurrentRequests,
			"loadBalancingStrategy": config.LoadBalancingStrategy,
		},
		"admin": map[string]interface{}{
			"path":              adminPath(),
			"authRequired":      !config.AdminInsecureSkipAuth,
			"networkRestricted": len(adminAllowedPrefixes) > 0,
			"allowedCIDRs":      append([]string(nil), config.AdminAllowedCIDRs...),
		},
		"routes": registeredRoutesSnapshot(),
		"counters": map[string]interface{}{
			"requestsTotal":      metrics.RequestsTotal.Value(),
			"cacheHits":          metrics.CacheHits.Value(),
			"cacheMisses":        metrics.CacheMisses.Value(),
			"backendErrors":      metrics.BackendErrors.Value(),
			"activeConnections":  metrics.ActiveConnections.Value(),
			"backendsRegistered": metrics.BackendsRegistered.Value(),
			"rateLimited":        metrics.RateLimited.Value(),
			"cacheSize":          cacheSize,
		},
		"runtimeScheduler": runtimeMetricSnapshot(schedulerMetricNames),
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logError("Error writing admin status: %v", err)
	}
}

func adminUIHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdminAccess(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, adminHTML(adminPath()))
}

func requireAdminAccess(w http.ResponseWriter, r *http.Request) bool {
	if !adminClientAllowed(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	if config.AdminInsecureSkipAuth {
		return true
	}
	if config.AdminPassword == "" {
		http.Error(w, "Admin authentication is not configured", http.StatusForbidden)
		return false
	}
	return requireBasicAuth(w, r, adminUsername(), config.AdminPassword, "HARP Admin")
}

func adminClientAllowed(r *http.Request) bool {
	if len(adminAllowedPrefixes) == 0 {
		return true
	}
	clientIP := clientIPFromRemoteAddr(r.RemoteAddr)
	addr, err := netip.ParseAddr(clientIP)
	if err != nil {
		return false
	}
	for _, prefix := range adminAllowedPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func adminHTML(path string) string {
	return strings.ReplaceAll(adminHTMLTemplate, "__ADMIN_API__", path+"/api/status")
}

func adminPath() string {
	path := config.AdminPath
	if path == "" {
		path = "/admin"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "/admin"
	}
	return path
}

func adminUsername() string {
	if config.AdminUsername == "" {
		return "admin"
	}
	return config.AdminUsername
}

func registerAdminHandlers(mux *http.ServeMux) {
	if !config.EnableAdminUI {
		return
	}
	path := adminPath()
	mux.HandleFunc(path, adminUIHandler)
	mux.HandleFunc(path+"/", adminUIHandler)
	mux.HandleFunc(path+"/api/status", adminStatusHandler)
}

//go:embed admin.html
var adminHTMLTemplate string

func runtimeMetricSnapshot(names []string) map[string]uint64 {
	samples := make([]runtimemetrics.Sample, len(names))
	for i, name := range names {
		samples[i].Name = name
	}
	runtimemetrics.Read(samples)

	out := make(map[string]uint64, len(samples))
	for _, sample := range samples {
		if sample.Value.Kind() == runtimemetrics.KindUint64 {
			out[sample.Name] = sample.Value.Uint64()
		}
	}
	return out
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
		if _, ok := allowedRegistrationFor(r.Path, reg.Key); ok {
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
		routeDomain := r.Domain
		if routeDomain == "" {
			routeDomain = reg.Domain
		}
		patternStr := routeDomain + r.Path
		pattern, err := regexp.Compile(patternStr)
		if err != nil {
			logError("Error compiling regexp for route %s: %v", r.Path, err)
			continue
		}
		authRule, _ := allowedRegistrationFor(r.Path, reg.Key)
		conn.routes = append(conn.routes, registeredRoute{
			name:     r.Name,
			pattern:  pattern,
			domain:   routeDomain,
			path:     r.Path,
			username: authRule.username,
			password: authRule.password,
		})
		backendsMu.Lock()
		addBackendToPool(backendKey(routeDomain, r.Path), conn.routes[len(conn.routes)-1], conn)
		backendsMu.Unlock()
		logDebug("Registered route: %s%s", routeDomain, r.Path)
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
					removeBackendFromPool(backendKey(route.domain, route.path), conn)
				}
				backendsMu.Unlock()
				metrics.BackendsRegistered.Add(-1)
				return
			}
			httpResp := clientMsg.GetHttpResponse()
			if httpResp == nil {
				wsData := clientMsg.GetWebsocketData()
				if wsData == nil {
					continue
				}
				tunnel, ok := pendingWebSockets.Get(wsData.RequestId)
				if ok {
					deliverPendingWebSocket(stream.Context(), wsData, tunnel)
				} else {
					logDebug("No pending WebSocket for request ID %s", wsData.RequestId)
				}
				continue
			}
			ch, ok := pendingResponses.Get(httpResp.RequestId)
			if ok {
				deliverPendingResponse(stream.Context(), httpResp, ch)
			} else {
				logDebug("No pending request for response ID %s", httpResp.RequestId)
			}
		}
	}()

	<-stream.Context().Done()
	logInfo("gRPC stream closed for backend: %s", reg.Name)
	return nil
}

func backendKey(domain, path string) string {
	return domain + "\x00" + path
}

func addBackendToPool(key string, route registeredRoute, conn *backendConn) {
	pool, ok := backends[key]
	if !ok {
		pool = &backendPool{
			route: route,
			conns: []*backendConn{conn},
		}
		backends[key] = pool
		backendIndex = append(backendIndex, pool)
		sortBackendIndex(backendIndex)
		return
	}
	for _, existing := range pool.conns {
		if existing == conn {
			return
		}
	}
	pool.conns = append(pool.conns, conn)
}

func removeBackendFromPool(key string, conn *backendConn) {
	pool, ok := backends[key]
	if !ok {
		return
	}
	for i, existing := range pool.conns {
		if existing == conn {
			pool.conns = append(pool.conns[:i], pool.conns[i+1:]...)
			break
		}
	}
	if len(pool.conns) == 0 {
		delete(backends, key)
		for i, indexedPool := range backendIndex {
			if indexedPool == pool {
				copy(backendIndex[i:], backendIndex[i+1:])
				backendIndex[len(backendIndex)-1] = nil
				backendIndex = backendIndex[:len(backendIndex)-1]
				break
			}
		}
	}
}

func sortBackendIndex(pools []*backendPool) {
	slices.SortStableFunc(pools, func(a, b *backendPool) int {
		return len(b.route.path) - len(a.route.path)
	})
}

func compileAllowedRegistrationRules(rules []AllowedRegistrationRule) ([]compiledAllowedRegistrationRule, error) {
	compiled := make([]compiledAllowedRegistrationRule, 0, len(rules))
	for _, rule := range rules {
		pattern, err := regexp.Compile(rule.Route)
		if err != nil {
			return nil, fmt.Errorf("invalid regex in allowedRegistration route %q: %w", rule.Route, err)
		}
		compiled = append(compiled, compiledAllowedRegistrationRule{
			pattern:  pattern,
			route:    rule.Route,
			key:      rule.Key,
			username: rule.Username,
			password: rule.Password,
		})
	}
	slices.SortStableFunc(compiled, func(a, b compiledAllowedRegistrationRule) int {
		return len(b.route) - len(a.route)
	})
	return compiled, nil
}

func allowedRegistrationFor(path, key string) (compiledAllowedRegistrationRule, bool) {
	rules := allowedRegistrationRules
	if len(rules) == 0 && len(config.AllowedRegistration) > 0 {
		var err error
		rules, err = compileAllowedRegistrationRules(config.AllowedRegistration)
		if err != nil {
			logError("Error compiling allowed registration rules: %v", err)
			return compiledAllowedRegistrationRule{}, false
		}
	}
	for _, rule := range rules {
		if key == rule.key && rule.pattern.MatchString(path) {
			return rule, true
		}
	}
	return compiledAllowedRegistrationRule{}, false
}

func isRegistrationAllowed(path, key string) bool {
	_, ok := allowedRegistrationFor(path, key)
	return ok
}

func deliverPendingResponse(ctx context.Context, resp *pb.HTTPResponse, ch chan<- *pb.HTTPResponse) bool {
	if responseHeaderEnabled(resp, pb.StreamHeader) {
		select {
		case ch <- resp:
			return true
		case <-ctx.Done():
			return false
		}
	}
	select {
	case ch <- resp:
		return true
	case <-ctx.Done():
		return false
	default:
		logWarn("Dropping response for request %s: response channel full", resp.RequestId)
		if metrics != nil {
			metrics.BackendErrors.Add(1)
		}
		return false
	}
}

func deliverPendingWebSocket(ctx context.Context, data *pb.WebSocketData, tunnel *proxyWebSocketTunnel) bool {
	select {
	case tunnel.ch <- data:
		return true
	case <-tunnel.done:
		return false
	case <-ctx.Done():
		return false
	}
}

// --- HTTP Handler for Client Requests ---
func httpHandler(w http.ResponseWriter, r *http.Request) {
	if requestSem != nil {
		select {
		case requestSem <- struct{}{}:
			defer func() { <-requestSem }()
		default:
			http.Error(w, "Server busy", http.StatusServiceUnavailable)
			return
		}
	}

	start := time.Now()
	metricRoute := "_unmatched"
	defer func() {
		duration := time.Since(start)
		metrics.RequestsTotal.Add(1)
		metrics.RequestDuration.Add(metricRoute, int64(duration/time.Millisecond))
	}()

	// CORS handling
	if config.EnableCORS {
		origin := config.CORSAllowedOrigins
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, PATCH")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	// Rate limiting
	clientIP := clientIPFromRemoteAddr(r.RemoteAddr)
	if !rateLimiter.Allow(clientIP) {
		metrics.RateLimited.Add(1)
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	// Find the best matching backend (longest path wins).
	chosen, matchedRoute, routeAuth := matchBackend(r.Host, r.URL.Path)

	if matchedRoute != "" {
		metricRoute = matchedRoute
		metrics.RouteRequests.Add(matchedRoute, 1)
	}

	if chosen == nil {
		logWarn("No matching route for %s", r.URL.String())
		http.NotFound(w, r)
		return
	}

	authenticatedRoute := false
	if routeAuth.password != "" {
		if !requireBasicAuth(w, r, routeAuth.username, routeAuth.password, "HARP Route") {
			return
		}
		authenticatedRoute = true
	}

	// Enforce request body size limit
	var bodyReader io.Reader = r.Body
	if config.MaxRequestBodySize > 0 {
		bodyReader = io.LimitReader(r.Body, config.MaxRequestBodySize+1)
	}
	bodyBytes, err := io.ReadAll(bodyReader)
	if err != nil {
		http.Error(w, "Error reading body", http.StatusBadRequest)
		return
	}
	r.Body.Close()
	if config.MaxRequestBodySize > 0 && int64(len(bodyBytes)) > config.MaxRequestBodySize {
		http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	bodyStr := string(bodyBytes)

	reqID := uuid.New().String()
	edgeRequestID := requestIDFromRequest(r)
	w.Header().Set("X-Request-ID", edgeRequestID)
	isWS := isWebSocketUpgrade(r)
	forwardHeaders := forwardedRequestHeaders(r)
	if isWS {
		forwardHeaders = websocketForwardedRequestHeaders(r)
	}
	setHTTPHeaderDefault(forwardHeaders, "X-Request-ID", edgeRequestID)
	headers := pb.HeaderMapFromHTTP(forwardHeaders)
	headerValues := pb.HeaderValuesFromHTTP(forwardHeaders)
	if authenticatedRoute {
		delete(headers, "Authorization")
		headerValues = removeHeaderValues(headerValues, "Authorization")
	}

	if isWS {
		handleWebSocket(w, r, chosen, headers, headerValues)
		return
	}

	cacheEnabled := config.EnableCache && cacheStore != nil && r.Method == http.MethodGet
	var cacheKey string
	if cacheEnabled {
		cacheKey = getCacheKey(r.Method, r.URL.String(), headers, bodyStr)
		if resp, ok := cacheStore.Get(cacheKey); ok {
			logDebug("Cache hit for %s", r.URL.String())
			metrics.CacheHits.Add(1)
			respHeaders := filterInternalHTTPHeaders(pb.HTTPHeaderFromProto(resp.Headers, resp.HeaderValues))
			appendVia(respHeaders)
			copyHTTPHeaders(w.Header(), respHeaders)
			w.WriteHeader(int(resp.Status))
			if body := pb.BodyBytesFromProto(resp.Body, resp.BodyBytes); len(body) > 0 {
				_, _ = w.Write(body)
			}
			return
		}
		metrics.CacheMisses.Add(1)
	}

	// Build an HTTPRequest message.
	httpReq := &pb.HTTPRequest{
		Method:       r.Method,
		Url:          r.URL.String(),
		Headers:      headers,
		HeaderValues: headerValues,
		Body:         bodyStr,
		BodyBytes:    append([]byte(nil), bodyBytes...),
		RequestId:    reqID,
		Timestamp:    time.Now().UnixNano(),
	}

	// Prepare channel for response.
	respCh := make(chan *pb.HTTPResponse, responseChannelBufferSize)
	pendingResponses.Set(reqID, respCh)
	defer func() {
		pendingResponses.Delete(reqID)
	}()

	// Send the HTTPRequest to the chosen backend.
	serverMsg := &pb.ServerMessage{Payload: &pb.ServerMessage_HttpRequest{HttpRequest: httpReq}}
	chosen.mu.Lock()
	err = chosen.stream.Send(serverMsg)
	chosen.mu.Unlock()
	if err != nil {
		http.Error(w, "Error forwarding request", http.StatusBadGateway)
		logError("Error sending to backend: %v", err)
		metrics.BackendErrors.Add(1)
		return
	}

	// Wait for the response while releasing resources promptly when the client
	// disconnects. Reuse one timer across streaming chunks to avoid retaining a
	// new time.After timer for every chunk.
	timeout := configuredRequestTimeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	flusher, canFlush := w.(http.Flusher)

	select {
	case resp := <-respCh:
		isStream := responseHeaderEnabled(resp, pb.StreamHeader)
		firstStatus := int(resp.Status)
		firstHTTPHeaders := filterInternalHTTPHeaders(pb.HTTPHeaderFromProto(resp.Headers, resp.HeaderValues))
		appendVia(firstHTTPHeaders)
		if isStream {
			applyStreamDefaultsHTTP(firstHTTPHeaders, streamTypeFromResponse(resp))
		}
		firstHeaders := pb.HeaderMapFromHTTP(firstHTTPHeaders)
		var bodyBuilder strings.Builder
		var bodyBytesBuilder []byte
		wroteHeaders := false
		for {
			if !wroteHeaders {
				copyHTTPHeaders(w.Header(), firstHTTPHeaders)
				w.WriteHeader(firstStatus)
				wroteHeaders = true
			}

			respBody := pb.BodyBytesFromProto(resp.Body, resp.BodyBytes)
			if len(respBody) > 0 {
				_, _ = w.Write(respBody)
				if !isStream {
					bodyBuilder.WriteString(string(respBody))
					bodyBytesBuilder = append(bodyBytesBuilder, respBody...)
				}
				if isStream && canFlush {
					flusher.Flush()
				}
			}

			if !isStream || responseHeaderEnabled(resp, pb.StreamEndHeader) {
				if cacheEnabled && !isStream {
					cacheStore.Set(cacheKey, &pb.HTTPResponse{
						Status:       int32(firstStatus),
						Headers:      firstHeaders,
						HeaderValues: pb.HeaderValuesFromHTTP(firstHTTPHeaders),
						Body:         bodyBuilder.String(),
						BodyBytes:    bodyBytesBuilder,
					})
				}
				return
			}

			resetTimer(timer, timeout)
			select {
			case resp = <-respCh:
			case <-timer.C:
				metrics.BackendErrors.Add(1)
				logWarn("Stream timeout for request %s", reqID)
				return
			case <-r.Context().Done():
				return
			}
		}
	case <-timer.C:
		http.Error(w, "Timeout waiting for backend", http.StatusGatewayTimeout)
		metrics.BackendErrors.Add(1)
	case <-r.Context().Done():
		return
	}
}

func configuredRequestTimeout() time.Duration {
	const defaultTimeout = 30 * time.Second
	if config.RequestTimeout == "" {
		return defaultTimeout
	}
	timeout, err := time.ParseDuration(config.RequestTimeout)
	if err != nil {
		return defaultTimeout
	}
	return timeout
}

func resetTimer(timer *time.Timer, timeout time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(timeout)
}

type routeAuthConfig struct {
	username string
	password string
}

func matchBackend(host, path string) (*backendConn, string, routeAuthConfig) {
	requestRoute := routeTarget(host, path)

	backendsMu.RLock()
	defer backendsMu.RUnlock()
	pools := backendIndex
	// Tests and embedders may replace the legacy map directly. Reconstruct a
	// local ordered view in that exceptional case; production updates maintain
	// backendIndex incrementally during registration and disconnection.
	if len(pools) != len(backends) {
		pools = make([]*backendPool, 0, len(backends))
		for _, pool := range backends {
			pools = append(pools, pool)
		}
		sortBackendIndex(pools)
	}
	for _, pool := range pools {
		route := pool.route
		if len(pool.conns) == 0 || !matchesRegisteredRoute(path, route.path) || !route.pattern.MatchString(requestRoute) {
			continue
		}
		auth := routeAuthConfig{username: route.username, password: route.password}
		if config.LoadBalancingStrategy == loadBalancingFirst {
			return pool.conns[0], route.path, auth
		}
		idx := atomic.AddUint64(&pool.next, 1) - 1
		return pool.conns[int(idx%uint64(len(pool.conns)))], route.path, auth
	}
	return nil, "", routeAuthConfig{}
}

func matchesRegisteredRoute(requestPath, routePath string) bool {
	if routePath == "/" {
		return strings.HasPrefix(requestPath, "/")
	}
	if !strings.HasPrefix(requestPath, routePath) {
		return false
	}
	return strings.HasSuffix(routePath, "/") || len(requestPath) == len(routePath) || requestPath[len(routePath)] == '/'
}

func routeTarget(host, path string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host + path
}

func clientIPFromRemoteAddr(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

func requireBasicAuth(w http.ResponseWriter, r *http.Request, username, password, realm string) bool {
	if password == "" {
		return true
	}
	if username == "" {
		username = "harp"
	}
	gotUser, gotPassword, ok := r.BasicAuth()
	if ok && secureCompare(gotUser, username) && secureCompare(gotPassword, password) {
		return true
	}
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm=%q, charset="UTF-8"`, realm))
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
	return false
}

func requestIDFromRequest(r *http.Request) string {
	for _, value := range r.Header.Values("X-Request-ID") {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return uuid.New().String()
}

func secureCompare(got, want string) bool {
	gotHash := sha256.Sum256([]byte(got))
	wantHash := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotHash[:], wantHash[:]) == 1
}

func headerEnabled(headers map[string]string, key string) bool {
	val := strings.TrimSpace(strings.ToLower(headerValue(headers, key)))
	return val == "1" || val == "true" || val == "yes"
}

func headerValue(headers map[string]string, key string) string {
	for k, v := range headers {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

func responseHeaderEnabled(resp *pb.HTTPResponse, key string) bool {
	val := strings.TrimSpace(strings.ToLower(protoHeaderValue(resp.Headers, resp.HeaderValues, key)))
	return val == "1" || val == "true" || val == "yes"
}

func protoHeaderValue(headers map[string]string, values []*pb.HTTPHeader, key string) string {
	for _, header := range values {
		if header != nil && strings.EqualFold(header.Name, key) && len(header.Values) > 0 {
			return header.Values[0]
		}
	}
	return headerValue(headers, key)
}

func streamType(headers map[string]string) string {
	switch strings.ToLower(strings.TrimSpace(headerValue(headers, pb.StreamTypeHeader))) {
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

func streamTypeFromResponse(resp *pb.HTTPResponse) string {
	switch strings.ToLower(strings.TrimSpace(protoHeaderValue(resp.Headers, resp.HeaderValues, pb.StreamTypeHeader))) {
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

func applyStreamDefaults(headers map[string]string, typ string) {
	deleteHeader(headers, "Content-Length")
	switch typ {
	case pb.StreamTypeSSE:
		setHeaderDefault(headers, "Content-Type", "text/event-stream")
		setHeaderDefault(headers, "Cache-Control", "no-cache")
		setHeaderDefault(headers, "X-Accel-Buffering", "no")
	case pb.StreamTypeNDJSON:
		setHeaderDefault(headers, "Content-Type", "application/x-ndjson")
	case pb.StreamTypeText:
		setHeaderDefault(headers, "Content-Type", "text/plain; charset=utf-8")
	}
}

func applyStreamDefaultsHTTP(headers http.Header, typ string) {
	deleteHTTPHeader(headers, "Content-Length")
	switch typ {
	case pb.StreamTypeSSE:
		setHTTPHeaderDefault(headers, "Content-Type", "text/event-stream")
		setHTTPHeaderDefault(headers, "Cache-Control", "no-cache")
		setHTTPHeaderDefault(headers, "X-Accel-Buffering", "no")
	case pb.StreamTypeNDJSON:
		setHTTPHeaderDefault(headers, "Content-Type", "application/x-ndjson")
	case pb.StreamTypeText:
		setHTTPHeaderDefault(headers, "Content-Type", "text/plain; charset=utf-8")
	}
}

func setHeaderDefault(headers map[string]string, key, value string) {
	if headerValue(headers, key) == "" {
		headers[key] = value
	}
}

func setHTTPHeaderDefault(headers http.Header, key, value string) {
	if headers.Get(key) == "" {
		headers.Set(key, value)
	}
}

func deleteHeader(headers map[string]string, key string) {
	for k := range headers {
		if strings.EqualFold(k, key) {
			delete(headers, k)
		}
	}
}

func deleteHTTPHeader(headers http.Header, key string) {
	for k := range headers {
		if strings.EqualFold(k, key) {
			delete(headers, k)
		}
	}
}

func filterInternalHeaders(headers map[string]string) map[string]string {
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		if strings.EqualFold(k, pb.StreamHeader) ||
			strings.EqualFold(k, pb.StreamEndHeader) ||
			strings.EqualFold(k, pb.StreamTypeHeader) {
			continue
		}
		out[k] = v
	}
	return out
}

func filterInternalHTTPHeaders(headers http.Header) http.Header {
	out := make(http.Header, len(headers))
	connectionTokens := hopByHopConnectionTokens(headers)
	for k, values := range headers {
		if strings.EqualFold(k, pb.StreamHeader) ||
			strings.EqualFold(k, pb.StreamEndHeader) ||
			strings.EqualFold(k, pb.StreamTypeHeader) ||
			isHopByHopHeader(k) ||
			connectionTokens[strings.ToLower(k)] {
			continue
		}
		out[k] = append([]string(nil), values...)
	}
	return out
}

func copyHTTPHeaders(dst, src http.Header) {
	for k, values := range src {
		dst.Del(k)
		for _, value := range values {
			dst.Add(k, value)
		}
	}
}

func removeHeaderValues(values []*pb.HTTPHeader, key string) []*pb.HTTPHeader {
	out := values[:0]
	for _, header := range values {
		if header == nil || strings.EqualFold(header.Name, key) {
			continue
		}
		out = append(out, header)
	}
	return out
}

func forwardedRequestHeaders(r *http.Request) http.Header {
	headers := removeHopByHopHeaders(r.Header)
	addForwardedHeaders(headers, r)
	return headers
}

func websocketForwardedRequestHeaders(r *http.Request) http.Header {
	headers := removeHopByHopHeaders(r.Header)
	copyHeaderIfPresent(headers, r.Header, "Connection")
	copyHeaderIfPresent(headers, r.Header, "Upgrade")
	addForwardedHeaders(headers, r)
	return headers
}

func addForwardedHeaders(headers http.Header, r *http.Request) {
	clientIP := clientIPFromRemoteAddr(r.RemoteAddr)
	if clientIP != "" {
		appendHeaderValue(headers, "X-Forwarded-For", clientIP)
	}
	if r.Host != "" {
		setHTTPHeaderDefault(headers, "X-Forwarded-Host", r.Host)
	}
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	setHTTPHeaderDefault(headers, "X-Forwarded-Proto", proto)
	if port := forwardedPort(r.Host, proto); port != "" {
		setHTTPHeaderDefault(headers, "X-Forwarded-Port", port)
	}
	appendForwardedHeader(headers, clientIP, proto, r.Host)
	appendVia(headers)
}

func copyHeaderIfPresent(dst, src http.Header, key string) {
	values := src.Values(key)
	if len(values) == 0 {
		return
	}
	dst[key] = append([]string(nil), values...)
}

func removeHopByHopHeaders(headers http.Header) http.Header {
	out := make(http.Header, len(headers))
	connectionTokens := hopByHopConnectionTokens(headers)
	for k, values := range headers {
		if isHopByHopHeader(k) || connectionTokens[strings.ToLower(k)] {
			continue
		}
		out[k] = append([]string(nil), values...)
	}
	return out
}

func hopByHopConnectionTokens(headers http.Header) map[string]bool {
	tokens := make(map[string]bool)
	for _, value := range headers.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			token = strings.ToLower(strings.TrimSpace(token))
			if token != "" {
				tokens[token] = true
			}
		}
	}
	return tokens
}

func isHopByHopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection",
		"keep-alive",
		"proxy-authenticate",
		"proxy-authorization",
		"te",
		"trailer",
		"transfer-encoding",
		"upgrade":
		return true
	default:
		return false
	}
}

func appendHeaderValue(headers http.Header, key, value string) {
	if value == "" {
		return
	}
	headers.Add(key, value)
}

func appendVia(headers http.Header) {
	headers.Add("Via", "1.1 harp")
}

func forwardedPort(host, proto string) string {
	if _, port, err := net.SplitHostPort(host); err == nil {
		return port
	}
	if strings.Contains(host, ":") {
		return ""
	}
	switch proto {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}

func appendForwardedHeader(headers http.Header, clientIP, proto, host string) {
	parts := make([]string, 0, 3)
	if clientIP != "" {
		parts = append(parts, "for="+quoteForwardedValue(clientIP))
	}
	if proto != "" {
		parts = append(parts, "proto="+quoteForwardedValue(proto))
	}
	if host != "" {
		parts = append(parts, "host="+quoteForwardedValue(host))
	}
	if len(parts) > 0 {
		headers.Add("Forwarded", strings.Join(parts, ";"))
	}
}

func quoteForwardedValue(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
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
	servers.mu.Lock()
	servers.grpc = append(servers.grpc, grpcServer)
	servers.mu.Unlock()
	lis, err := net.Listen("tcp", config.GRPCPort)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", config.GRPCPort, err)
	}
	logInfo("gRPC server listening on %s", config.GRPCPort)
	if err := grpcServer.Serve(lis); err != nil && !shuttingDown.Load() {
		log.Fatalf("gRPC server error: %v", err)
	}
}

func startHTTPServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", httpHandler)
	registerAdminHandlers(mux)
	registerHealthHandlers(mux)

	server := newConfiguredHTTPServer(config.HTTPPort, mux)
	registerHTTPServer(server)

	logInfo("HTTP server listening on %s", config.HTTPPort)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) && !shuttingDown.Load() {
		log.Fatalf("HTTP server error: %v", err)
	}
}

func startHTTPSServer() {
	if config.HTTPSCert == "" || config.HTTPSKey == "" {
		log.Fatal("https enabled but cert or key not provided")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", httpHandler)
	registerAdminHandlers(mux)
	registerHealthHandlers(mux)

	server := newConfiguredHTTPServer(config.HTTPPort, mux)
	server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	registerHTTPServer(server)
	logInfo("HTTPS server listening on %s", config.HTTPPort)
	if err := server.ListenAndServeTLS(config.HTTPSCert, config.HTTPSKey); err != nil && !errors.Is(err, http.ErrServerClosed) && !shuttingDown.Load() {
		log.Fatalf("HTTPS server error: %v", err)
	}
}

func newConfiguredHTTPServer(addr string, handler http.Handler) *http.Server {
	server := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  configuredDuration(config.ReadTimeout, 30*time.Second),
		WriteTimeout: configuredDuration(config.WriteTimeout, 30*time.Second),
		IdleTimeout:  configuredDuration(config.IdleTimeout, 120*time.Second),
	}
	if config.MaxHeaderSize > 0 {
		server.MaxHeaderBytes = config.MaxHeaderSize
	}
	return server
}

func configuredDuration(value string, fallback time.Duration) time.Duration {
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func startHTTP3Server() {
	if config.HTTPSCert == "" || config.HTTPSKey == "" {
		log.Fatal("http3 enabled but https cert or key not provided")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", httpHandler)
	registerAdminHandlers(mux)
	registerHealthHandlers(mux)

	server := &http3.Server{
		Addr:      config.HTTP3Port,
		Handler:   mux,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13},
	}
	servers.mu.Lock()
	servers.http3 = append(servers.http3, server)
	servers.mu.Unlock()
	logInfo("HTTP/3 server listening on %s", config.HTTP3Port)
	if err := server.ListenAndServeTLS(config.HTTPSCert, config.HTTPSKey); err != nil && !shuttingDown.Load() {
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
	registerHTTPServer(server)

	logInfo("Metrics server listening on %s", metricsPort)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) && !shuttingDown.Load() {
		logError("Metrics server error: %v", err)
	}
}

func registerHTTPServer(server *http.Server) {
	servers.mu.Lock()
	servers.http = append(servers.http, server)
	servers.mu.Unlock()
}

func shutdownRunningServers(ctx context.Context) {
	shuttingDown.Store(true)
	servers.mu.Lock()
	httpServers := append([]*http.Server(nil), servers.http...)
	http3Servers := append([]*http3.Server(nil), servers.http3...)
	grpcServers := append([]*grpc.Server(nil), servers.grpc...)
	servers.mu.Unlock()

	var wg sync.WaitGroup
	for _, server := range httpServers {
		wg.Go(func() {
			if err := server.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				logError("HTTP shutdown error: %v", err)
			}
		})
	}
	for _, server := range http3Servers {
		wg.Go(func() {
			if err := server.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				logError("HTTP/3 shutdown error: %v", err)
			}
		})
	}
	for _, server := range grpcServers {
		wg.Go(func() {
			done := make(chan struct{})
			go func() {
				server.GracefulStop()
				close(done)
			}()
			select {
			case <-done:
			case <-ctx.Done():
				server.Stop()
				<-done
			}
		})
	}
	wg.Wait()
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
	if config.MaxRequestBodySize == 0 {
		config.MaxRequestBodySize = 10 * 1024 * 1024 // 10MB default
	}
	if config.GracefulShutdownDelay == "" {
		config.GracefulShutdownDelay = "10s"
	}
	if strategy := normalizeLoadBalancingStrategy(config.LoadBalancingStrategy); strategy != "" {
		config.LoadBalancingStrategy = strategy
	}

	if err := validateConfig(config); err != nil {
		log.Fatalf("Config error: %v", err)
	}
	allowedRegistrationRules, err = compileAllowedRegistrationRules(config.AllowedRegistration)
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}
	adminAllowedPrefixes, err = compileAdminAllowedCIDRs(config.AdminAllowedCIDRs)
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}
}

func validateConfig(config Config) error {
	var errs []error

	if config.EnableGRPCTLS && (config.GRPCTLSCert == "" || config.GRPCTLSKey == "") {
		errs = append(errs, errors.New("enableGRPCTLS requires grpcTLSCert and grpcTLSKey"))
	}
	if config.EnableHTTPS && (config.HTTPSCert == "" || config.HTTPSKey == "") {
		errs = append(errs, errors.New("enableHTTPS requires httpsCert and httpsKey"))
	}
	if config.EnableHTTP3 && !config.EnableHTTPS {
		errs = append(errs, errors.New("enableHTTP3 requires enableHTTPS"))
	}
	if normalizeLoadBalancingStrategy(config.LoadBalancingStrategy) == "" {
		errs = append(errs, fmt.Errorf("invalid loadBalancingStrategy %q: use %q or %q", config.LoadBalancingStrategy, loadBalancingRoundRobin, loadBalancingFirst))
	}
	if config.EnableAdminUI && !config.AdminInsecureSkipAuth && config.AdminPassword == "" {
		errs = append(errs, errors.New("enableAdminUI requires adminPassword unless adminInsecureSkipAuth is true"))
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"cacheTTL", config.CacheTTL},
		{"requestTimeout", config.RequestTimeout},
		{"readTimeout", config.ReadTimeout},
		{"writeTimeout", config.WriteTimeout},
		{"idleTimeout", config.IdleTimeout},
		{"gracefulShutdownDelay", config.GracefulShutdownDelay},
	} {
		if field.value == "" {
			continue
		}
		if _, err := time.ParseDuration(field.value); err != nil {
			errs = append(errs, fmt.Errorf("invalid %s %q: %w", field.name, field.value, err))
		}
	}
	if _, err := compileAllowedRegistrationRules(config.AllowedRegistration); err != nil {
		errs = append(errs, err)
	}
	if _, err := compileAdminAllowedCIDRs(config.AdminAllowedCIDRs); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func normalizeLoadBalancingStrategy(strategy string) string {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "", loadBalancingRoundRobin, "round-robin", "roundrobin":
		return loadBalancingRoundRobin
	case loadBalancingFirst:
		return loadBalancingFirst
	default:
		return ""
	}
}

func compileAdminAllowedCIDRs(values []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if strings.Contains(value, "/") {
			prefix, err := netip.ParsePrefix(value)
			if err != nil {
				return nil, fmt.Errorf("invalid adminAllowedCIDRs entry %q: %w", value, err)
			}
			prefixes = append(prefixes, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(value)
		if err != nil {
			return nil, fmt.Errorf("invalid adminAllowedCIDRs entry %q: %w", value, err)
		}
		prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return prefixes, nil
}

func main() {
	configPath := flag.String("config", "config.json", "Path to configuration file")
	flag.Parse()

	startTime = time.Now()
	loadConfig(*configPath)
	logInfo("Enhanced HARP proxy starting with configuration: %s", *configPath)
	if config.MaxConcurrentRequests > 0 {
		requestSem = make(chan struct{}, config.MaxConcurrentRequests)
	}

	// Always initialize metrics (used in httpHandler unconditionally)
	initMetrics()

	// Initialize rate limiter
	rateLimiter = NewRateLimiter(config.RateLimitPerSecond)

	// Start periodic rate limiter cleanup
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			rateLimiter.Cleanup()
		}
	}()

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
		go startHTTPSServer()
	} else {
		go startHTTPServer()
	}

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logInfo("Shutdown signal received, draining connections...")

	shutdownDelay := 10 * time.Second
	if d, err := time.ParseDuration(config.GracefulShutdownDelay); err == nil {
		shutdownDelay = d
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownDelay)
	defer cancel()

	shutdownRunningServers(ctx)
	logInfo("HARP proxy stopped")
}
