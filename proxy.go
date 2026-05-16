// Description: A simple HTTP reverse proxy that forwards requests to registered backends.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"expvar"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	runtimemetrics "runtime/metrics"
	"slices"
	"strings"
	"sync"
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
	MaxRequestBodySize    int64  `json:"maxRequestBodySize"`
	EnableCORS            bool   `json:"enableCORS"`
	CORSAllowedOrigins    string `json:"corsAllowedOrigins"`
	GracefulShutdownDelay string `json:"gracefulShutdownDelay"`
	EnableAdminUI         bool   `json:"enableAdminUI"`
	AdminPath             string `json:"adminPath"`
}

type AllowedRegistrationRule struct {
	Route string `json:"route"`
	Key   string `json:"key"`
}

type compiledAllowedRegistrationRule struct {
	pattern *regexp.Regexp
	key     string
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
	if err := os.MkdirAll(dir, 0755); err != nil {
		logError("Error creating disk cache directory %s: %v", dir, err)
	}
	return &DiskCache{dir: dir}
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
	if err := os.WriteFile(filename, data, 0644); err != nil {
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
	if !config.EnableRateLimit || rl.limit <= 0 {
		return true
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	requests := rl.requests[clientIP]

	// Remove old requests (older than 1 second)
	validRequests := requests[:0]
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

// Cleanup removes stale entries from the rate limiter to prevent memory leaks.
func (rl *RateLimiter) Cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	for ip, requests := range rl.requests {
		valid := requests[:0]
		for _, t := range requests {
			if now.Sub(t) < time.Second {
				valid = append(valid, t)
			}
		}
		if len(valid) == 0 {
			delete(rl.requests, ip)
		} else {
			rl.requests[ip] = valid
		}
	}
}

var rateLimiter *RateLimiter

// getCacheKey computes a key for caching.
func getCacheKey(method, url string, headers map[string]string, body string) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte(url))
	keys := slices.Sorted(maps.Keys(headers))
	for _, k := range keys {
		v := headers[k]
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

type routeSnapshot struct {
	Name   string `json:"name"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
}

var (
	backendsMu sync.RWMutex
	// Map route path to backend connection.
	backends = make(map[string]*backendConn)
	// Pending responses keyed by request ID.
	pendingMu       sync.RWMutex
	pendingResponse = make(map[string]chan *pb.HTTPResponse)
	startTime       time.Time
	requestSem      chan struct{}
)

const (
	// Small burst buffer for streamed chunks to reduce head-of-line blocking
	// on backend receive loops while keeping per-request memory bounded.
	responseChannelBufferSize = 8
)

// Health check endpoint
func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	backendsMu.RLock()
	backendCount := len(backends)
	backendsMu.RUnlock()

	health := map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now().Unix(),
		"backends":  backendCount,
		"uptime":    time.Since(startTime).Seconds(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(health)
}

func registeredRoutesSnapshot() []routeSnapshot {
	backendsMu.RLock()
	defer backendsMu.RUnlock()

	routes := make([]routeSnapshot, 0, len(backends))
	seen := make(map[string]struct{}, len(backends))
	for _, conn := range backends {
		for _, route := range conn.routes {
			key := backendKey(route.domain, route.path)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			routes = append(routes, routeSnapshot{
				Name:   route.name,
				Domain: route.domain,
				Path:   route.path,
			})
		}
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
	w.Header().Set("Content-Type", "application/json")

	var cacheSize int
	if cacheStore != nil {
		cacheSize = cacheStore.Size()
	}

	resp := map[string]interface{}{
		"status": "healthy",
		"uptime": time.Since(startTime).Seconds(),
		"proxy": map[string]interface{}{
			"grpcPort":      config.GRPCPort,
			"httpPort":      config.HTTPPort,
			"cacheEnabled":  config.EnableCache,
			"cacheType":     config.CacheType,
			"rateLimit":     config.EnableRateLimit,
			"maxConcurrent": config.MaxConcurrentRequests,
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, adminHTML(adminPath()))
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

func registerAdminHandlers(mux *http.ServeMux) {
	if !config.EnableAdminUI {
		return
	}
	path := adminPath()
	mux.HandleFunc(path, adminUIHandler)
	mux.HandleFunc(path+"/", adminUIHandler)
	mux.HandleFunc(path+"/api/status", adminStatusHandler)
}

const adminHTMLTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>HARP Admin</title>
<style>
:root {
  --bg: #f4f1ea;
  --ink: #18201c;
  --muted: #69746e;
  --line: #d9d3c6;
  --panel: #fffdf8;
  --accent: #0d7c66;
  --accent-2: #c46f28;
  --warn: #a8342f;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  color: var(--ink);
  background:
    radial-gradient(circle at 20% 0%, rgba(13, 124, 102, .16), transparent 28rem),
    linear-gradient(135deg, #f7f2e8 0%, #eef3ee 52%, #f6eee2 100%);
  font: 15px/1.45 ui-sans-serif, "Avenir Next", "Segoe UI", sans-serif;
}
main { width: min(1180px, calc(100vw - 32px)); margin: 0 auto; padding: 28px 0 42px; }
header {
  display: grid;
  grid-template-columns: 1fr auto;
  gap: 18px;
  align-items: end;
  padding: 22px 0 26px;
}
h1 { margin: 0; font-size: clamp(36px, 7vw, 76px); line-height: .9; letter-spacing: 0; }
.subline { margin-top: 12px; color: var(--muted); max-width: 720px; }
.status-pill {
  display: inline-flex;
  gap: 8px;
  align-items: center;
  min-height: 34px;
  padding: 7px 12px;
  border: 1px solid var(--line);
  border-radius: 999px;
  background: rgba(255,255,255,.72);
  font-weight: 700;
}
.dot { width: 10px; height: 10px; border-radius: 50%; background: var(--accent); box-shadow: 0 0 0 5px rgba(13,124,102,.12); }
.grid { display: grid; grid-template-columns: repeat(12, 1fr); gap: 14px; }
.panel {
  border: 1px solid var(--line);
  border-radius: 8px;
  background: rgba(255,253,248,.86);
  box-shadow: 0 18px 45px rgba(24,32,28,.08);
  overflow: hidden;
}
.span-3 { grid-column: span 3; }
.span-4 { grid-column: span 4; }
.span-8 { grid-column: span 8; }
.span-12 { grid-column: span 12; }
.metric { padding: 18px; min-height: 124px; display: flex; flex-direction: column; justify-content: space-between; }
.label { color: var(--muted); font-size: 12px; text-transform: uppercase; letter-spacing: .08em; }
.value { font-size: 34px; font-weight: 800; letter-spacing: 0; margin-top: 14px; }
.panel h2 { margin: 0; padding: 16px 18px; border-bottom: 1px solid var(--line); font-size: 16px; letter-spacing: 0; }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: 12px 18px; border-bottom: 1px solid var(--line); vertical-align: top; }
th { color: var(--muted); font-size: 12px; text-transform: uppercase; letter-spacing: .08em; }
tr:last-child td { border-bottom: 0; }
code { font-family: "SF Mono", Consolas, monospace; font-size: 13px; overflow-wrap: anywhere; }
.runtime { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 10px; padding: 18px; }
.runtime div { border: 1px solid var(--line); border-radius: 8px; padding: 12px; background: #fffaf1; }
.runtime strong { display: block; font-size: 22px; margin-bottom: 4px; }
.toolbar { display: flex; justify-content: space-between; align-items: center; padding: 14px 18px; border-top: 1px solid var(--line); color: var(--muted); }
button {
  border: 1px solid #0b5f4f;
  border-radius: 8px;
  background: var(--accent);
  color: white;
  padding: 9px 13px;
  font-weight: 800;
  cursor: pointer;
}
.empty, .error { padding: 18px; color: var(--muted); }
.error { color: var(--warn); }
@media (max-width: 820px) {
  header { grid-template-columns: 1fr; }
  .span-3, .span-4, .span-8 { grid-column: span 12; }
  .runtime { grid-template-columns: 1fr; }
}
</style>
</head>
<body>
<main>
  <header>
    <div>
      <h1>HARP Admin</h1>
      <div class="subline">Live proxy state, backend routes, runtime pressure, and request counters.</div>
    </div>
    <div class="status-pill"><span class="dot"></span><span id="status">loading</span></div>
  </header>

  <section class="grid" aria-live="polite">
    <article class="panel metric span-3"><div class="label">Backends</div><div class="value" id="backends">0</div></article>
    <article class="panel metric span-3"><div class="label">Requests</div><div class="value" id="requests">0</div></article>
    <article class="panel metric span-3"><div class="label">Cache hits</div><div class="value" id="hits">0</div></article>
    <article class="panel metric span-3"><div class="label">Errors</div><div class="value" id="errors">0</div></article>

    <article class="panel span-8">
      <h2>Routes</h2>
      <table>
        <thead><tr><th>Name</th><th>Domain</th><th>Path</th></tr></thead>
        <tbody id="routes"><tr><td colspan="3" class="empty">No routes registered</td></tr></tbody>
      </table>
    </article>

    <article class="panel span-4">
      <h2>Runtime</h2>
      <div class="runtime" id="runtime"></div>
      <div class="toolbar"><span id="updated">Waiting for data</span><button id="refresh">Refresh</button></div>
    </article>
  </section>
</main>
<script>
const api = "__ADMIN_API__";
const $ = (id) => document.getElementById(id);
const fmt = (v) => Number(v || 0).toLocaleString();

function metricKey(data, suffix) {
  const metrics = data.runtimeScheduler || {};
  return metrics[suffix] || 0;
}

function render(data) {
  $("status").textContent = data.status || "unknown";
  $("backends").textContent = fmt(data.counters && data.counters.activeConnections);
  $("requests").textContent = fmt(data.counters && data.counters.requestsTotal);
  $("hits").textContent = fmt(data.counters && data.counters.cacheHits);
  $("errors").textContent = fmt(data.counters && data.counters.backendErrors);

  const routes = data.routes || [];
  $("routes").innerHTML = routes.length ? routes.map((route) =>
    "<tr>" +
    "<td>" + escapeHTML(route.name || "unnamed") + "</td>" +
    "<td><code>" + escapeHTML(route.domain || "") + "</code></td>" +
    "<td><code>" + escapeHTML(route.path || "") + "</code></td>" +
    "</tr>"
  ).join("") : "<tr><td colspan=\"3\" class=\"empty\">No routes registered</td></tr>";

  const runtime = [
    ["Goroutines", "/sched/goroutines:goroutines"],
    ["Runnable", "/sched/goroutines/runnable:goroutines"],
    ["Waiting", "/sched/goroutines/waiting:goroutines"],
    ["Threads", "/sched/threads/total:threads"]
  ];
  $("runtime").innerHTML = runtime.map(([label, key]) =>
    "<div><strong>" + fmt(metricKey(data, key)) + "</strong><span class=\"label\">" + label + "</span></div>"
  ).join("");
  $("updated").textContent = "Updated " + new Date().toLocaleTimeString();
}

function escapeHTML(value) {
  return String(value).replace(/[&<>"']/g, (char) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;"
  }[char]));
}

async function load() {
  try {
    const res = await fetch(api, { cache: "no-store" });
    if (!res.ok) throw new Error("HTTP " + res.status);
    render(await res.json());
  } catch (err) {
    $("status").textContent = "error";
    $("routes").innerHTML = "<tr><td colspan=\"3\" class=\"error\">" + escapeHTML(err.message) + "</td></tr>";
  }
}

$("refresh").addEventListener("click", load);
load();
setInterval(load, 5000);
</script>
</body>
</html>`

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
		if isRegistrationAllowed(r.Path, reg.Key) {
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
		conn.routes = append(conn.routes, registeredRoute{
			name:    r.Name,
			pattern: pattern,
			domain:  routeDomain,
			path:    r.Path,
		})
		backendsMu.Lock()
		backends[backendKey(routeDomain, r.Path)] = conn
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
					delete(backends, backendKey(route.domain, route.path))
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

func compileAllowedRegistrationRules(rules []AllowedRegistrationRule) ([]compiledAllowedRegistrationRule, error) {
	compiled := make([]compiledAllowedRegistrationRule, 0, len(rules))
	for _, rule := range rules {
		pattern, err := regexp.Compile(rule.Route)
		if err != nil {
			return nil, fmt.Errorf("invalid regex in allowedRegistration route %q: %w", rule.Route, err)
		}
		compiled = append(compiled, compiledAllowedRegistrationRule{
			pattern: pattern,
			key:     rule.Key,
		})
	}
	return compiled, nil
}

func isRegistrationAllowed(path, key string) bool {
	rules := allowedRegistrationRules
	if len(rules) == 0 && len(config.AllowedRegistration) > 0 {
		var err error
		rules, err = compileAllowedRegistrationRules(config.AllowedRegistration)
		if err != nil {
			logError("Error compiling allowed registration rules: %v", err)
			return false
		}
	}
	for _, rule := range rules {
		if key == rule.key && rule.pattern.MatchString(path) {
			return true
		}
	}
	return false
}

func deliverPendingResponse(ctx context.Context, resp *pb.HTTPResponse, ch chan<- *pb.HTTPResponse) bool {
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
	defer func() {
		duration := time.Since(start)
		metrics.RequestsTotal.Add(1)
		metrics.RequestDuration.Add(r.URL.Path, int64(duration/time.Millisecond))
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
	w.Header().Set("X-Request-ID", reqID)
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

	// Find the best matching backend (longest path wins).
	chosen, matchedRoute := matchBackend(r.Host, r.URL.Path)

	if matchedRoute != "" {
		metrics.RouteRequests.Add(matchedRoute, 1)
	}

	if chosen == nil {
		logWarn("No matching route for %s", r.URL.String())
		http.NotFound(w, r)
		return
	}

	// Prepare channel for response.
	respCh := make(chan *pb.HTTPResponse, responseChannelBufferSize)
	pendingMu.Lock()
	pendingResponse[reqID] = respCh
	pendingMu.Unlock()
	defer func() {
		pendingMu.Lock()
		delete(pendingResponse, reqID)
		pendingMu.Unlock()
	}()

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
	flusher, canFlush := w.(http.Flusher)

	select {
	case resp := <-respCh:
		isStream := headerEnabled(resp.Headers, pb.StreamHeader)
		firstStatus := int(resp.Status)
		firstHeaders := filterInternalHeaders(resp.Headers)
		var bodyBuilder strings.Builder
		wroteHeaders := false
		for {
			if !wroteHeaders {
				for k, v := range firstHeaders {
					w.Header().Set(k, v)
				}
				w.WriteHeader(firstStatus)
				wroteHeaders = true
			}

			if resp.Body != "" {
				fmt.Fprint(w, resp.Body)
				if !isStream {
					bodyBuilder.WriteString(resp.Body)
				}
				if isStream && canFlush {
					flusher.Flush()
				}
			}

			if !isStream || headerEnabled(resp.Headers, pb.StreamEndHeader) {
				if config.EnableCache && strings.ToUpper(r.Method) == "GET" && config.CacheType != "none" && !isStream {
					cacheStore.Set(cacheKey, &pb.HTTPResponse{
						Status:  int32(firstStatus),
						Headers: firstHeaders,
						Body:    bodyBuilder.String(),
					})
				}
				return
			}

			select {
			case resp = <-respCh:
			case <-time.After(timeout):
				metrics.BackendErrors.Add(1)
				logWarn("Stream timeout for request %s", reqID)
				return
			}
		}
	case <-time.After(timeout):
		http.Error(w, "Timeout waiting for backend", http.StatusGatewayTimeout)
		metrics.BackendErrors.Add(1)
	}
}

func matchBackend(host, path string) (*backendConn, string) {
	var chosen *backendConn
	var matchedRoute string
	var bestLen int
	requestRoute := routeTarget(host, path)

	backendsMu.RLock()
	defer backendsMu.RUnlock()
	for _, conn := range backends {
		for _, route := range conn.routes {
			if route.pattern.MatchString(requestRoute) && len(route.path) > bestLen {
				chosen = conn
				matchedRoute = route.path
				bestLen = len(route.path)
			}
		}
	}
	return chosen, matchedRoute
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

func headerEnabled(headers map[string]string, key string) bool {
	val := strings.TrimSpace(strings.ToLower(headers[key]))
	return val == "1" || val == "true" || val == "yes"
}

func filterInternalHeaders(headers map[string]string) map[string]string {
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		if strings.EqualFold(k, pb.StreamHeader) || strings.EqualFold(k, pb.StreamEndHeader) {
			continue
		}
		out[k] = v
	}
	return out
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
	registerAdminHandlers(mux)

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
	registerAdminHandlers(mux)

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
	registerAdminHandlers(mux)

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
	if config.MaxRequestBodySize == 0 {
		config.MaxRequestBodySize = 10 * 1024 * 1024 // 10MB default
	}
	if config.GracefulShutdownDelay == "" {
		config.GracefulShutdownDelay = "10s"
	}

	if err := validateConfig(config); err != nil {
		log.Fatalf("Config error: %v", err)
	}
	allowedRegistrationRules, err = compileAllowedRegistrationRules(config.AllowedRegistration)
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

	return errors.Join(errs...)
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

	// Log shutdown completion
	<-ctx.Done()
	logInfo("HARP proxy stopped")
}
