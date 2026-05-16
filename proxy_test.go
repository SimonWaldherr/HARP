package main

import (
	"os"
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
		streamHeader:        "1",
		streamEndHeader:     "1",
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
	if _, ok := filtered[streamHeader]; ok {
		t.Fatalf("expected %s to be removed", streamHeader)
	}
	if _, ok := filtered[streamEndHeader]; ok {
		t.Fatalf("expected %s to be removed", streamEndHeader)
	}
	if _, ok := filtered["x-harp-stream-end"]; ok {
		t.Fatalf("expected case-insensitive stream-end header to be removed")
	}
}
