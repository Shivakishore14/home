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
	@echo " Machine Entrypoints:"
	@echo "   make macmini       - Start Mac mini services (Docker Compose + TaskEngine server)"
	@echo "   make macbookair    - Start MacBook Air TaskEngine worker"
	@echo ""
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

PLUGINS ?=

# ==============================================================================
# Machine Entrypoints
# ==============================================================================

.PHONY: macmini
macmini: check-env taskengine-build
	@echo "==> [Mac mini] Starting Docker Compose infrastructure..."
	docker compose up -d
	@echo "==> [Mac mini] Starting TaskEngine server on port $(PORT)..."
	@./src/taskengine/bin/taskengine server --port $(PORT) --tasks-dir tasks

.PHONY: macbookair macbook-air
macbookair macbook-air: taskengine-build
	@WORKER_ID_VAL="$(WORKER_ID)"; \
	if [ -z "$$WORKER_ID_VAL" ]; then WORKER_ID_VAL="macbook-air"; fi; \
	echo "==> [MacBook Air] Starting TaskEngine worker ($$WORKER_ID_VAL) connecting to $(SERVER_URL)..."; \
	ARGS="--server-url $(SERVER_URL) --worker-id $$WORKER_ID_VAL"; \
	if [ "$(CONCURRENCY)" -gt 0 ]; then ARGS="$$ARGS --concurrency $(CONCURRENCY)"; fi; \
	if [ -n "$(PLUGINS)" ]; then ARGS="$$ARGS --plugins $(PLUGINS)"; fi; \
	./src/taskengine/bin/taskengine worker $$ARGS

# ==============================================================================
# TaskEngine Targets
# ==============================================================================

.PHONY: taskengine-ensure-bin
taskengine-ensure-bin:
	@$(MAKE) -C src/taskengine ensure-bin SERVER_URL=$(SERVER_URL)

.PHONY: taskengine-build
taskengine-build:
	@echo "==> Building TaskEngine..."
	@$(MAKE) -C src/taskengine build SERVER_URL=$(SERVER_URL)

.PHONY: taskengine-test
taskengine-test:
	@echo "==> Running TaskEngine test suite..."
	@$(MAKE) -C src/taskengine test

.PHONY: taskengine-server
taskengine-server: taskengine-build
	@./src/taskengine/bin/taskengine server --port $(PORT) --tasks-dir tasks

.PHONY: taskengine-worker
taskengine-worker: taskengine-build
	@ARGS="--server-url $(SERVER_URL)"; \
	if [ -n "$(WORKER_ID)" ]; then ARGS="$$ARGS --worker-id $(WORKER_ID)"; fi; \
	if [ "$(CONCURRENCY)" -gt 0 ]; then ARGS="$$ARGS --concurrency $(CONCURRENCY)"; fi; \
	if [ -n "$(PLUGINS)" ]; then ARGS="$$ARGS --plugins $(PLUGINS)"; fi; \
	./src/taskengine/bin/taskengine worker $$ARGS

.PHONY: taskengine-producer
taskengine-producer: taskengine-build
	@ARGS="--server-url $(SERVER_URL) --tasks-dir tasks"; \
	if [ -n "$(INTERVAL)" ]; then ARGS="$$ARGS --interval $(INTERVAL)"; fi; \
	if [ "$(ONCE)" = "true" ]; then ARGS="$$ARGS --once"; fi; \
	./src/taskengine/bin/taskengine producer $$ARGS

.PHONY: taskengine-reload
taskengine-reload: taskengine-build
	@./src/taskengine/bin/taskengine reload --server-url $(SERVER_URL)

.PHONY: taskengine-status
taskengine-status: taskengine-build
	@./src/taskengine/bin/taskengine status --server-url $(SERVER_URL)

.PHONY: taskengine-retry-failed
taskengine-retry-failed: taskengine-build
	@./src/taskengine/bin/taskengine retry-failed --server-url $(SERVER_URL)

.PHONY: taskengine-e2e
taskengine-e2e: taskengine-build
	@$(MAKE) -C src/taskengine e2e

.PHONY: taskengine-release
taskengine-release:
	@$(MAKE) -C src/taskengine cross-compile

.PHONY: taskengine-export
taskengine-export:
	@mkdir -p backups data
	@TS=$$(date +"%Y%m%d_%H%M%S"); \
	OUT="backups/taskengine_export_$${TS}.tar.gz"; \
	echo "==> Exporting TaskEngine state (database & config)..."; \
	if [ -f "data/taskengine.db" ]; then \
		if command -v sqlite3 >/dev/null 2>&1; then \
			sqlite3 data/taskengine.db "VACUUM INTO 'data/taskengine_backup.db';"; \
		else \
			cp data/taskengine.db data/taskengine_backup.db; \
		fi; \
		tar -czf "$$OUT" tasks/ -C data taskengine_backup.db; \
		rm -f data/taskengine_backup.db; \
		echo " [✓] Export saved to: $$OUT"; \
	else \
		tar -czf "$$OUT" tasks/; \
		echo " [✓] Export saved to: $$OUT (configs only, no db found)"; \
	fi

.PHONY: taskengine-import
taskengine-import:
	@if [ -z "$(FILE)" ]; then \
		echo "ERROR: Please specify FILE=backups/taskengine_export_YYYYMMDD_HHMMSS.tar.gz"; \
		exit 1; \
	fi
	@if [ ! -f "$(FILE)" ]; then \
		echo "ERROR: Backup file '$(FILE)' not found."; \
		exit 1; \
	fi
	@echo "==> Restoring TaskEngine state from $(FILE)..."
	@mkdir -p data tasks
	@TMPDIR=$$(mktemp -d); \
	tar -xzf "$(FILE)" -C "$$TMPDIR"; \
	if [ -f "$$TMPDIR/taskengine_backup.db" ]; then \
		cp "$$TMPDIR/taskengine_backup.db" data/taskengine.db; \
		echo " [✓] Restored database to data/taskengine.db"; \
	fi; \
	if [ -d "$$TMPDIR/tasks" ]; then \
		cp -r "$$TMPDIR/tasks/"* tasks/; \
		echo " [✓] Restored configuration to tasks/"; \
	fi; \
	rm -rf "$$TMPDIR"; \
	echo " [✓] Restore completed successfully."


