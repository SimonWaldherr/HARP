.PHONY: all build build-proxy build-gateway build-demos fmt fmt-all vet vet-all lint test test-all test-verbose test-cover \
       run run-proxy run-gateway run-demo-simple run-demo-complex run-demo-enhanced \
       run-demo-multi run-demo-remote-helper run-demo-static demo demo-admin clean proto help

# ── Variables ────────────────────────────────────────────────────────────────
BINARY       := bin/harp-proxy
GATEWAY      := bin/harp-gateway
GO           := go
GOFLAGS      ?=
CONFIG       ?= config.json
ADMIN_CONFIG ?= demos/admin-ui/config.json
GATEWAY_CFG  ?= cmd/harp-gateway/gateway-example.json
PROXY_ADDR   ?= localhost:50054

# Demo binaries
DEMOS := simple-go complex-harp-server enhanced-go multi-service-go static-wrapper-go
MODULE_DIRS := . harp harpserver demos/remote-helper-go demos/advanced-enterprise

# ── Default target ───────────────────────────────────────────────────────────
all: fmt-all vet-all test-all build ## Format, vet, test, and build everything

# ── Build ────────────────────────────────────────────────────────────────────
build: build-proxy build-gateway build-demos ## Build proxy, gateway, and all demos

build-proxy: ## Build the HARP proxy binary
	$(GO) build $(GOFLAGS) -o $(BINARY) .

build-gateway: ## Build the harp-gateway agent
	$(GO) build $(GOFLAGS) -o $(GATEWAY) ./cmd/harp-gateway

build-demos: ## Build all demo backends
	@for demo in $(DEMOS); do \
		echo "Building demos/$$demo ..."; \
		$(GO) build $(GOFLAGS) -o bin/demo-$$demo ./demos/$$demo; \
	done
	@if [ -f demos/remote-helper-go/go.mod ]; then \
		echo "Building demos/remote-helper-go ..."; \
		cd demos/remote-helper-go && $(GO) build $(GOFLAGS) -o ../../bin/demo-remote-helper-go .; \
	fi

# ── Code quality ─────────────────────────────────────────────────────────────
fmt: ## Run gofmt on all Go files
	$(GO) fmt ./...

fmt-all: ## Run gofmt in every workspace module
	@set -e; for dir in $(MODULE_DIRS); do \
		echo "Formatting $$dir ..."; \
		(cd $$dir && $(GO) fmt ./...); \
	done

vet: ## Run go vet on all packages
	$(GO) vet ./...

vet-all: ## Run go vet in every workspace module
	@set -e; for dir in $(MODULE_DIRS); do \
		echo "Vetting $$dir ..."; \
		(cd $$dir && $(GO) vet ./...); \
	done

lint: ## Run staticcheck (install with: go install honnef.co/go/tools/cmd/staticcheck@latest)
	@command -v staticcheck >/dev/null 2>&1 || { echo "staticcheck not installed, skipping"; exit 0; }
	staticcheck ./...

# ── Testing ──────────────────────────────────────────────────────────────────
test: ## Run all tests
	$(GO) test ./...

test-all: ## Run tests in every workspace module
	@set -e; for dir in $(MODULE_DIRS); do \
		echo "Testing $$dir ..."; \
		(cd $$dir && $(GO) test ./...); \
	done

test-verbose: ## Run all tests with verbose output
	$(GO) test -v -count=1 ./...

test-cover: ## Run tests with coverage report
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out
	@echo ""
	@echo "To view HTML report: go tool cover -html=coverage.out"

test-race: ## Run tests with race detector
	$(GO) test -race ./...

# ── Run ──────────────────────────────────────────────────────────────────────
run: run-proxy ## Alias for run-proxy

run-proxy: build-proxy ## Build and run the HARP proxy
	./$(BINARY) -config $(CONFIG)

run-gateway: build-gateway ## Build and run the harp-gateway agent
	./$(GATEWAY) -config $(GATEWAY_CFG)

run-demo-simple: ## Run the simple-go demo backend (proxy must be running)
	$(GO) run ./demos/simple-go -proxy $(PROXY_ADDR)

run-demo-complex: ## Run the complex-harp-server demo
	$(GO) run ./demos/complex-harp-server -proxy $(PROXY_ADDR)

run-demo-enhanced: ## Run the enhanced-go demo
	$(GO) run ./demos/enhanced-go -proxy $(PROXY_ADDR)

run-demo-multi: ## Run the multi-service-go demo
	$(GO) run ./demos/multi-service-go -proxy $(PROXY_ADDR)

run-demo-remote-helper: ## Run the remote-helper-go demo
	cd demos/remote-helper-go && $(GO) run . -proxy $(PROXY_ADDR)

run-demo-static: ## Run the static-wrapper-go demo
	$(GO) run ./demos/static-wrapper-go -proxy $(PROXY_ADDR)

# ── Demo: full workflow ──────────────────────────────────────────────────────
demo: build-proxy ## Run proxy + simple demo, then curl a test request
	@echo "=== Starting HARP proxy ==="
	@./$(BINARY) -config $(CONFIG) &
	@PROXY_PID=$$!; \
	trap 'kill $$DEMO_PID $$PROXY_PID 2>/dev/null || true' EXIT INT TERM; \
	sleep 1; \
	echo "=== Starting simple-go demo backend ==="; \
	$(GO) run ./demos/simple-go -proxy $(PROXY_ADDR) & \
	DEMO_PID=$$!; \
	sleep 1; \
	echo ""; \
	echo "=== Sending test request ==="; \
	curl -s http://localhost:8080/test || true; \
	echo ""; \
	echo ""; \
	echo "=== Health check ==="; \
	curl -s http://localhost:8080/health | python3 -m json.tool 2>/dev/null || curl -s http://localhost:8080/health; \
	echo ""; \
	echo "=== Cleaning up ==="; \
	kill $$DEMO_PID 2>/dev/null; \
	kill $$PROXY_PID 2>/dev/null; \
	echo "Done."

demo-admin: build-proxy ## Run proxy + backend with the admin UI enabled
	@echo "=== Starting HARP proxy with admin UI ==="
	@./$(BINARY) -config $(ADMIN_CONFIG) &
	@PROXY_PID=$$!; \
	trap 'kill $$DEMO_PID $$PROXY_PID 2>/dev/null || true' EXIT INT TERM; \
	sleep 1; \
	echo "=== Starting simple-go demo backend ==="; \
	$(GO) run ./demos/simple-go -proxy $(PROXY_ADDR) & \
	DEMO_PID=$$!; \
	sleep 1; \
	echo ""; \
	echo "=== Sending proxied request ==="; \
	curl -s http://localhost:8080/test || true; \
	echo ""; \
	echo ""; \
	echo "=== Admin API ==="; \
	curl -s http://localhost:8080/admin/api/status | python3 -m json.tool 2>/dev/null || curl -s http://localhost:8080/admin/api/status; \
	echo ""; \
	echo "Admin UI: http://localhost:8080/admin"; \
	echo "=== Cleaning up ==="; \
	kill $$DEMO_PID 2>/dev/null; \
	kill $$PROXY_PID 2>/dev/null; \
	echo "Done."

# ── Protobuf ─────────────────────────────────────────────────────────────────
proto: ## Regenerate protobuf/gRPC code from harp.proto
	protoc --go_out=. --go_opt=paths=source_relative \
	       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	       harp/harp.proto

# ── Maintenance ──────────────────────────────────────────────────────────────
clean: ## Remove build artifacts
	rm -f $(BINARY) $(GATEWAY) coverage.out
	rm -f bin/demo-*

deps: ## Download and tidy dependencies
	$(GO) mod download
	$(GO) mod tidy

# ── Help ─────────────────────────────────────────────────────────────────────
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2}'
