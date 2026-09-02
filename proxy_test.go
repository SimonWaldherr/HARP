package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

func TestCacheLookupMethod(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		cacheMethod, ok := cacheLookupMethod(method)
		if !ok || cacheMethod != http.MethodGet {
			t.Fatalf("cacheLookupMethod(%q) = %q, %v", method, cacheMethod, ok)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		if cacheMethod, ok := cacheLookupMethod(method); ok || cacheMethod != "" {
			t.Fatalf("cacheLookupMethod(%q) unexpectedly enabled caching", method)
		}
	}
}

func TestResponseBodyAllowed(t *testing.T) {
	tests := []struct {
		method string
		status int
		want   bool
	}{
		{http.MethodGet, http.StatusOK, true},
		{http.MethodPost, http.StatusCreated, true},
		{http.MethodHead, http.StatusOK, false},
		{http.MethodGet, http.StatusContinue, false},
		{http.MethodGet, http.StatusNoContent, false},
		{http.MethodGet, http.StatusNotModified, false},
	}
	for _, tc := range tests {
		if got := responseBodyAllowed(tc.method, tc.status); got != tc.want {
			t.Errorf("responseBodyAllowed(%q, %d) = %v, want %v", tc.method, tc.status, got, tc.want)
		}
	}
}

func TestWriteCachedResponseHandlesHEAD(t *testing.T) {
	resp := &pb.HTTPResponse{
		Status:  http.StatusOK,
		Headers: map[string]string{"Content-Type": "text/plain"},
		Body:    "cached body",
	}
	rec := httptest.NewRecorder()
	writeCachedResponse(rec, http.MethodHead, resp)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD response included %d body bytes", rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(resp.Body)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(resp.Body))
	}
	if got := rec.Header().Get("Via"); got != "1.1 harp" {
		t.Fatalf("Via = %q", got)
	}
}

func TestWriteCachedResponseSuppressesNoContentBody(t *testing.T) {
	rec := httptest.NewRecorder()
	writeCachedResponse(rec, http.MethodPost, &pb.HTTPResponse{Status: http.StatusNoContent, Body: "invalid"})
	if rec.Body.Len() != 0 {
		t.Fatalf("204 response included %d body bytes", rec.Body.Len())
	}
}

type trackedReadCloser struct {
	reader bytes.Reader
	reads  int
	closed bool
}

func newTrackedReadCloser(body []byte) *trackedReadCloser {
	reader := &trackedReadCloser{}
	reader.reader.Reset(body)
	return reader
}

func (r *trackedReadCloser) Read(p []byte) (int, error) {
	r.reads++
	return r.reader.Read(p)
}

func (r *trackedReadCloser) Close() error {
	r.closed = true
	return nil
}

func TestReadRequestBodyRejectsKnownOversizeWithoutReading(t *testing.T) {
	body := newTrackedReadCloser([]byte("oversized"))
	req := &http.Request{Body: body, ContentLength: 9}
	_, err := readRequestBody(req, 4)
	if !errors.Is(err, errRequestBodyTooLarge) {
		t.Fatalf("expected body-too-large error, got %v", err)
	}
	if body.reads != 0 {
		t.Fatalf("oversized body was read %d times", body.reads)
	}
	if !body.closed {
		t.Fatal("oversized body was not closed")
	}
}

func TestReadRequestBodyLimitsUnknownLength(t *testing.T) {
	req := &http.Request{Body: io.NopCloser(strings.NewReader("oversized")), ContentLength: -1}
	_, err := readRequestBody(req, 4)
	if !errors.Is(err, errRequestBodyTooLarge) {
		t.Fatalf("expected body-too-large error, got %v", err)
	}
}

func TestReadRequestBodyAcceptsShortKnownLength(t *testing.T) {
	req := &http.Request{
		Body:          io.NopCloser(strings.NewReader("short")),
		ContentLength: 10,
	}
	body, err := readRequestBody(req, 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(body) != "short" {
		t.Fatalf("body = %q", body)
	}
}

func TestCORSPreflightBypassesFullRequestSemaphore(t *testing.T) {
	originalConfig := config
	originalMetrics := metrics
	originalRequestSem := requestSem
	config.EnableCORS = true
	metrics = newTestMetrics()
	requestSem = make(chan struct{}, 1)
	requestSem <- struct{}{}
	t.Cleanup(func() {
		config = originalConfig
		metrics = originalMetrics
		requestSem = originalRequestSem
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api", nil)
	httpHandler(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if methods := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(methods, http.MethodHead) {
		t.Fatalf("HEAD missing from allowed methods: %q", methods)
	}
	if got := metrics.RequestsTotal.Value(); got != 1 {
		t.Fatalf("request count = %d, want 1", got)
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
	var now time.Duration
	rl.now = func() time.Duration { return now }

	ip := "127.0.0.1"
	for i := 0; i < 3; i++ {
		if !rl.Allow(ip) {
			t.Errorf("request %d should be allowed", i+1)
		}
	}

	if rl.Allow(ip) {
		t.Error("4th request in same second should be rate limited")
	}

	now += time.Second
	if !rl.Allow(ip) {
		t.Error("request should be allowed once the oldest entry leaves the sliding window")
	}
	if got := rl.windowSize(ip); got != 3 {
		t.Fatalf("rate window grew beyond configured limit: got %d, want 3", got)
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

func TestRateLimiterConcurrentClients(t *testing.T) {
	originalEnabled := config.EnableRateLimit
	config.EnableRateLimit = true
	t.Cleanup(func() { config.EnableRateLimit = originalEnabled })

	rl := NewRateLimiter(2)
	rl.now = func() time.Duration { return 0 }

	const clients = 256
	errors := make(chan string, clients)
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		clientIP := "192.0.2." + strconv.Itoa(i)
		wg.Go(func() {
			if !rl.Allow(clientIP) || !rl.Allow(clientIP) || rl.Allow(clientIP) {
				errors <- clientIP
			}
		})
	}
	wg.Wait()
	close(errors)
	for clientIP := range errors {
		t.Errorf("unexpected concurrent rate-limit result for %s", clientIP)
	}
	if got := rl.trackedClients(); got != clients {
		t.Fatalf("tracked clients = %d, want %d", got, clients)
	}
}

func TestShardedStoreConcurrentAccess(t *testing.T) {
	store := newShardedStore[int]()
	const entries = 1_024
	errors := make(chan string, entries)
	var wg sync.WaitGroup
	for i := 0; i < entries; i++ {
		key := "request-" + strconv.Itoa(i)
		value := i
		wg.Go(func() {
			store.Set(key, value)
			if got, ok := store.Get(key); !ok || got != value {
				errors <- key
			}
			store.Delete(key)
			if _, ok := store.Get(key); ok {
				errors <- key
			}
		})
	}
	wg.Wait()
	close(errors)
	for key := range errors {
		t.Errorf("unexpected sharded store result for %s", key)
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

func TestRouteMetricsConcurrentSnapshot(t *testing.T) {
	testMetrics := newTestMetrics()
	route := testMetrics.routeMetricsFor("/api")
	const goroutines = 32
	const updates = 1000

	var wg sync.WaitGroup
	for worker := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for update := range updates {
				route.add(uint64(worker*updates+update), time.Millisecond)
			}
		}()
	}
	wg.Wait()

	want := int64(goroutines * updates)
	if got := route.requestCount(); got != want {
		t.Fatalf("request count = %d, want %d", got, want)
	}
	if got := route.durationMillisTotal(); got != want {
		t.Fatalf("duration total = %d, want %d", got, want)
	}
	if got := expvarIntMapSnapshot(testMetrics.RouteRequests)["/api"]; got != want {
		t.Fatalf("exported request count = %d, want %d", got, want)
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

func TestMemoryCacheUsesIndependentCapacity(t *testing.T) {
	originalConfig := config
	config.ConnectionPoolSize = 1
	config.CacheMaxItems = 7
	t.Cleanup(func() { config = originalConfig })

	cache := NewMemoryCache()
	if cache.maxItems != 7 {
		t.Fatalf("cache capacity = %d, want 7", cache.maxItems)
	}
}

func TestV1CompatibilityUsesLegacyCacheCapacity(t *testing.T) {
	originalConfig := config
	config.CompatibilityMode = compatibilityModeV1
	config.ConnectionPoolSize = 7
	config.CacheMaxItems = 0
	t.Cleanup(func() { config = originalConfig })

	cache := NewMemoryCache()
	if cache.maxItems != 70 {
		t.Fatalf("legacy cache capacity = %d, want 70", cache.maxItems)
	}

	config.CacheMaxItems = 11
	cache = NewMemoryCache()
	if cache.maxItems != 11 {
		t.Fatalf("explicit cache capacity = %d, want 11", cache.maxItems)
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

func TestDiskCacheAtomicSetGet(t *testing.T) {
	originalTTL := config.CacheTTL
	config.CacheTTL = "1m"
	t.Cleanup(func() { config.CacheTTL = originalTTL })

	dir := t.TempDir()
	cache := NewDiskCache(dir)
	want := &pb.HTTPResponse{Status: http.StatusOK, Body: "cached"}
	cache.Set("key", want)

	got, ok := cache.Get("key")
	if !ok {
		t.Fatal("expected disk cache hit")
	}
	if got.Status != want.Status || got.Body != want.Body {
		t.Fatalf("unexpected cached response: %#v", got)
	}
	tempFiles, err := filepath.Glob(filepath.Join(dir, ".harp-cache-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tempFiles) != 0 {
		t.Fatalf("temporary cache files were not cleaned up: %#v", tempFiles)
	}
}

func TestRateLimiterCleanup(t *testing.T) {
	origEnabled := config.EnableRateLimit
	config.EnableRateLimit = true
	t.Cleanup(func() { config.EnableRateLimit = origEnabled })

	rl := NewRateLimiter(100)
	var now time.Duration
	rl.now = func() time.Duration { return now }

	// Add some requests
	rl.Allow("10.0.0.1")
	rl.Allow("10.0.0.2")

	if got := rl.trackedClients(); got != 2 {
		t.Errorf("expected 2 IPs tracked, got %d", got)
	}

	// Advance the injected clock so cleanup remains deterministic and fast.
	now += time.Second

	rl.Cleanup()

	if got := rl.trackedClients(); got != 0 {
		t.Errorf("expected 0 IPs after cleanup, got %d", got)
	}
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

func TestGetCacheKeyIgnoresVolatileProxyHeaders(t *testing.T) {
	base := map[string]string{
		"Accept":            "application/json",
		"X-Forwarded-Host":  "example.com",
		"X-Forwarded-Proto": "https",
	}
	withRequestMetadata := map[string]string{
		"accept":            "application/json",
		"x-forwarded-host":  "example.com",
		"x-forwarded-proto": "https",
		"X-Forwarded-For":   "203.0.113.10",
		"Forwarded":         `for="203.0.113.10";proto="https";host="example.com"`,
		"Via":               "1.1 harp",
		"X-Request-ID":      "unique-request-id",
	}

	if got, want := getCacheKey("GET", "/api", withRequestMetadata, ""), getCacheKey("GET", "/api", base, ""); got != want {
		t.Fatalf("volatile proxy headers changed cache key: got %s, want %s", got, want)
	}

	differentHost := maps.Clone(base)
	differentHost["X-Forwarded-Host"] = "other.example.com"
	if getCacheKey("GET", "/api", differentHost, "") == getCacheKey("GET", "/api", base, "") {
		t.Fatal("representation-affecting forwarded host must remain part of the cache key")
	}
}

func BenchmarkGetCacheKey(b *testing.B) {
	headers := map[string]string{
		"Accept":            "application/json",
		"Accept-Encoding":   "gzip, br",
		"Authorization":     "Bearer token",
		"X-Forwarded-Host":  "example.com",
		"X-Forwarded-Proto": "https",
		"X-Forwarded-For":   "203.0.113.10",
		"X-Request-ID":      "request-id",
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = getCacheKey(http.MethodGet, "/api/items?page=1", headers, "")
	}
}

func BenchmarkRateLimiterParallel(b *testing.B) {
	originalEnabled := config.EnableRateLimit
	config.EnableRateLimit = true
	b.Cleanup(func() { config.EnableRateLimit = originalEnabled })
	rl := NewRateLimiter(100)
	var clientID atomic.Uint64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		clientIP := "198.51.100." + strconv.FormatUint(clientID.Add(1), 10)
		for pb.Next() {
			rl.Allow(clientIP)
		}
	})
}

func BenchmarkBackendRoutingManyRoutes(b *testing.B) {
	originalBackends := backends
	originalIndex := backendIndex
	originalPathIndex := backendPathIndex
	originalIndexedPools := backendIndexedPools
	originalStrategy := config.LoadBalancingStrategy
	backendsMu.Lock()
	backends = make(map[string]*backendPool)
	backendIndex = nil
	backendPathIndex = make(map[string][]*backendPool)
	backendIndexedPools = 0
	for i := 0; i < 1_000; i++ {
		routePath := fmt.Sprintf("/route/%04d", i)
		route := registeredRoute{
			path:          routePath,
			domain:        `example\.com`,
			domainPattern: regexp.MustCompile(`^example\.com$`),
		}
		conn := &backendConn{routes: []registeredRoute{route}}
		addBackendToPool(backendKey(route.domain, route.path), route, conn)
	}
	backendsMu.Unlock()
	config.LoadBalancingStrategy = loadBalancingFirst
	b.Cleanup(func() {
		config.LoadBalancingStrategy = originalStrategy
		backendsMu.Lock()
		backends = originalBackends
		backendIndex = originalIndex
		backendPathIndex = originalPathIndex
		backendIndexedPools = originalIndexedPools
		backendsMu.Unlock()
	})

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			backend, _, _ := matchBackend("example.com", "/route/0999/items")
			if backend == nil {
				b.Fatal("expected matching backend")
			}
		}
	})
}

func BenchmarkMetricsHandler(b *testing.B) {
	originalMetrics := metrics
	originalCache := cacheStore
	metrics = newTestMetrics()
	cacheStore = NewMemoryCache()
	b.Cleanup(func() {
		metrics = originalMetrics
		cacheStore = originalCache
	})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		metricsHandler(rec, req)
	}
}

func BenchmarkAdminStatusHandler(b *testing.B) {
	originalConfig := config
	originalMetrics := metrics
	originalCache := cacheStore
	originalStart := startTime
	config.AdminInsecureSkipAuth = true
	metrics = newTestMetrics()
	cacheStore = NewMemoryCache()
	startTime = time.Now().Add(-time.Hour)
	b.Cleanup(func() {
		config = originalConfig
		metrics = originalMetrics
		cacheStore = originalCache
		startTime = originalStart
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/api/status", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rec := httptest.NewRecorder()
		adminStatusHandler(rec, req)
	}
}

func BenchmarkRouteMetricsParallel(b *testing.B) {
	testMetrics := newTestMetrics()
	route := testMetrics.routeMetricsFor("/api")
	var sequence atomic.Uint64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			route.add(sequence.Add(1), time.Millisecond)
		}
	})
}

func BenchmarkReadRequestBody(b *testing.B) {
	payload := bytes.Repeat([]byte("x"), 64<<10)
	b.Run("POST-64KiB", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			req := &http.Request{
				Method:        http.MethodPost,
				Body:          io.NopCloser(bytes.NewReader(payload)),
				ContentLength: int64(len(payload)),
			}
			body, err := readRequestBody(req, int64(len(payload)))
			if err != nil || len(body) != len(payload) {
				b.Fatal("unexpected body read result")
			}
		}
	})
	b.Run("POST-known-oversize", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			req := &http.Request{
				Method:        http.MethodPost,
				Body:          io.NopCloser(bytes.NewReader(payload)),
				ContentLength: int64(len(payload)),
			}
			if _, err := readRequestBody(req, 1024); !errors.Is(err, errRequestBodyTooLarge) {
				b.Fatal("expected body-too-large error")
			}
		}
	})
}

func BenchmarkWriteCachedResponse(b *testing.B) {
	resp := &pb.HTTPResponse{
		Status:    http.StatusOK,
		Headers:   map[string]string{"Content-Type": "application/octet-stream"},
		BodyBytes: bytes.Repeat([]byte("x"), 64<<10),
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		b.Run(method, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				writeCachedResponse(httptest.NewRecorder(), method, resp)
			}
		})
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	// Save original config
	origConfig := config
	origAdminPrefixes := adminAllowedPrefixes

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
	if config.MaxConcurrentWebSockets != 256 {
		t.Errorf("expected default MaxConcurrentWebSockets=256, got %d", config.MaxConcurrentWebSockets)
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
	if config.LoadBalancingStrategy != loadBalancingRoundRobin {
		t.Errorf("expected default LoadBalancingStrategy=%q, got %q", loadBalancingRoundRobin, config.LoadBalancingStrategy)
	}

	// Restore config
	config = origConfig
	adminAllowedPrefixes = origAdminPrefixes
}

func TestNewConfiguredHTTPServerUsesConfiguredLimits(t *testing.T) {
	original := config
	t.Cleanup(func() { config = original })
	config.ReadTimeout = "7s"
	config.WriteTimeout = "11s"
	config.IdleTimeout = "2m"
	config.MaxHeaderSize = 16 << 10

	server := newConfiguredHTTPServer(":8443", http.NotFoundHandler())
	if server.ReadTimeout != 7*time.Second {
		t.Fatalf("ReadTimeout = %s, want 7s", server.ReadTimeout)
	}
	if server.WriteTimeout != 11*time.Second {
		t.Fatalf("WriteTimeout = %s, want 11s", server.WriteTimeout)
	}
	if server.IdleTimeout != 2*time.Minute {
		t.Fatalf("IdleTimeout = %s, want 2m", server.IdleTimeout)
	}
	if server.MaxHeaderBytes != 16<<10 {
		t.Fatalf("MaxHeaderBytes = %d, want %d", server.MaxHeaderBytes, 16<<10)
	}
}

func TestShutdownRunningServersStopsHTTPServer(t *testing.T) {
	servers.mu.Lock()
	originalHTTP := servers.http
	originalHTTP3 := servers.http3
	originalGRPC := servers.grpc
	servers.http = nil
	servers.http3 = nil
	servers.grpc = nil
	servers.mu.Unlock()
	originalShuttingDown := shuttingDown.Load()
	t.Cleanup(func() {
		servers.mu.Lock()
		servers.http = originalHTTP
		servers.http3 = originalHTTP3
		servers.grpc = originalGRPC
		servers.mu.Unlock()
		shuttingDown.Store(originalShuttingDown)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.NotFoundHandler()}
	registerHTTPServer(server)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	shutdownRunningServers(ctx)

	select {
	case err := <-serveDone:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("Serve returned %v, want http.ErrServerClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP server did not stop during graceful shutdown")
	}
}

func TestValidateConfigCollectsErrors(t *testing.T) {
	err := validateConfig(Config{
		EnableGRPCTLS:           true,
		EnableHTTPS:             true,
		EnableHTTP3:             true,
		EnableAdminUI:           true,
		CacheTTL:                "not-a-duration",
		ReadTimeout:             "still-not-a-duration",
		LoadBalancingStrategy:   "random",
		AdminAllowedCIDRs:       []string{"not-a-cidr"},
		ConnectionPoolSize:      -1,
		CacheMaxItems:           -1,
		MaxConcurrentWebSockets: -1,
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
		"enableAdminUI requires adminPassword unless adminInsecureSkipAuth is true",
		"invalid loadBalancingStrategy",
		"invalid adminAllowedCIDRs entry",
		"invalid cacheTTL",
		"invalid readTimeout",
		"invalid regex in allowedRegistration route",
		"connectionPoolSize must not be negative",
		"cacheMaxItems must not be negative",
		"maxConcurrentWebSockets must not be negative",
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
		{"case-insensitive", map[string]string{"x-test": "1"}, true},
		{"missing", map[string]string{}, false},
	}
	for _, tc := range tests {
		if got := headerEnabled(tc.headers, "X-Test"); got != tc.want {
			t.Errorf("%s: expected %v, got %v", tc.name, tc.want, got)
		}
	}
}

func TestIsWebSocketUpgrade(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Connection", "keep-alive, Upgrade")
	req.Header.Set("Upgrade", "websocket")

	if !isWebSocketUpgrade(req) {
		t.Fatal("expected WebSocket upgrade to be detected")
	}

	req.Header.Set("Connection", "keep-alive")
	if isWebSocketUpgrade(req) {
		t.Fatal("expected request without upgrade connection token to be rejected")
	}
}

func TestDeliverPendingResponseStreamsUseBackpressure(t *testing.T) {
	ch := make(chan *pb.HTTPResponse)
	resp := &pb.HTTPResponse{
		RequestId: "stream-1",
		Headers:   map[string]string{pb.StreamHeader: "1"},
	}
	done := make(chan bool, 1)

	go func() {
		done <- deliverPendingResponse(context.Background(), resp, ch)
	}()

	got := <-ch
	if got != resp {
		t.Fatal("expected streamed response to be delivered")
	}
	if ok := <-done; !ok {
		t.Fatal("expected streamed response delivery to succeed")
	}
}

func TestDeliverPendingResponseDropsFullNonStreamChannel(t *testing.T) {
	ch := make(chan *pb.HTTPResponse, 1)
	ch <- &pb.HTTPResponse{RequestId: "existing"}

	ok := deliverPendingResponse(context.Background(), &pb.HTTPResponse{RequestId: "non-stream"}, ch)
	if ok {
		t.Fatal("expected non-stream response delivery to fail when channel is full")
	}
}

func TestFilterInternalHeaders(t *testing.T) {
	headers := map[string]string{
		"Content-Type":      "text/event-stream",
		pb.StreamHeader:     "1",
		pb.StreamEndHeader:  "1",
		pb.StreamTypeHeader: pb.StreamTypeSSE,
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
	if _, ok := filtered[pb.StreamTypeHeader]; ok {
		t.Fatalf("expected %s to be removed", pb.StreamTypeHeader)
	}
}

func TestApplyStreamDefaults(t *testing.T) {
	headers := map[string]string{"Content-Length": "100"}
	applyStreamDefaults(headers, pb.StreamTypeSSE)

	if headers["Content-Type"] != "text/event-stream" {
		t.Fatalf("expected SSE content type, got %q", headers["Content-Type"])
	}
	if headers["Cache-Control"] != "no-cache" {
		t.Fatalf("expected no-cache cache control, got %q", headers["Cache-Control"])
	}
	if _, ok := headers["Content-Length"]; ok {
		t.Fatal("expected Content-Length to be removed for streams")
	}

	headers = map[string]string{"Content-Type": "application/json"}
	applyStreamDefaults(headers, pb.StreamTypeNDJSON)
	if headers["Content-Type"] != "application/json" {
		t.Fatalf("expected existing content type to be preserved")
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
	config.AdminInsecureSkipAuth = true
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
	config.AdminInsecureSkipAuth = false
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

func TestAdminHandlersRejectMissingPasswordUnlessExplicitlyDisabled(t *testing.T) {
	origConfig := config
	origPrefixes := adminAllowedPrefixes
	t.Cleanup(func() {
		config = origConfig
		adminAllowedPrefixes = origPrefixes
	})

	config.EnableAdminUI = true
	config.AdminPath = "/admin"
	config.AdminPassword = ""
	config.AdminInsecureSkipAuth = false
	adminAllowedPrefixes = nil

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	adminUIHandler(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected missing admin password to return 403, got %d", rec.Code)
	}
}

func TestAdminHandlersRespectAllowedCIDRs(t *testing.T) {
	origConfig := config
	origPrefixes := adminAllowedPrefixes
	t.Cleanup(func() {
		config = origConfig
		adminAllowedPrefixes = origPrefixes
	})

	config.AdminPassword = "secret"
	config.AdminInsecureSkipAuth = false
	var err error
	adminAllowedPrefixes, err = compileAdminAllowedCIDRs([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatalf("unexpected CIDR compile error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.SetBasicAuth("admin", "secret")
	adminUIHandler(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected disallowed admin IP to return 403, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.SetBasicAuth("admin", "secret")
	adminUIHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected allowed admin IP to return 200, got %d", rec.Code)
	}
}

func TestRegisterHealthHandlers(t *testing.T) {
	origConfig := config
	origStart := startTime
	t.Cleanup(func() {
		config = origConfig
		startTime = origStart
	})

	config.EnableHealthCheck = true
	config.HealthCheckPath = "/health"
	startTime = time.Now().Add(-time.Second)

	mux := http.NewServeMux()
	registerHealthHandlers(mux)

	tests := []struct {
		path       string
		wantCode   int
		wantCheck  string
		wantStatus string
	}{
		{"/health", http.StatusOK, "healthz", "healthy"},
		{"/healthz", http.StatusOK, "healthz", "healthy"},
		{"/livez", http.StatusOK, "livez", "alive"},
		{"/readyz", http.StatusOK, "readyz", "ready"},
	}

	for _, tc := range tests {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		mux.ServeHTTP(rec, req)
		if rec.Code != tc.wantCode {
			t.Fatalf("%s: expected status %d, got %d", tc.path, tc.wantCode, rec.Code)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: invalid json response: %v", tc.path, err)
		}
		if body["check"] != tc.wantCheck {
			t.Fatalf("%s: expected check %q, got %#v", tc.path, tc.wantCheck, body["check"])
		}
		if body["status"] != tc.wantStatus {
			t.Fatalf("%s: expected status %q, got %#v", tc.path, tc.wantStatus, body["status"])
		}
	}
}

func TestReadyzReportsUnavailableBeforeStartup(t *testing.T) {
	origStart := startTime
	t.Cleanup(func() { startTime = origStart })
	startTime = time.Time{}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	readyzHandler(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503, got %d", rec.Code)
	}
}

func TestForwardedRequestHeadersStripHopByHopAndAddProxyHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://public.example.test/app", nil)
	req.Host = "public.example.test"
	req.RemoteAddr = "203.0.113.7:54321"
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("Connection", "keep-alive, X-Custom-Hop")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Proxy-Authorization", "secret")
	req.Header.Set("TE", "trailers")
	req.Header.Set("Trailer", "Expires")
	req.Header.Set("Transfer-Encoding", "chunked")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("X-Custom-Hop", "remove-me")
	req.Header.Set("X-Forwarded-For", "198.51.100.10")

	got := forwardedRequestHeaders(req)

	for _, key := range []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authorization",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
		"X-Custom-Hop",
	} {
		if value := got.Get(key); value != "" {
			t.Fatalf("expected %s to be stripped, got %q", key, value)
		}
	}
	if got.Get("Accept") != "text/plain" {
		t.Fatalf("expected end-to-end header to be preserved, got %q", got.Get("Accept"))
	}
	if values := got.Values("X-Forwarded-For"); len(values) != 2 || values[0] != "198.51.100.10" || values[1] != "203.0.113.7" {
		t.Fatalf("unexpected X-Forwarded-For chain: %#v", values)
	}
	if got.Get("X-Forwarded-Host") != "public.example.test" {
		t.Fatalf("unexpected X-Forwarded-Host: %q", got.Get("X-Forwarded-Host"))
	}
	if got.Get("X-Forwarded-Proto") != "https" {
		t.Fatalf("unexpected X-Forwarded-Proto: %q", got.Get("X-Forwarded-Proto"))
	}
	if got.Get("X-Forwarded-Port") != "443" {
		t.Fatalf("unexpected X-Forwarded-Port: %q", got.Get("X-Forwarded-Port"))
	}
	if got.Get("Forwarded") != `for="203.0.113.7";proto="https";host="public.example.test"` {
		t.Fatalf("unexpected Forwarded header: %q", got.Get("Forwarded"))
	}
	if got.Get("Via") != "1.1 harp" {
		t.Fatalf("unexpected Via header: %q", got.Get("Via"))
	}
}

func TestForwardedRequestHeadersPreserveExistingForwardedDefaults(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://internal.example.test/app", nil)
	req.Host = "public.example.test"
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-Forwarded-Host", "edge.example.test")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Port", "8443")
	req.Header.Set("Via", "1.1 edge")

	got := forwardedRequestHeaders(req)

	if got.Get("X-Forwarded-Host") != "edge.example.test" {
		t.Fatalf("expected existing X-Forwarded-Host to be preserved, got %q", got.Get("X-Forwarded-Host"))
	}
	if got.Get("X-Forwarded-Proto") != "https" {
		t.Fatalf("expected existing X-Forwarded-Proto to be preserved, got %q", got.Get("X-Forwarded-Proto"))
	}
	if got.Get("X-Forwarded-Port") != "8443" {
		t.Fatalf("expected existing X-Forwarded-Port to be preserved, got %q", got.Get("X-Forwarded-Port"))
	}
	if got.Get("Forwarded") != `for="203.0.113.9";proto="http";host="public.example.test"` {
		t.Fatalf("unexpected Forwarded header: %q", got.Get("Forwarded"))
	}
	if values := got.Values("Via"); len(values) != 2 || values[0] != "1.1 edge" || values[1] != "1.1 harp" {
		t.Fatalf("unexpected Via chain: %#v", values)
	}
}

func TestForwardedPort(t *testing.T) {
	tests := []struct {
		name  string
		host  string
		proto string
		want  string
	}{
		{name: "explicit http port", host: "public.example.test:8080", proto: "http", want: "8080"},
		{name: "default http", host: "public.example.test", proto: "http", want: "80"},
		{name: "default https", host: "public.example.test", proto: "https", want: "443"},
		{name: "ipv6 without port", host: "::1", proto: "http", want: ""},
		{name: "ipv6 with port", host: "[::1]:8443", proto: "https", want: "8443"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := forwardedPort(tc.host, tc.proto); got != tc.want {
				t.Fatalf("forwardedPort(%q, %q) = %q, want %q", tc.host, tc.proto, got, tc.want)
			}
		})
	}
}

func TestRequestIDFromRequestUsesIncomingHeaderOrGeneratesID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://public.example.test/app", nil)
	req.Header.Set("X-Request-ID", " client-request-1 ")

	if got := requestIDFromRequest(req); got != "client-request-1" {
		t.Fatalf("expected incoming request id, got %q", got)
	}

	req = httptest.NewRequest(http.MethodGet, "http://public.example.test/app", nil)
	got := requestIDFromRequest(req)
	if strings.TrimSpace(got) == "" {
		t.Fatal("expected generated request id")
	}
}

func TestWebSocketForwardedRequestHeadersPreserveUpgradeHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://public.example.test/ws", nil)
	req.Host = "public.example.test"
	req.RemoteAddr = "203.0.113.11:1234"
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-Websocket-Key", "test-key")
	req.Header.Set("Sec-Websocket-Version", "13")
	req.Header.Set("X-Custom-Hop", "remove-me")
	req.Header.Add("Connection", "X-Custom-Hop")

	got := websocketForwardedRequestHeaders(req)

	if got.Get("Connection") != "Upgrade" {
		t.Fatalf("expected WebSocket Connection header to be preserved, got %q", got.Get("Connection"))
	}
	if got.Get("Upgrade") != "websocket" {
		t.Fatalf("expected WebSocket Upgrade header to be preserved, got %q", got.Get("Upgrade"))
	}
	if got.Get("X-Custom-Hop") != "" {
		t.Fatalf("expected custom Connection token header to be stripped, got %q", got.Get("X-Custom-Hop"))
	}
	if got.Get("Sec-Websocket-Key") != "test-key" {
		t.Fatalf("expected WebSocket key header to be preserved, got %q", got.Get("Sec-Websocket-Key"))
	}
	if got.Get("X-Forwarded-For") != "203.0.113.11" {
		t.Fatalf("unexpected X-Forwarded-For: %q", got.Get("X-Forwarded-For"))
	}
	if got.Get("X-Forwarded-Port") != "80" {
		t.Fatalf("unexpected X-Forwarded-Port: %q", got.Get("X-Forwarded-Port"))
	}
	if got.Get("Forwarded") != `for="203.0.113.11";proto="http";host="public.example.test"` {
		t.Fatalf("unexpected Forwarded header: %q", got.Get("Forwarded"))
	}
	if got.Get("Via") != "1.1 harp" {
		t.Fatalf("unexpected Via header: %q", got.Get("Via"))
	}
}

func TestFilterInternalHTTPHeadersStripsHopByHopResponseHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set("Content-Type", "text/plain")
	headers.Set("Connection", "X-Backend-Hop")
	headers.Set("X-Backend-Hop", "remove-me")
	headers.Set("Upgrade", "websocket")
	headers.Set(pb.StreamHeader, "true")

	got := filterInternalHTTPHeaders(headers)

	if got.Get("Content-Type") != "text/plain" {
		t.Fatalf("expected response content type to be preserved, got %q", got.Get("Content-Type"))
	}
	for _, key := range []string{"Connection", "X-Backend-Hop", "Upgrade", pb.StreamHeader} {
		if value := got.Get(key); value != "" {
			t.Fatalf("expected %s to be stripped, got %q", key, value)
		}
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
		AdminInsecureSkipAuth: true,
		LoadBalancingStrategy: loadBalancingRoundRobin,
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

func TestMetricsHandlerIncludesRouteMetrics(t *testing.T) {
	originalMetrics := metrics
	originalCache := cacheStore
	metrics = newTestMetrics()
	cacheStore = nil
	t.Cleanup(func() {
		metrics = originalMetrics
		cacheStore = originalCache
	})

	route := metrics.routeMetricsFor("/api")
	route.add(0, 3*time.Millisecond)
	rec := httptest.NewRecorder()
	metricsHandler(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	var body struct {
		RouteRequests     map[string]int64 `json:"route_requests"`
		RequestDurationMS map[string]int64 `json:"request_duration_ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid metrics response: %v", err)
	}
	if body.RouteRequests["/api"] != 1 || body.RequestDurationMS["/api"] != 3 {
		t.Fatalf("unexpected route metrics: %#v %#v", body.RouteRequests, body.RequestDurationMS)
	}
}

func TestAdminUIRejectsUnknownSubpath(t *testing.T) {
	originalConfig := config
	config.AdminPath = "/admin"
	config.AdminInsecureSkipAuth = true
	t.Cleanup(func() { config = originalConfig })

	rec := httptest.NewRecorder()
	adminUIHandler(rec, httptest.NewRequest(http.MethodGet, "/admin/unknown", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown admin subpath returned %d, want 404", rec.Code)
	}
}

func newTestMetrics() *Metrics {
	m := &Metrics{
		RequestsTotal:      new(expvar.Int),
		RequestDuration:    new(expvar.Map).Init(),
		CacheHits:          new(expvar.Int),
		CacheMisses:        new(expvar.Int),
		BackendErrors:      new(expvar.Int),
		ActiveConnections:  new(expvar.Int),
		BackendsRegistered: new(expvar.Int),
		RouteRequests:      new(expvar.Map).Init(),
		RateLimited:        new(expvar.Int),
		InflightRequests:   new(expvar.Int),
		ActiveWebSockets:   new(expvar.Int),
		ActiveStreams:      new(expvar.Int),
	}
	m.unmatched = m.routeMetricsFor("_unmatched")
	return m
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

func TestRouteHost(t *testing.T) {
	tests := map[string]string{
		"example.com":       "example.com",
		"example.com:8443":  "example.com",
		"127.0.0.1:8080":    "127.0.0.1",
		"[2001:db8::1]:443": "2001:db8::1",
		"[2001:db8::1]":     "2001:db8::1",
		"2001:db8::1":       "2001:db8::1",
	}
	for input, want := range tests {
		if got := routeHost(input); got != want {
			t.Errorf("routeHost(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMatchBackendUsesHostAndPath(t *testing.T) {
	origBackends := backends
	backendsMu.Lock()
	exampleBackend := &backendConn{
		routes: []registeredRoute{{
			name:          "example",
			domain:        `example\.com`,
			domainPattern: regexp.MustCompile(`^example\.com$`),
			path:          "/api",
			username:      "operator",
			password:      "route-password",
		}},
	}
	otherBackend := &backendConn{
		routes: []registeredRoute{{
			name:          "other",
			domain:        `other\.com`,
			domainPattern: regexp.MustCompile(`^other\.com$`),
			path:          "/api",
		}},
	}
	backends = map[string]*backendPool{
		backendKey(`example\.com`, "/api"): {
			route: exampleBackend.routes[0],
			conns: []*backendConn{exampleBackend},
		},
		backendKey(`other\.com`, "/api"): {
			route: otherBackend.routes[0],
			conns: []*backendConn{otherBackend},
		},
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

	got, route, _ = matchBackend("example.com", "/apix")
	if got != nil || route != "" {
		t.Fatalf("expected route segment boundary to reject /apix, got %#v route %q", got, route)
	}
}

func TestMatchesRegisteredRoute(t *testing.T) {
	tests := []struct {
		requestPath string
		routePath   string
		want        bool
	}{
		{"/", "/", true},
		{"/api", "/api", true},
		{"/api/users", "/api", true},
		{"/apix", "/api", false},
		{"/api/users", "/api/", true},
		{"/api", "/api/", false},
	}
	for _, tc := range tests {
		if got := matchesRegisteredRoute(tc.requestPath, tc.routePath); got != tc.want {
			t.Errorf("matchesRegisteredRoute(%q, %q) = %v, want %v", tc.requestPath, tc.routePath, got, tc.want)
		}
	}
}

func TestMatchBackendRoundRobinWithinRoutePool(t *testing.T) {
	origConfig := config
	origBackends := backends
	route := registeredRoute{
		name:    "api",
		domain:  `example\.com`,
		path:    "/api",
		pattern: regexp.MustCompile(`example\.com/api`),
	}
	first := &backendConn{routes: []registeredRoute{route}}
	second := &backendConn{routes: []registeredRoute{route}}
	backendsMu.Lock()
	backends = map[string]*backendPool{
		backendKey(`example\.com`, "/api"): {
			route: route,
			conns: []*backendConn{first, second},
		},
	}
	backendsMu.Unlock()
	t.Cleanup(func() {
		config = origConfig
		backendsMu.Lock()
		backends = origBackends
		backendsMu.Unlock()
	})
	config.LoadBalancingStrategy = loadBalancingRoundRobin

	got, routePath, _ := matchBackend("example.com", "/api/users")
	if got != first || routePath != "/api" {
		t.Fatalf("first request should select first backend, got %#v route %q", got, routePath)
	}
	got, routePath, _ = matchBackend("example.com", "/api/users")
	if got != second || routePath != "/api" {
		t.Fatalf("second request should select second backend, got %#v route %q", got, routePath)
	}
	got, routePath, _ = matchBackend("example.com", "/api/users")
	if got != first || routePath != "/api" {
		t.Fatalf("third request should wrap to first backend, got %#v route %q", got, routePath)
	}
}

func TestSelectBackendConnectionSkipsFailedConnections(t *testing.T) {
	originalConfig := config
	t.Cleanup(func() { config = originalConfig })
	failed := &backendConn{}
	failed.failed.Store(true)
	firstHealthy := &backendConn{}
	secondHealthy := &backendConn{}
	pool := &backendPool{conns: []*backendConn{failed, firstHealthy, secondHealthy}}

	config.LoadBalancingStrategy = loadBalancingFirst
	if got := selectBackendConnection(pool); got != firstHealthy {
		t.Fatalf("first strategy selected %#v, want first healthy connection", got)
	}

	config.LoadBalancingStrategy = loadBalancingRoundRobin
	if got := selectBackendConnection(pool); got != firstHealthy {
		t.Fatalf("round robin selected %#v, want first healthy connection", got)
	}
	if got := selectBackendConnection(pool); got != firstHealthy {
		t.Fatalf("round robin selected %#v while skipping failed slot", got)
	}
	if got := selectBackendConnection(pool); got != secondHealthy {
		t.Fatalf("round robin selected %#v, want second healthy connection", got)
	}
	firstHealthy.failed.Store(true)
	secondHealthy.failed.Store(true)
	if got := selectBackendConnection(pool); got != nil {
		t.Fatalf("all-failed pool selected %#v", got)
	}
}

func TestSelectBackendConnectionUsesLeastInflight(t *testing.T) {
	originalConfig := config
	config.LoadBalancingStrategy = loadBalancingLeast
	t.Cleanup(func() { config = originalConfig })
	busy := &backendConn{}
	idle := &backendConn{}
	medium := &backendConn{}
	busy.inflight.Store(8)
	idle.inflight.Store(1)
	medium.inflight.Store(4)
	pool := &backendPool{conns: []*backendConn{busy, idle, medium}}
	if got := selectBackendConnection(pool); got != idle {
		t.Fatalf("selected %#v, want least-loaded connection", got)
	}
	idle.failed.Store(true)
	if got := selectBackendConnection(pool); got != medium {
		t.Fatalf("selected %#v, want least-loaded healthy connection", got)
	}
}

func TestWebSocketSemaphoreIsIndependent(t *testing.T) {
	requestSlots := make(chan struct{}, 1)
	webSocketSlots := make(chan struct{}, 1)
	requestSlots <- struct{}{}
	rec := httptest.NewRecorder()
	if !acquireConcurrencySlot(rec, webSocketSlots, "full") {
		t.Fatal("free WebSocket slot was rejected because request slots were full")
	}
	if acquireConcurrencySlot(rec, webSocketSlots, "full") {
		t.Fatal("full WebSocket capacity accepted another connection")
	}
}

func TestBackendPoolCapacity(t *testing.T) {
	originalConfig := config
	originalBackends := backends
	originalIndex := backendIndex
	originalPathIndex := backendPathIndex
	originalIndexedPools := backendIndexedPools
	config.ConnectionPoolSize = 2
	backends = make(map[string]*backendPool)
	backendIndex = nil
	backendPathIndex = make(map[string][]*backendPool)
	backendIndexedPools = 0
	t.Cleanup(func() {
		config = originalConfig
		backends = originalBackends
		backendIndex = originalIndex
		backendPathIndex = originalPathIndex
		backendIndexedPools = originalIndexedPools
	})

	route := registeredRoute{domain: `example\.com`, path: "/api"}
	key := backendKey(route.domain, route.path)
	if !addBackendToPool(key, route, &backendConn{}) || !addBackendToPool(key, route, &backendConn{}) {
		t.Fatal("connections within pool capacity were rejected")
	}
	if addBackendToPool(key, route, &backendConn{}) {
		t.Fatal("connection beyond pool capacity was accepted")
	}
	if got := len(backends[key].conns); got != 2 {
		t.Fatalf("pool size = %d, want 2", got)
	}
}

func TestV1CompatibilityLeavesBackendPoolUnlimited(t *testing.T) {
	originalConfig := config
	config.CompatibilityMode = compatibilityModeV1
	config.ConnectionPoolSize = 1
	t.Cleanup(func() { config = originalConfig })

	if limit := backendConnectionPoolLimit(); limit != 0 {
		t.Fatalf("legacy backend pool limit = %d, want unlimited", limit)
	}
}

func TestV1CompatibilityMatchesCombinedDomainPathRegexp(t *testing.T) {
	originalConfig := config
	originalBackends := backends
	originalIndex := backendIndex
	originalPathIndex := backendPathIndex
	originalIndexedPools := backendIndexedPools
	config.CompatibilityMode = compatibilityModeV1
	backends = make(map[string]*backendPool)
	backendIndex = nil
	backendPathIndex = make(map[string][]*backendPool)
	backendIndexedPools = 0
	t.Cleanup(func() {
		config = originalConfig
		backends = originalBackends
		backendIndex = originalIndex
		backendPathIndex = originalPathIndex
		backendIndexedPools = originalIndexedPools
	})

	pattern := regexp.MustCompile(`example\.com/api/[0-9]+$`)
	route := registeredRoute{domain: `example\.com`, path: `/api/[0-9]+$`, pattern: pattern}
	conn := &backendConn{}
	if !addBackendToPool(backendKey(route.domain, route.path), route, conn) {
		t.Fatal("legacy route registration was rejected")
	}
	if backendIndexedPools != 0 {
		t.Fatalf("legacy regexp route entered literal-path index")
	}

	got, matched, _ := matchBackend("example.com:443", "/api/42")
	if got != conn || matched != route.path {
		t.Fatalf("legacy route match = (%p, %q), want (%p, %q)", got, matched, conn, route.path)
	}
	if got, _, _ := matchBackend("example.com", "/api/users"); got != nil {
		t.Fatal("legacy regexp route matched an invalid path")
	}
}

func TestValidateConfigRejectsUnknownCompatibilityMode(t *testing.T) {
	err := validateConfig(Config{CompatibilityMode: "v0", LoadBalancingStrategy: loadBalancingRoundRobin})
	if err == nil || !strings.Contains(err.Error(), "invalid compatibilityMode") {
		t.Fatalf("validateConfig error = %v, want invalid compatibilityMode", err)
	}
}

func TestIdempotentMethod(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodTrace} {
		if !isIdempotentMethod(method) {
			t.Errorf("%s should be retryable", method)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodConnect} {
		if isIdempotentMethod(method) {
			t.Errorf("%s must not be retried automatically", method)
		}
	}
}

func BenchmarkBackendPoolSelectionParallel(b *testing.B) {
	originalConfig := config
	config.LoadBalancingStrategy = loadBalancingRoundRobin
	b.Cleanup(func() { config = originalConfig })
	pool := &backendPool{conns: make([]*backendConn, 64)}
	for i := range pool.conns {
		pool.conns[i] = &backendConn{}
		if i%4 == 0 {
			pool.conns[i].failed.Store(true)
		}
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if selectBackendConnection(pool) == nil {
				b.Fatal("no healthy connection selected")
			}
		}
	})
}

func BenchmarkBackendPoolLeastConnectionsParallel(b *testing.B) {
	originalConfig := config
	config.LoadBalancingStrategy = loadBalancingLeast
	b.Cleanup(func() { config = originalConfig })
	pool := &backendPool{conns: make([]*backendConn, 64)}
	for i := range pool.conns {
		pool.conns[i] = &backendConn{}
		pool.conns[i].inflight.Store(int64(i%8 + 1))
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if selectBackendConnection(pool) == nil {
				b.Fatal("no healthy connection selected")
			}
		}
	})
}

func TestBackendIndexOrdersAndRemovesRoutes(t *testing.T) {
	originalBackends := backends
	originalIndex := backendIndex
	originalPathIndex := backendPathIndex
	originalIndexedPools := backendIndexedPools
	backendsMu.Lock()
	backends = make(map[string]*backendPool)
	backendIndex = nil
	backendPathIndex = make(map[string][]*backendPool)
	backendIndexedPools = 0
	backendsMu.Unlock()
	t.Cleanup(func() {
		backendsMu.Lock()
		backends = originalBackends
		backendIndex = originalIndex
		backendPathIndex = originalPathIndex
		backendIndexedPools = originalIndexedPools
		backendsMu.Unlock()
	})

	shortRoute := registeredRoute{path: "/api", pattern: regexp.MustCompile(`example\.com/api`)}
	longRoute := registeredRoute{path: "/api/admin", pattern: regexp.MustCompile(`example\.com/api/admin`)}
	shortConn := &backendConn{routes: []registeredRoute{shortRoute}}
	longConn := &backendConn{routes: []registeredRoute{longRoute}}
	backendsMu.Lock()
	addBackendToPool(backendKey(`example\.com`, shortRoute.path), shortRoute, shortConn)
	addBackendToPool(backendKey(`example\.com`, longRoute.path), longRoute, longConn)
	backendsMu.Unlock()

	if got, route, _ := matchBackend("example.com", "/api/admin/users"); got != longConn || route != longRoute.path {
		t.Fatalf("expected longest indexed route, got %#v route %q", got, route)
	}
	backendsMu.RLock()
	if len(backendIndex) != 2 || backendIndex[0].route.path != longRoute.path {
		t.Fatalf("backend index not ordered longest-first: %#v", backendIndex)
	}
	backendsMu.RUnlock()

	backendsMu.Lock()
	removeBackendFromPool(backendKey(`example\.com`, longRoute.path), longConn)
	backendsMu.Unlock()
	if got, route, _ := matchBackend("example.com", "/api/admin/users"); got != shortConn || route != shortRoute.path {
		t.Fatalf("expected shorter route after removal, got %#v route %q", got, route)
	}
}

func TestMatchBackendFirstStrategy(t *testing.T) {
	origConfig := config
	origBackends := backends
	route := registeredRoute{
		name:    "api",
		domain:  `example\.com`,
		path:    "/api",
		pattern: regexp.MustCompile(`example\.com/api`),
	}
	first := &backendConn{routes: []registeredRoute{route}}
	second := &backendConn{routes: []registeredRoute{route}}
	backendsMu.Lock()
	backends = map[string]*backendPool{
		backendKey(`example\.com`, "/api"): {
			route: route,
			conns: []*backendConn{first, second},
		},
	}
	backendsMu.Unlock()
	t.Cleanup(func() {
		config = origConfig
		backendsMu.Lock()
		backends = origBackends
		backendsMu.Unlock()
	})
	config.LoadBalancingStrategy = loadBalancingFirst

	for i := 0; i < 3; i++ {
		got, routePath, _ := matchBackend("example.com", "/api/users")
		if got != first || routePath != "/api" {
			t.Fatalf("request %d should select first backend, got %#v route %q", i+1, got, routePath)
		}
	}
}

func TestRegisteredRoutesSnapshotIncludesBackendPoolSize(t *testing.T) {
	origBackends := backends
	route := registeredRoute{
		name:     "api",
		domain:   `example\.com`,
		path:     "/api",
		pattern:  regexp.MustCompile(`example\.com/api`),
		password: "route-password",
	}
	backendsMu.Lock()
	backends = map[string]*backendPool{
		backendKey(`example\.com`, "/api"): {
			route: route,
			conns: []*backendConn{{}, {}},
		},
	}
	backendsMu.Unlock()
	t.Cleanup(func() {
		backendsMu.Lock()
		backends = origBackends
		backendsMu.Unlock()
	})

	routes := registeredRoutesSnapshot()
	if len(routes) != 1 {
		t.Fatalf("expected one route snapshot, got %#v", routes)
	}
	if routes[0].Backends != 2 {
		t.Fatalf("expected backend pool size 2, got %d", routes[0].Backends)
	}
	if !routes[0].Protected {
		t.Fatal("expected protected route snapshot")
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
