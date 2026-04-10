// cmd/harp-gateway/main.go
// harp-gateway is a standalone agent that reads a JSON configuration file and
// exposes local HTTP services through a remote HARP proxy. Install it on a
// Raspberry Pi or home server and describe your services in a config file —
// no Go code required.
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/SimonWaldherr/HARP/harpserver"
)

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

	// Create an HTTP client with sensible defaults for local-network calls.
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: false},
			MaxIdleConnsPerHost: 10,
		},
	}

	for _, svc := range cfg.Services {
		svc := svc // capture loop variable
		timeout := 30 * time.Second
		if svc.TimeoutSeconds > 0 {
			timeout = time.Duration(svc.TimeoutSeconds) * time.Second
		}

		log.Printf("Registering service %q: %s -> %s (stripPrefix=%v)", svc.Name, svc.Route, svc.Upstream, svc.StripPrefix)

		helper.Register(svc.Route, svc.Name, func(r *http.Request) (int, map[string]string, string) {
			// Build the upstream URL.
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

			// Build upstream request.
			var body io.Reader
			if r.Body != nil {
				body = r.Body
			}
			upReq, err := http.NewRequest(r.Method, upstreamURL, body)
			if err != nil {
				log.Printf("[%s] Error building upstream request: %v", svc.Name, err)
				return http.StatusBadGateway, nil, fmt.Sprintf("gateway error: %v", err)
			}

			// Copy original headers.
			for k, vals := range r.Header {
				for _, v := range vals {
					upReq.Header.Add(k, v)
				}
			}
			// Add configured extra headers.
			for k, v := range svc.AddHeaders {
				upReq.Header.Set(k, v)
			}

			client := *httpClient
			client.Timeout = timeout

			resp, err := client.Do(upReq)
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

			// Collect response headers.
			respHeaders := make(map[string]string)
			for k, v := range resp.Header {
				if len(v) > 0 {
					respHeaders[k] = v[0]
				}
			}

			return resp.StatusCode, respHeaders, string(respBody)
		})
	}

	log.Printf("harp-gateway %q connecting to %s with %d service(s)...", cfg.Name, cfg.ProxyURL, len(cfg.Services))
	log.Fatal(helper.ListenAndServe())
}
