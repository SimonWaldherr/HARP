// harpctl is a small operations helper for a running HARP proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const defaultProxyHTTPAddr = "http://localhost:8080"

type exitCode int

const (
	exitOK exitCode = iota
	exitUsage
	exitFailure
)

func main() {
	os.Exit(int(run(os.Args[1:], os.Stdout, os.Stderr)))
}

func run(args []string, stdout, stderr io.Writer) exitCode {
	if len(args) == 0 {
		printUsage(stderr)
		return exitUsage
	}

	cmd := args[0]
	switch cmd {
	case "health", "ready", "live", "metrics":
		return runGet(cmd, args[1:], stdout, stderr)
	case "wait":
		return runWait(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
		printUsage(stderr)
		return exitUsage
	}
}

func runGet(cmd string, args []string, stdout, stderr io.Writer) exitCode {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultProxyHTTPAddr, "HARP HTTP proxy base URL")
	timeout := fs.Duration("timeout", 5*time.Second, "HTTP request timeout")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "%s does not accept positional arguments\n", cmd)
		return exitUsage
	}

	path := commandPath(cmd)
	body, statusCode, err := fetch(context.Background(), http.DefaultClient, *addr, path, *timeout)
	if err != nil {
		fmt.Fprintf(stderr, "%s failed: %v\n", cmd, err)
		return exitFailure
	}
	fmt.Fprintln(stdout, strings.TrimRight(string(body), "\n"))
	if statusCode < 200 || statusCode >= 300 {
		return exitFailure
	}
	return exitOK
}

func runWait(args []string, stdout, stderr io.Writer) exitCode {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultProxyHTTPAddr, "HARP HTTP proxy base URL")
	timeout := fs.Duration("timeout", 30*time.Second, "maximum time to wait")
	interval := fs.Duration("interval", time.Second, "delay between readiness checks")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "wait does not accept positional arguments")
		return exitUsage
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "timeout must be positive")
		return exitUsage
	}
	if *interval <= 0 {
		fmt.Fprintln(stderr, "interval must be positive")
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	for {
		body, statusCode, err := fetch(ctx, http.DefaultClient, *addr, "/readyz", *interval)
		if err == nil && statusCode >= 200 && statusCode < 300 {
			fmt.Fprintln(stdout, strings.TrimRight(string(body), "\n"))
			return exitOK
		}

		select {
		case <-ctx.Done():
			fmt.Fprintf(stderr, "HARP did not become ready within %s\n", timeout.String())
			return exitFailure
		case <-ticker.C:
		}
	}
}

func fetch(ctx context.Context, client *http.Client, baseURL, path string, timeout time.Duration) ([]byte, int, error) {
	endpoint, err := joinURL(baseURL, path)
	if err != nil {
		return nil, 0, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func joinURL(baseURL, path string) (string, error) {
	if strings.TrimSpace(baseURL) == "" {
		return "", errors.New("addr must not be empty")
	}
	if !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid addr %q", baseURL)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + path
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func commandPath(cmd string) string {
	switch cmd {
	case "health":
		return "/healthz"
	case "ready":
		return "/readyz"
	case "live":
		return "/livez"
	case "metrics":
		return "/metrics"
	default:
		return "/healthz"
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  harpctl health  [-addr http://localhost:8080]
  harpctl ready   [-addr http://localhost:8080]
  harpctl live    [-addr http://localhost:8080]
  harpctl metrics [-addr http://localhost:8080]
  harpctl wait    [-addr http://localhost:8080] [-timeout 30s] [-interval 1s]

Commands:
  health   Fetch /healthz.
  ready    Fetch /readyz.
  live     Fetch /livez.
  metrics  Fetch /metrics.
  wait     Poll /readyz until the proxy is ready or the timeout expires.
`)
}
