// demos/remote-helper-go/main.go
//
// Remote Helper Demo
//
// This demo shows how to use harpserver.RemoteHelper to create a lightweight
// "remote helper" that runs on a home network (e.g. a MacBook) and exposes
// local resources – such as Home Assistant state or local scripts – through a
// publicly hosted HARP proxy.
//
// Architecture:
//   Public server: HARP proxy  (listens on :8080 HTTP, :50054 gRPC)
//   Home network : This helper (connects OUT to the proxy, no inbound ports needed)
//
// Usage:
//
//	go run main.go -proxy <HARP-proxy-address>
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/SimonWaldherr/HARP/harpserver"
)

var proxyAddr = flag.String("proxy", "localhost:50054", "Address of the HARP proxy gRPC server")

func main() {
	flag.Parse()

	helper := &harpserver.RemoteHelper{
		Name:              "HomeNetworkHelper",
		ProxyURL:          *proxyAddr,
		Key:               "master-key",
		Domain:            ".*",
		ReconnectInterval: 5 * time.Second,
	}

	// /helper/status – returns basic system info (no local processes needed).
	helper.Register("/helper/status", "HelperStatus", func(r *http.Request) (int, map[string]string, string) {
		info := map[string]interface{}{
			"helper":    "HomeNetworkHelper",
			"os":        runtime.GOOS,
			"arch":      runtime.GOARCH,
			"timestamp": time.Now().Format(time.RFC3339),
			"uptime":    "running",
		}
		body, _ := json.Marshal(info)
		return 200, map[string]string{"Content-Type": "application/json"}, string(body)
	})

	// /helper/homeassistant – simulates querying a local Home Assistant instance.
	// In a real deployment this would call http://localhost:8123/api/...
	helper.Register("/helper/homeassistant", "HomeAssistant", func(r *http.Request) (int, map[string]string, string) {
		entity := r.URL.Query().Get("entity")
		if entity == "" {
			entity = "light.living_room"
		}
		// Simulated Home Assistant state response.
		state := map[string]interface{}{
			"entity_id":    entity,
			"state":        "on",
			"last_changed": time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
			"attributes": map[string]interface{}{
				"brightness":  200,
				"color_temp":  370,
				"friendly_name": strings.ReplaceAll(entity, "_", " "),
			},
		}
		body, _ := json.Marshal(state)
		return 200, map[string]string{"Content-Type": "application/json"}, string(body)
	})

	// /helper/exec – executes a safe, pre-defined local command.
	// Only a fixed allow-list of commands is permitted to avoid arbitrary execution.
	helper.Register("/helper/exec", "LocalExec", func(r *http.Request) (int, map[string]string, string) {
		cmd := r.URL.Query().Get("cmd")
		allowed := map[string][]string{
			"hostname": {"hostname"},
			"date":     {"date"},
			"uptime":   {"uptime"},
		}
		args, ok := allowed[cmd]
		if !ok {
			body, _ := json.Marshal(map[string]string{
				"error": fmt.Sprintf("command %q is not in the allow-list", cmd),
			})
			return 400, map[string]string{"Content-Type": "application/json"}, string(body)
		}
		out, err := exec.Command(args[0], args[1:]...).Output() // #nosec G204 – allow-listed only
		if err != nil {
			body, _ := json.Marshal(map[string]string{"error": err.Error()})
			return 500, map[string]string{"Content-Type": "application/json"}, string(body)
		}
		body, _ := json.Marshal(map[string]string{
			"command": cmd,
			"output":  strings.TrimSpace(string(out)),
		})
		return 200, map[string]string{"Content-Type": "application/json"}, string(body)
	})

	log.Printf("Starting HomeNetworkHelper, connecting to HARP proxy at %s", *proxyAddr)
	log.Println("Registered routes:")
	log.Println("  GET /helper/status              – system info")
	log.Println("  GET /helper/homeassistant?entity=<id> – Home Assistant state")
	log.Println("  GET /helper/exec?cmd=<hostname|date|uptime> – safe local commands")

	if err := helper.ListenAndServe(); err != nil {
		log.Fatalf("Helper error: %v", err)
	}
}
