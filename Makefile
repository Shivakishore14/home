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
	@echo "======================================================================"
