// cmd/harp-gateway/main.go
// harp-gateway is a standalone agent that reads a JSON configuration file and
// exposes local HTTP services through a remote HARP proxy. Install it on a
// Raspberry Pi or home server and describe your services in a config file —
// no Go code required.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/SimonWaldherr/HARP/harpserver"
)

const streamChunkSize = 32 * 1024

var streamBufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, streamChunkSize)
	},
}

// GatewayConfig is the top-level configuration for the gateway agent.
type GatewayConfig struct {
	// Name identifies this gateway on the HARP proxy.
	Name string `json:"name"`
	// ProxyURL is the gRPC address of the HARP proxy, e.g. "proxy.example.com:50054".
	ProxyURL string `json:"proxyURL"`
	// Key is the authentication key for route registration.
	Key string `json:"key"`
	// Domain is the regex matched against incoming request domains (default ".*").
	Domain string `json:"domain"`
	// ReconnectInterval is the delay between reconnect attempts (default "5s").
	ReconnectInterval string `json:"reconnectInterval"`
	// Services lists the local services to expose.
	Services []ServiceConfig `json:"services"`
	// UpstreamMaxIdleConns sets Transport.MaxIdleConns (default 200).
	UpstreamMaxIdleConns int `json:"upstreamMaxIdleConns"`
	// UpstreamMaxIdleConnsPerHost sets Transport.MaxIdleConnsPerHost (default 100).
	UpstreamMaxIdleConnsPerHost int `json:"upstreamMaxIdleConnsPerHost"`
	// UpstreamMaxConnsPerHost sets Transport.MaxConnsPerHost (default 0 = unlimited).
	UpstreamMaxConnsPerHost int `json:"upstreamMaxConnsPerHost"`
}

// ServiceConfig describes a single local service to expose via HARP.
type ServiceConfig struct {
	// Name is a human-readable label for this service.
	Name string `json:"name"`
	// Route is the public URL path prefix registered with the proxy (e.g. "/homeassistant/").
	Route string `json:"route"`
	// Upstream is the local HTTP base URL to forward requests to (e.g. "http://localhost:8123").
	Upstream string `json:"upstream"`
	// StripPrefix removes the route prefix before forwarding to the upstream.
	// For example, with route="/ha/" and stripPrefix=true, a request to /ha/api/states
	// is forwarded to http://upstream/api/states.
	StripPrefix bool `json:"stripPrefix"`
	// AddHeaders are extra headers added to every request forwarded to the upstream.
	AddHeaders map[string]string `json:"addHeaders"`
	// TimeoutSeconds is the per-request timeout for the upstream call (default 30).
	TimeoutSeconds int `json:"timeoutSeconds"`
	// Streaming enables chunked forwarding for long-lived responses (SSE/token streams).
	Streaming bool `json:"streaming"`
}

func main() {
	configPath := flag.String("config", "gateway.json", "Path to gateway configuration file")
	flag.Parse()

	data, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("Failed to read config file %q: %v", *configPath, err)
	}

	var cfg GatewayConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("Failed to parse config: %v", err)
	}

	if cfg.Name == "" {
		cfg.Name = "harp-gateway"
	}
	if cfg.Domain == "" {
		cfg.Domain = ".*"
	}
	if len(cfg.Services) == 0 {
		log.Fatal("No services defined in config")
	}

	reconnect := 5 * time.Second
	if cfg.ReconnectInterval != "" {
		if d, err := time.ParseDuration(cfg.ReconnectInterval); err == nil {
			reconnect = d
		}
	}

	helper := &harpserver.RemoteHelper{
		Name:              cfg.Name,
		ProxyURL:          cfg.ProxyURL,
		Key:               cfg.Key,
		Domain:            cfg.Domain,
		ReconnectInterval: reconnect,
	}

	maxIdleConns := cfg.UpstreamMaxIdleConns
	if maxIdleConns <= 0 {
		maxIdleConns = 200
	}
	maxIdleConnsPerHost := cfg.UpstreamMaxIdleConnsPerHost
	if maxIdleConnsPerHost <= 0 {
		maxIdleConnsPerHost = 100
	}

	// Create an HTTP client with concurrency-friendly defaults for local upstreams.
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: false},
			MaxIdleConns:        maxIdleConns,
			MaxIdleConnsPerHost: maxIdleConnsPerHost,
			MaxConnsPerHost:     cfg.UpstreamMaxConnsPerHost,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	for _, svc := range cfg.Services {
		svc := svc // capture loop variable
		timeout := 30 * time.Second
		if svc.TimeoutSeconds > 0 {
			timeout = time.Duration(svc.TimeoutSeconds) * time.Second
		}

		log.Printf("Registering service %q: %s -> %s (stripPrefix=%v)", svc.Name, svc.Route, svc.Upstream, svc.StripPrefix)

		if svc.Streaming {
			helper.RegisterStream(svc.Route, svc.Name, func(
				r *http.Request,
				send func(statusCode int, headers map[string]string, body string, end bool) error,
			) error {
				upReq, err := buildUpstreamRequest(r, svc)
				if err != nil {
					return err
				}
				cancel := func() {}
				if svc.TimeoutSeconds > 0 {
					upReq, cancel = withRequestTimeout(upReq, timeout)
					defer cancel()
				}
				resp, err := httpClient.Do(upReq)
				if err != nil {
					return fmt.Errorf("upstream error: %w", err)
				}
				defer resp.Body.Close()

				respHeaders := extractResponseHeaders(resp.Header)
				delete(respHeaders, "Content-Length")

				buf := streamBufferPool.Get().([]byte)
				defer streamBufferPool.Put(buf)
				firstChunkSent := false
				for {
					n, readErr := resp.Body.Read(buf)
					if n > 0 {
						chunkHeaders := map[string]string(nil)
						if !firstChunkSent {
							chunkHeaders = respHeaders
						}
						if err := send(resp.StatusCode, chunkHeaders, string(buf[:n]), false); err != nil {
							return err
						}
						firstChunkSent = true
					}
					if readErr == io.EOF {
						endHeaders := map[string]string(nil)
						if !firstChunkSent {
							endHeaders = respHeaders
						}
						return send(resp.StatusCode, endHeaders, "", true)
					}
					if readErr != nil {
						return fmt.Errorf("stream read error: %w", readErr)
					}
				}
			})
			continue
		}

		helper.Register(svc.Route, svc.Name, func(r *http.Request) (int, map[string]string, string) {
			upReq, err := buildUpstreamRequest(r, svc)
			if err != nil {
				log.Printf("[%s] Error building upstream request: %v", svc.Name, err)
				return http.StatusBadGateway, nil, fmt.Sprintf("gateway error: %v", err)
			}
			upReq, cancel := withRequestTimeout(upReq, timeout)
			defer cancel()
			resp, err := httpClient.Do(upReq)
			if err != nil {
				log.Printf("[%s] Upstream error: %v", svc.Name, err)
				return http.StatusBadGateway, nil, fmt.Sprintf("upstream error: %v", err)
			}
			defer resp.Body.Close()

			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Printf("[%s] Error reading upstream response: %v", svc.Name, err)
				return http.StatusBadGateway, nil, "error reading upstream response"
			}

			return resp.StatusCode, extractResponseHeaders(resp.Header), string(respBody)
		})
	}

	log.Printf("harp-gateway %q connecting to %s with %d service(s)...", cfg.Name, cfg.ProxyURL, len(cfg.Services))
	log.Fatal(helper.ListenAndServe())
}

func buildUpstreamRequest(r *http.Request, svc ServiceConfig) (*http.Request, error) {
	path := r.URL.Path
	if svc.StripPrefix {
		path = strings.TrimPrefix(path, strings.TrimSuffix(svc.Route, "/"))
		if path == "" {
			path = "/"
		}
	}
	upstreamURL := strings.TrimSuffix(svc.Upstream, "/") + path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	var body io.Reader
	if r.Body != nil {
		body = r.Body
	}
	upReq, err := http.NewRequest(r.Method, upstreamURL, body)
	if err != nil {
		return nil, err
	}
	for k, vals := range r.Header {
		for _, v := range vals {
			upReq.Header.Add(k, v)
		}
	}
	for k, v := range svc.AddHeaders {
		upReq.Header.Set(k, v)
	}
	return upReq, nil
}

func extractResponseHeaders(h http.Header) map[string]string {
	respHeaders := make(map[string]string)
	for k, v := range h {
		if len(v) > 0 {
			respHeaders[k] = v[0]
		}
	}
	return respHeaders
}

func withRequestTimeout(req *http.Request, timeout time.Duration) (*http.Request, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	return req.WithContext(ctx), cancel
}
