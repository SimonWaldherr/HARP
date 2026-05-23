// demos/sse-go/app.go
//
// Run a HARP proxy first, then start this demo:
//
//	go run ./demos/sse-go -proxy localhost:50054
//
// Test through the HARP HTTP proxy:
//
//	curl -N http://localhost:8080/events
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/SimonWaldherr/HARP/harpserver"
)

var proxyAddr = flag.String("proxy", "localhost:50054", "Address of the HARP proxy gRPC server")

func main() {
	flag.Parse()

	helper := &harpserver.RemoteHelper{
		Name:              "SSEDemo",
		ProxyURL:          *proxyAddr,
		Key:               "master-key",
		Domain:            ".*",
		ReconnectInterval: 5 * time.Second,
	}

	helper.RegisterSSE("/events", "Events", func(
		r *http.Request,
		send func(statusCode int, headers map[string]string, body string, end bool) error,
	) error {
		for i := 1; i <= 10; i++ {
			event := fmt.Sprintf("event: tick\ndata: {\"count\":%d,\"time\":%q}\n\n", i, time.Now().Format(time.RFC3339))
			if err := send(http.StatusOK, nil, event, false); err != nil {
				return err
			}
			time.Sleep(time.Second)
		}
		return send(http.StatusOK, nil, "event: done\ndata: {}\n\n", true)
	})

	log.Printf("SSE demo connecting to HARP proxy at %s", *proxyAddr)
	log.Println("Registered route: GET /events")
	log.Println("Test with: curl -N http://localhost:8080/events")

	if err := helper.ListenAndServe(); err != nil {
		log.Fatalf("SSE demo failed: %v", err)
	}
}
