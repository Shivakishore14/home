# ==============================================================================
# Home Server Makefile
# ==============================================================================
# Orchestrates docker-compose operations, environment initialization, and
# backup/restore workflows.
# ==============================================================================

# Default target
.DEFAULT_GOAL := help

# Ensure .env is present before docker actions, or run init
.PHONY: check-env
check-env:
	@if [ ! -f .env ]; then \
		echo " [!] .env not found. Running initialization first..."; \
		$(MAKE) init; \
	fi

.PHONY: init
init:
	@echo "==> Initializing environment directories..."
	@bash scripts/init.sh

.PHONY: up
up: check-env
	@echo "==> Starting home server services..."
	docker compose up -d
	@echo "==> Services started. Run 'make status' to check health."

.PHONY: down
down: check-env
	@echo "==> Stopping home server services..."
	docker compose down

.PHONY: restart
restart: check-env
	@echo "==> Restarting home server services..."
	docker compose restart

.PHONY: logs
logs: check-env
	@echo "==> Viewing service logs (Ctrl+C to exit)..."
	docker compose logs -f

.PHONY: status
status: check-env
	@echo "==> Home Server Status:"
	docker compose ps --all

.PHONY: update
update: check-env
	@echo "==> Pulling latest container images..."
	docker compose pull
	@echo "==> Re-deploying updated services..."
	docker compose up -d --remove-orphans
	@echo "==> Pruning dangling docker images..."
	docker image prune -f

.PHONY: backup
backup: check-env
	@echo "==> Starting data backup..."
	@bash scripts/backup.sh

.PHONY: restore
restore: check-env
	@echo "==> Starting data restore..."
	@bash scripts/restore.sh

.PHONY: config-check
config-check: check-env
	@echo "==> Validating Docker Compose configuration..."
	docker compose config

.PHONY: clean
clean: check-env
	@echo "==> Cleaning up stopped containers, unused networks, and dangling volumes..."
	docker system prune -f
	@echo " [✓] System cleanup finished."

.PHONY: help
help:
	@echo "======================================================================"
	@echo " Home Server Infrastructure CLI"
	@echo "======================================================================"
	@echo " Available commands:"
	@echo "   make init          - Initialize host directories & .env file"
	@echo "   make up            - Build and start services in the background"
	@echo "   make down          - Stop and remove running services"
	@echo "   make restart       - Restart running services"
	@echo "   make status        - View container health and status"
	@echo "   make logs          - Tail container logs"
	@echo "   make update        - Pull latest images and recreate containers"
	@echo "   make backup        - Safely archive application configuration data"
	@echo "   make restore       - Restore application configuration data from backup"
	@echo "   make config-check  - Validate docker-compose syntax"
	@echo "   make clean         - Prune unused docker resources"
	@echo "   make taskengine-build   - Build the TaskEngine distributed binary"
	@echo "   make taskengine-test    - Run all TaskEngine unit & integration tests"
	@echo "   make taskengine-server  - Run TaskEngine in server mode (PORT=8080)"
	@echo "   make taskengine-worker  - Run TaskEngine in worker mode (SERVER_URL=... WORKER_ID=...)"
	@echo "   make taskengine-reload  - Trigger hot configuration reload from tasks/"
	@echo "   make taskengine-status  - View TaskEngine stats and active workers"
	@echo "   make taskengine-e2e     - Run automated end-to-end integration test"
	@echo "   make taskengine-release - Cross-compile binaries for Linux & macOS (ARM64/AMD64)"
	@echo "======================================================================"

SERVER_URL ?= http://localhost:8080
WORKER_ID ?=
CONCURRENCY ?= 0
PORT ?= 8080

.PHONY: taskengine-ensure-bin
taskengine-ensure-bin:
	@$(MAKE) -C src/taskengine ensure-bin SERVER_URL=$(SERVER_URL)

.PHONY: taskengine-build
taskengine-build:
	@echo "==> Building TaskEngine..."
	@$(MAKE) -C src/taskengine build

.PHONY: taskengine-test
taskengine-test:
	@echo "==> Running TaskEngine test suite..."
	@$(MAKE) -C src/taskengine test

.PHONY: taskengine-server
taskengine-server: taskengine-ensure-bin
	@./src/taskengine/bin/taskengine server --port $(PORT) --tasks-dir tasks

.PHONY: taskengine-worker
taskengine-worker: taskengine-ensure-bin
	@ARGS="--server-url $(SERVER_URL)"; \
	if [ -n "$(WORKER_ID)" ]; then ARGS="$$ARGS --worker-id $(WORKER_ID)"; fi; \
	if [ "$(CONCURRENCY)" -gt 0 ]; then ARGS="$$ARGS --concurrency $(CONCURRENCY)"; fi; \
	./src/taskengine/bin/taskengine worker $$ARGS

.PHONY: taskengine-reload
taskengine-reload: taskengine-ensure-bin
	@./src/taskengine/bin/taskengine reload --server-url $(SERVER_URL)

.PHONY: taskengine-status
taskengine-status: taskengine-ensure-bin
	@./src/taskengine/bin/taskengine status --server-url $(SERVER_URL)

.PHONY: taskengine-e2e
taskengine-e2e: taskengine-build
	@$(MAKE) -C src/taskengine e2e

.PHONY: taskengine-release
taskengine-release:
	@$(MAKE) -C src/taskengine cross-compile

