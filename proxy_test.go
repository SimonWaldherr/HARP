package main

import (
	"context"
	"encoding/json"
	"expvar"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	pb "github.com/SimonWaldherr/HARP/harp"
)

func TestGetCacheKey(t *testing.T) {
	headers := map[string]string{"Content-Type": "application/json"}

	key1 := getCacheKey("GET", "/test", headers, "")
	key2 := getCacheKey("GET", "/test", headers, "")
	if key1 != key2 {
		t.Errorf("same inputs should produce same cache key: %s != %s", key1, key2)
	}

	key3 := getCacheKey("POST", "/test", headers, "")
	if key1 == key3 {
		t.Errorf("different methods should produce different cache keys")
	}

	key4 := getCacheKey("GET", "/other", headers, "")
	if key1 == key4 {
		t.Errorf("different URLs should produce different cache keys")
	}
}

func TestMemoryCache(t *testing.T) {
	origTTL := config.CacheTTL
	config.CacheTTL = "1m"
	t.Cleanup(func() { config.CacheTTL = origTTL })
	mc := NewMemoryCache()

	if mc.Size() != 0 {
		t.Errorf("new cache should be empty, got size %d", mc.Size())
	}

	resp := &pb.HTTPResponse{Status: 200, Body: "hello"}
	mc.Set("key1", resp)

	if mc.Size() != 1 {
		t.Errorf("cache should have 1 item, got %d", mc.Size())
	}

	got, ok := mc.Get("key1")
	if !ok {
		t.Fatal("expected cache hit for key1")
	}
	if got.Body != "hello" {
		t.Errorf("expected body 'hello', got '%s'", got.Body)
	}

	_, ok = mc.Get("missing")
	if ok {
		t.Error("expected cache miss for missing key")
	}

	mc.Delete("key1")
	_, ok = mc.Get("key1")
	if ok {
		t.Error("expected cache miss after delete")
	}

	mc.Set("k2", resp)
	mc.Set("k3", resp)
	mc.Clear()
	if mc.Size() != 0 {
		t.Errorf("cache should be empty after clear, got size %d", mc.Size())
	}
}

func TestMemoryCacheExpiry(t *testing.T) {
	origTTL := config.CacheTTL
	config.CacheTTL = "10ms"
	t.Cleanup(func() { config.CacheTTL = origTTL })
	mc := NewMemoryCache()

	resp := &pb.HTTPResponse{Status: 200, Body: "expiring"}
	mc.Set("expire_key", resp)

	_, ok := mc.Get("expire_key")
	if !ok {
		t.Fatal("expected cache hit immediately after set")
	}

	time.Sleep(20 * time.Millisecond)

	_, ok = mc.Get("expire_key")
	if ok {
		t.Error("expected cache miss after TTL expiry")
	}
	if mc.Size() != 0 {
		t.Errorf("expected expired cache entry to be removed, got size %d", mc.Size())
	}
}

func TestRateLimiter(t *testing.T) {
	origEnabled := config.EnableRateLimit
	config.EnableRateLimit = true
	t.Cleanup(func() { config.EnableRateLimit = origEnabled })
	rl := NewRateLimiter(3)

	ip := "127.0.0.1"
	for i := 0; i < 3; i++ {
		if !rl.Allow(ip) {
			t.Errorf("request %d should be allowed", i+1)
		}
	}

	if rl.Allow(ip) {
		t.Error("4th request in same second should be rate limited")
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	origEnabled := config.EnableRateLimit
	config.EnableRateLimit = false
	t.Cleanup(func() { config.EnableRateLimit = origEnabled })
	rl := NewRateLimiter(1)

	ip := "192.168.1.1"
	for i := 0; i < 10; i++ {
		if !rl.Allow(ip) {
			t.Errorf("request %d should be allowed when rate limiting is disabled", i+1)
		}
	}
}

func TestRateLimiterZeroLimitAllowsRequests(t *testing.T) {
	origEnabled := config.EnableRateLimit
	config.EnableRateLimit = true
	t.Cleanup(func() { config.EnableRateLimit = origEnabled })

	rl := NewRateLimiter(0)
	if !rl.Allow("127.0.0.1") {
		t.Fatal("zero rate limit should disable limiting")
	}
}

func TestInitMetrics(t *testing.T) {
	// Ensure initMetrics doesn't panic and creates non-nil metrics
	initMetrics()
	if metrics == nil {
		t.Fatal("metrics should not be nil after initMetrics")
	}
	if metrics.RequestsTotal == nil {
		t.Error("RequestsTotal should not be nil")
	}
	if metrics.CacheHits == nil {
		t.Error("CacheHits should not be nil")
	}
	if metrics.RouteRequests == nil {
		t.Error("RouteRequests should not be nil")
	}
	if metrics.RateLimited == nil {
		t.Error("RateLimited should not be nil")
	}
}

func TestMemoryCacheEviction(t *testing.T) {
	origTTL := config.CacheTTL
	origPool := config.ConnectionPoolSize
	config.CacheTTL = "1m"
	config.ConnectionPoolSize = 0
	t.Cleanup(func() {
		config.CacheTTL = origTTL
		config.ConnectionPoolSize = origPool
	})

	mc := &MemoryCache{
		items:    make(map[string]cacheItem),
		maxItems: 3,
	}

	// Fill cache to max
	mc.Set("a", &pb.HTTPResponse{Status: 200, Body: "a"})
	mc.Set("b", &pb.HTTPResponse{Status: 200, Body: "b"})
	mc.Set("c", &pb.HTTPResponse{Status: 200, Body: "c"})

	if mc.Size() != 3 {
		t.Fatalf("expected 3 items, got %d", mc.Size())
	}

	// Adding one more should evict the one closest to expiry (all same TTL, so one removed)
	mc.Set("d", &pb.HTTPResponse{Status: 200, Body: "d"})
	if mc.Size() != 3 {
		t.Errorf("expected 3 items after eviction, got %d", mc.Size())
	}

	// The new item should be present
	if _, ok := mc.Get("d"); !ok {
		t.Error("expected 'd' to be in cache")
	}
}

func TestMemoryCacheEvictsExpiredFirst(t *testing.T) {
	origTTL := config.CacheTTL
	config.CacheTTL = "5ms"
	t.Cleanup(func() { config.CacheTTL = origTTL })

	mc := &MemoryCache{
		items:    make(map[string]cacheItem),
		maxItems: 2,
	}

	mc.Set("old", &pb.HTTPResponse{Status: 200, Body: "old"})
	time.Sleep(10 * time.Millisecond)

	// Change TTL for new items
	config.CacheTTL = "1m"
	mc.Set("fresh", &pb.HTTPResponse{Status: 200, Body: "fresh"})

	// Cache is full (2 items), adding another should evict expired "old"
	mc.Set("newest", &pb.HTTPResponse{Status: 200, Body: "newest"})

	if _, ok := mc.Get("fresh"); !ok {
		t.Error("expected 'fresh' to still be in cache")
	}
	if _, ok := mc.Get("newest"); !ok {
		t.Error("expected 'newest' to be in cache")
	}
}

func TestRateLimiterCleanup(t *testing.T) {
	origEnabled := config.EnableRateLimit
	config.EnableRateLimit = true
	t.Cleanup(func() { config.EnableRateLimit = origEnabled })

	rl := NewRateLimiter(100)

	// Add some requests
	rl.Allow("10.0.0.1")
	rl.Allow("10.0.0.2")

	rl.mu.Lock()
	if len(rl.requests) != 2 {
		t.Errorf("expected 2 IPs tracked, got %d", len(rl.requests))
	}
	rl.mu.Unlock()

	// Wait for entries to expire
	time.Sleep(1100 * time.Millisecond)

	rl.Cleanup()

	rl.mu.Lock()
	if len(rl.requests) != 0 {
		t.Errorf("expected 0 IPs after cleanup, got %d", len(rl.requests))
	}
	rl.mu.Unlock()
}

func TestGetCacheKeyDeterministic(t *testing.T) {
	headers := map[string]string{"Accept": "text/html"}
	key1 := getCacheKey("GET", "/page", headers, "body1")
	key2 := getCacheKey("GET", "/page", headers, "body2")
	if key1 == key2 {
		t.Error("different body should produce different cache keys")
	}

	headersA := map[string]string{
		"Accept":       "text/html",
		"Content-Type": "application/json",
	}
	headersB := map[string]string{
		"Content-Type": "application/json",
		"Accept":       "text/html",
	}
	cacheKeyA := getCacheKey("GET", "/page", headersA, "body")
	cacheKeyB := getCacheKey("GET", "/page", headersB, "body")
	if cacheKeyA != cacheKeyB {
		t.Errorf("header map insertion order should not affect cache key: %s != %s", cacheKeyA, cacheKeyB)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	// Save original config
	origConfig := config

	// Create a minimal temporary config
	tmpFile, err := os.CreateTemp("", "harp-config-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(`{
		"grpcPort": ":50054",
		"httpPort": ":8080",
		"enableCache": false,
		"allowedRegistration": []
	}`)
	if err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	loadConfig(tmpFile.Name())

	if config.MaxConcurrentRequests != 1000 {
		t.Errorf("expected default MaxConcurrentRequests=1000, got %d", config.MaxConcurrentRequests)
	}
	if config.RequestTimeout != "30s" {
		t.Errorf("expected default RequestTimeout='30s', got %q", config.RequestTimeout)
	}
	if config.MaxRequestBodySize != 10*1024*1024 {
		t.Errorf("expected default MaxRequestBodySize=10MB, got %d", config.MaxRequestBodySize)
	}
	if config.GracefulShutdownDelay != "10s" {
		t.Errorf("expected default GracefulShutdownDelay='10s', got %q", config.GracefulShutdownDelay)
	}

	// Restore config
	config = origConfig
}

func TestValidateConfigCollectsErrors(t *testing.T) {
	err := validateConfig(Config{
		EnableGRPCTLS: true,
		EnableHTTPS:   true,
		EnableHTTP3:   true,
		CacheTTL:      "not-a-duration",
		ReadTimeout:   "still-not-a-duration",
		AllowedRegistration: []AllowedRegistrationRule{
			{Route: "[", Key: "secret"},
		},
	})
	if err == nil {
		t.Fatal("expected validation errors")
	}

	msg := err.Error()
	for _, want := range []string{
		"enableGRPCTLS requires grpcTLSCert and grpcTLSKey",
		"enableHTTPS requires httpsCert and httpsKey",
		"invalid cacheTTL",
		"invalid readTimeout",
		"invalid regex in allowedRegistration route",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected validation error %q in %q", want, msg)
		}
	}
}

func TestCompiledAllowedRegistrationRules(t *testing.T) {
	origConfig := config
	origRules := allowedRegistrationRules
	t.Cleanup(func() {
		config = origConfig
		allowedRegistrationRules = origRules
	})

	config.AllowedRegistration = []AllowedRegistrationRule{
		{Route: "/.*$", Key: "secret"},
		{Route: "/api/.*$", Key: "secret", Username: "operator", Password: "route-password"},
		{Route: "/api/admin/.*$", Key: "secret", Username: "admin", Password: "admin-password"},
	}
	var err error
	allowedRegistrationRules, err = compileAllowedRegistrationRules(config.AllowedRegistration)
	if err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}

	if !isRegistrationAllowed("/api/users", "secret") {
		t.Fatal("expected matching route/key to be allowed")
	}
	if isRegistrationAllowed("/api/users", "wrong-key") {
		t.Fatal("expected wrong key to be rejected")
	}
	if isRegistrationAllowed("/other", "wrong-key") {
		t.Fatal("expected wrong key to be rejected for catch-all route")
	}
	rule, ok := allowedRegistrationFor("/api/users", "secret")
	if !ok {
		t.Fatal("expected matching rule to be returned")
	}
	if rule.username != "operator" || rule.password != "route-password" {
		t.Fatalf("unexpected route auth config: %#v", rule)
	}
	rule, ok = allowedRegistrationFor("/api/admin/users", "secret")
	if !ok {
		t.Fatal("expected admin matching rule to be returned")
	}
	if rule.username != "admin" || rule.password != "admin-password" {
		t.Fatalf("expected most specific admin route auth config, got %#v", rule)
	}
	rule, ok = allowedRegistrationFor("/public", "secret")
	if !ok {
		t.Fatal("expected catch-all matching rule to be returned")
	}
	if rule.password != "" {
		t.Fatalf("expected catch-all route to be unprotected, got %#v", rule)
	}
}

func TestDeliverPendingResponseDoesNotBlockWhenChannelFull(t *testing.T) {
	ch := make(chan *pb.HTTPResponse, 1)
	ch <- &pb.HTTPResponse{RequestId: "first"}

	start := time.Now()
	ok := deliverPendingResponse(context.Background(), &pb.HTTPResponse{RequestId: "second"}, ch)
	if ok {
		t.Fatal("expected full response channel delivery to fail")
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("delivery blocked too long: %s", elapsed)
	}
}

func TestHeaderEnabled(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"one", map[string]string{"X-Test": "1"}, true},
		{"true", map[string]string{"X-Test": "true"}, true},
		{"yes", map[string]string{"X-Test": "yes"}, true},
		{"false", map[string]string{"X-Test": "false"}, false},
		{"missing", map[string]string{}, false},
	}
	for _, tc := range tests {
		if got := headerEnabled(tc.headers, "X-Test"); got != tc.want {
			t.Errorf("%s: expected %v, got %v", tc.name, tc.want, got)
		}
	}
}

func TestFilterInternalHeaders(t *testing.T) {
	headers := map[string]string{
		"Content-Type":      "text/event-stream",
		pb.StreamHeader:     "1",
		pb.StreamEndHeader:  "1",
		"X-Another-Header":  "ok",
		"x-harp-stream-end": "1",
	}
	filtered := filterInternalHeaders(headers)
	if filtered["Content-Type"] != "text/event-stream" {
		t.Fatalf("expected Content-Type to be preserved")
	}
	if filtered["X-Another-Header"] != "ok" {
		t.Fatalf("expected custom header to be preserved")
	}
	if _, ok := filtered[pb.StreamHeader]; ok {
		t.Fatalf("expected %s to be removed", pb.StreamHeader)
	}
	if _, ok := filtered[pb.StreamEndHeader]; ok {
		t.Fatalf("expected %s to be removed", pb.StreamEndHeader)
	}
	if _, ok := filtered["x-harp-stream-end"]; ok {
		t.Fatalf("expected case-insensitive stream-end header to be removed")
	}
}

func TestRuntimeMetricSnapshot(t *testing.T) {
	snapshot := runtimeMetricSnapshot([]string{
		"/sched/goroutines:goroutines",
		"/does/not/exist:units",
	})

	if _, ok := snapshot["/sched/goroutines:goroutines"]; !ok {
		t.Fatalf("expected scheduler goroutine metric to be present")
	}
	if _, ok := snapshot["/does/not/exist:units"]; ok {
		t.Fatalf("unsupported runtime metrics should be omitted")
	}
}

func TestAdminHandlersRespectConfig(t *testing.T) {
	origConfig := config
	t.Cleanup(func() { config = origConfig })

	config.EnableAdminUI = false
	mux := http.NewServeMux()
	registerAdminHandlers(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected disabled admin UI to return 404, got %d", rec.Code)
	}

	config.EnableAdminUI = true
	config.AdminPath = "/admin"
	mux = http.NewServeMux()
	registerAdminHandlers(mux)

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected enabled admin UI to return 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "HARP Admin") {
		t.Fatalf("expected admin HTML response, got %q", rec.Body.String())
	}
}

func TestAdminHandlersRequireConfiguredPassword(t *testing.T) {
	origConfig := config
	t.Cleanup(func() { config = origConfig })

	config.EnableAdminUI = true
	config.AdminPath = "/admin"
	config.AdminPassword = "secret"
	mux := http.NewServeMux()
	registerAdminHandlers(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthenticated admin request to return 401, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.SetBasicAuth("admin", "secret")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected authenticated admin request to return 200, got %d", rec.Code)
	}
}

func TestAdminStatusHandler(t *testing.T) {
	origConfig := config
	origMetrics := metrics
	origCache := cacheStore
	origStart := startTime
	t.Cleanup(func() {
		config = origConfig
		metrics = origMetrics
		cacheStore = origCache
		startTime = origStart
	})

	config = Config{
		GRPCPort:              ":50054",
		HTTPPort:              ":8080",
		EnableCache:           true,
		CacheType:             "memory",
		EnableRateLimit:       true,
		MaxConcurrentRequests: 1000,
	}
	metrics = newTestMetrics()
	metrics.RequestsTotal.Add(3)
	cacheStore = NewMemoryCache()
	startTime = time.Now().Add(-time.Second)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/status", nil)
	adminStatusHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}
	if body["status"] != "healthy" {
		t.Fatalf("expected healthy status, got %#v", body["status"])
	}
	if _, ok := body["runtimeScheduler"].(map[string]interface{}); !ok {
		t.Fatalf("expected runtimeScheduler object in response")
	}
}

func newTestMetrics() *Metrics {
	return &Metrics{
		RequestsTotal:      new(expvar.Int),
		RequestDuration:    new(expvar.Map).Init(),
		CacheHits:          new(expvar.Int),
		CacheMisses:        new(expvar.Int),
		BackendErrors:      new(expvar.Int),
		ActiveConnections:  new(expvar.Int),
		BackendsRegistered: new(expvar.Int),
		RouteRequests:      new(expvar.Map).Init(),
		RateLimited:        new(expvar.Int),
	}
}

func TestClientIPFromRemoteAddr(t *testing.T) {
	tests := []struct {
		remoteAddr string
		want       string
	}{
		{"127.0.0.1:12345", "127.0.0.1"},
		{"[::1]:12345", "::1"},
		{"malformed-address", "malformed-address"},
	}

	for _, tc := range tests {
		if got := clientIPFromRemoteAddr(tc.remoteAddr); got != tc.want {
			t.Errorf("clientIPFromRemoteAddr(%q) = %q, want %q", tc.remoteAddr, got, tc.want)
		}
	}
}

func TestMatchBackendUsesHostAndPath(t *testing.T) {
	origBackends := backends
	backendsMu.Lock()
	exampleBackend := &backendConn{
		routes: []registeredRoute{{
			name:     "example",
			domain:   `example\.com`,
			path:     "/api",
			pattern:  regexp.MustCompile(`example\.com/api`),
			username: "operator",
			password: "route-password",
		}},
	}
	otherBackend := &backendConn{
		routes: []registeredRoute{{
			name:    "other",
			domain:  `other\.com`,
			path:    "/api",
			pattern: regexp.MustCompile(`other\.com/api`),
		}},
	}
	backends = map[string]*backendConn{
		backendKey(`example\.com`, "/api"): exampleBackend,
		backendKey(`other\.com`, "/api"):   otherBackend,
	}
	backendsMu.Unlock()
	t.Cleanup(func() {
		backendsMu.Lock()
		backends = origBackends
		backendsMu.Unlock()
	})

	got, route, auth := matchBackend("example.com", "/api/users")
	if got != exampleBackend {
		t.Fatalf("expected example.com backend, got %#v", got)
	}
	if route != "/api" {
		t.Fatalf("expected matched route /api, got %q", route)
	}
	if auth.username != "operator" || auth.password != "route-password" {
		t.Fatalf("unexpected route auth config: %#v", auth)
	}

	got, route, _ = matchBackend("example.com:8080", "/api/users")
	if got != exampleBackend {
		t.Fatalf("expected example.com backend when Host includes a port, got %#v", got)
	}
	if route != "/api" {
		t.Fatalf("expected matched route /api with Host port, got %q", route)
	}

	got, route, _ = matchBackend("missing.example", "/api/users")
	if got != nil || route != "" {
		t.Fatalf("expected no backend for unmatched host, got %#v route %q", got, route)
	}
}

func TestRequireBasicAuth(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	if requireBasicAuth(rec, req, "user", "password", "Test Realm") {
		t.Fatal("expected missing credentials to fail")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.SetBasicAuth("user", "password")
	if !requireBasicAuth(rec, req, "user", "password", "Test Realm") {
		t.Fatal("expected valid credentials to pass")
	}
}
