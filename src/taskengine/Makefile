.PHONY: build test clean run-server run-worker reload status e2e cross-compile ensure-bin

BINARY_NAME=bin/taskengine
SERVER_URL ?= http://localhost:8080
WORKER_ID ?=
CONCURRENCY ?= 0
PORT ?= 8080

# Use external drive for Go build temp directory if available
ifneq ($(wildcard /Volumes/drive001/tmp),)
export GOTMPDIR := /Volumes/drive001/tmp
endif

OS := $(shell uname -s | tr '[:upper:]' '[:lower:]')
ARCH_RAW := $(shell uname -m)
ifeq ($(ARCH_RAW),x86_64)
ARCH := amd64
else ifeq ($(ARCH_RAW),arm64)
ARCH := arm64
else ifeq ($(ARCH_RAW),aarch64)
ARCH := arm64
else
ARCH := $(ARCH_RAW)
endif

ensure-bin:
	@mkdir -p bin
	@if [ -x $(BINARY_NAME) ]; then \
		exit 0; \
	elif [ -f "bin/taskengine_$(OS)_$(ARCH)" ]; then \
		echo "==> Using pre-built binary for $(OS)/$(ARCH)..."; \
		cp "bin/taskengine_$(OS)_$(ARCH)" $(BINARY_NAME); \
		chmod +x $(BINARY_NAME); \
	elif command -v go >/dev/null 2>&1; then \
		echo "==> Building TaskEngine binary via Go..."; \
		go build -o $(BINARY_NAME) ./cmd/taskengine; \
	elif [ -n "$(SERVER_URL)" ] && curl -sSL -f "$(SERVER_URL)/api/v1/binaries/$(OS)_$(ARCH)" -o $(BINARY_NAME) 2>/dev/null; then \
		echo "==> Downloaded TaskEngine binary for $(OS)/$(ARCH) from $(SERVER_URL)..."; \
		chmod +x $(BINARY_NAME); \
	else \
		echo "ERROR: TaskEngine binary '$(BINARY_NAME)' not found."; \
		echo "Go is not installed on this machine and could not download binary from SERVER_URL ($(SERVER_URL))."; \
		echo "Please either:"; \
		echo "  1. Run 'make taskengine-release' on the server machine, or"; \
		echo "  2. Pass a reachable SERVER_URL (e.g. make taskengine-worker SERVER_URL=http://<server-ip>:8080), or"; \
		echo "  3. Install Go on this machine."; \
		exit 1; \
	fi

build:
	@mkdir -p bin
	@if command -v go >/dev/null 2>&1; then \
		go build -o $(BINARY_NAME) ./cmd/taskengine; \
	else \
		$(MAKE) ensure-bin; \
	fi

test:
	go test -v ./...

clean:
	rm -rf bin/ data/ *.db *.db-wal *.db-shm /tmp/taskengine_*

run-server: ensure-bin
	./$(BINARY_NAME) server --port $(PORT) --tasks-dir ../../tasks

run-worker: ensure-bin
	@ARGS="--server-url $(SERVER_URL)"; \
	if [ -n "$(WORKER_ID)" ]; then ARGS="$$ARGS --worker-id $(WORKER_ID)"; fi; \
	if [ "$(CONCURRENCY)" -gt 0 ]; then ARGS="$$ARGS --concurrency $(CONCURRENCY)"; fi; \
	if [ -n "$(PLUGINS)" ]; then ARGS="$$ARGS --plugins $(PLUGINS)"; fi; \
	./$(BINARY_NAME) worker $$ARGS

reload: ensure-bin
	./$(BINARY_NAME) reload --server-url $(SERVER_URL)

status: ensure-bin
	./$(BINARY_NAME) status --server-url $(SERVER_URL)

e2e: build
	./test_e2e.sh

cross-compile:
	@mkdir -p bin
	@echo "Building macOS ARM64 (Apple Silicon)..."
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o bin/taskengine_darwin_arm64 ./cmd/taskengine
	@echo "Building macOS AMD64 (Intel)..."
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -o bin/taskengine_darwin_amd64 ./cmd/taskengine
	@echo "Building Linux AMD64 (x86_64)..."
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/taskengine_linux_amd64 ./cmd/taskengine
	@echo "Building Linux ARM64 (aarch64)..."
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bin/taskengine_linux_arm64 ./cmd/taskengine
	@echo "All cross-platform binaries successfully built in bin/:"
	@ls -lh bin/
