package main

import (
	"testing"
	"time"
)

func TestNewUpstreamTransportConcurrencyDefaults(t *testing.T) {
	transport := newUpstreamTransport(GatewayConfig{})
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("expected HTTP/2 to be enabled for concurrent upstream requests")
	}
	if transport.MaxIdleConns != 200 {
		t.Fatalf("MaxIdleConns = %d, want 200", transport.MaxIdleConns)
	}
	if transport.MaxIdleConnsPerHost != 100 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 100", transport.MaxIdleConnsPerHost)
	}
	if transport.IdleConnTimeout != 90*time.Second {
		t.Fatalf("IdleConnTimeout = %s, want 90s", transport.IdleConnTimeout)
	}
	if transport.TLSHandshakeTimeout != 10*time.Second {
		t.Fatalf("TLSHandshakeTimeout = %s, want 10s", transport.TLSHandshakeTimeout)
	}
}

func TestNewUpstreamTransportHonorsPoolLimits(t *testing.T) {
	transport := newUpstreamTransport(GatewayConfig{
		UpstreamMaxIdleConns:        50,
		UpstreamMaxIdleConnsPerHost: 25,
		UpstreamMaxConnsPerHost:     40,
	})
	if transport.MaxIdleConns != 50 || transport.MaxIdleConnsPerHost != 25 || transport.MaxConnsPerHost != 40 {
		t.Fatalf("unexpected configured pool limits: %#v", transport)
	}
}
