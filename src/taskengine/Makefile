.PHONY: build test clean run-server run-worker reload status e2e cross-compile

BINARY_NAME=bin/taskengine
SERVER_URL ?= http://localhost:8080
WORKER_ID ?=
CONCURRENCY ?= 0
PORT ?= 8080

# Use external drive for Go build temp directory if available
ifneq ($(wildcard /Volumes/drive001/tmp),)
export GOTMPDIR := /Volumes/drive001/tmp
endif

build:
	@mkdir -p bin
	go build -o $(BINARY_NAME) ./cmd/taskengine

test:
	go test -v ./...

clean:
	rm -rf bin/ data/ *.db *.db-wal *.db-shm /tmp/taskengine_*

run-server: build
	./$(BINARY_NAME) server --port $(PORT) --tasks-dir ../../tasks

run-worker: build
	@ARGS="--server-url $(SERVER_URL)"; \
	if [ -n "$(WORKER_ID)" ]; then ARGS="$$ARGS --worker-id $(WORKER_ID)"; fi; \
	if [ "$(CONCURRENCY)" -gt 0 ]; then ARGS="$$ARGS --concurrency $(CONCURRENCY)"; fi; \
	./$(BINARY_NAME) worker $$ARGS

reload: build
	./$(BINARY_NAME) reload --server-url $(SERVER_URL)

status: build
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
