package main

import (
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
}
