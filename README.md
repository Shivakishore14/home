# Infrastructure as Code (IaC) Home Server Setup

This repository is the single source of truth for managing a production-grade, reproducible home server on a Mac mini (Apple Silicon) running macOS. 

All infrastructure settings, service relationships, and network environments are managed entirely through code using Docker Compose, a [Makefile](file:///Users/server/github/home/Makefile), and shell automation scripts.

---

## 📖 Table of Contents
1. [System Architecture](#system-architecture)
2. [Directory Structure](#directory-structure)
3. [Prerequisites & External Drive](#prerequisites--external-drive)
4. [Quick Start (Installation)](#quick-start-installation)
5. [Operational Commands Reference](#operational-commands-reference)
6. [Service Documentation](#service-documentation)
7. [VPS Stack](#-vps-stack)
8. [Backup and Restoration](#backup-and-restoration)
9. [Adding New Services](#adding-new-services)
10. [Troubleshooting & macOS Notes](#troubleshooting--macos-notes)

---

## 🏗️ System Architecture

Services are split into isolated, service-specific files located in the [compose/](file:///Users/server/github/home/compose/) folder. The base configurations (such as global networks) are declared in the root [docker-compose.yml](file:///Users/server/github/home/docker-compose.yml). 

Environment variables are loaded from a local `.env` file created from [.env.example](file:///Users/server/github/home/.env.example) during initialization. A custom internal bridge network named `home-network` links all services.

A separate public VPS node is defined in [docker-compose.vps.yml](file:///Users/server/github/home/docker-compose.vps.yml) — it runs TaskEngine (server mode) and ntfy on its own `vps-network`, independent of the Mac mini stack. It's kept as a single file for now; once more VPS services are added, it will be split into `compose/<service>.yml` files following the same pattern as the Mac mini stack.

> [!NOTE]
> For a technical deep-dive into design decisions, network topologies, and system scaling plans, see [docs/architecture.md](file:///Users/server/github/home/docs/architecture.md).

---

## 📁 Directory Structure

The repository is structured to prioritize isolation, modularity, and future expansion:

```text
home/
│
├── README.md                # This document (Operators Manual)
├── Makefile                 # Task automation interface
├── .env.example             # Environment configuration template
├── docker-compose.yml       # Base docker-compose configuration
├── docker-compose.vps.yml   # Standalone VPS stack (TaskEngine + ntfy)
│
├── compose/                 # Modular service compose files
│   ├── jellyfin.yml         # Jellyfin media player definition
│   └── homeassistant.yml    # Home Assistant smart-home definition
│
├── tasks/                   # TaskEngine git-tracked configs & schedules
│   ├── config.yaml          # Server runtime settings & defaults
│   ├── workers/             # Per-worker override definitions
│   ├── definitions/         # Periodic task schedules & definitions
│   └── assets/              # Task scripts synced dynamically to workers
│
├── src/
│   └── taskengine/          # TaskEngine distributed Go orchestrator
│
├── configs/                 # Config template/schema tracking
│   └── README.md
│
├── scripts/                 # Automation and utility scripts
│   ├── init.sh              # Directory & permission setup script
│   ├── backup.sh            # Safe database backup script
│   └── restore.sh           # Interactive configuration restore script
│
├── backups/                 # Local directory for system backups
│
├── docs/                    # Deep-dive documentation
│   ├── architecture.md      # Scaling and design docs
│   ├── taskengine.md        # TaskEngine operator and developer manual
│   ├── macos.md             # macOS/Apple Silicon specific settings
│   └── restore.md           # Backup & Recovery documentation
│
└── .github/                 # GitHub workflows folder
    └── workflows/
        └── docker-compose-validate.yml # CI Config check
```

---

## 💾 Prerequisites & External Drive

To ensure the internal SSD is protected from read/write wear and has sufficient storage capacity, **all persistent application configs and media data must reside on the external drive**.

### Core Requirements
1. **Docker Desktop**: Docker Desktop must be installed and running on macOS.
2. **External Drive Mount**: An external drive formatted as **APFS** (preferred) or exFAT must be mounted at `/Volumes/drive001`.
3. **Docker Drive Permissions**: Docker Desktop must be granted permissions to share `/Volumes` and `/Volumes/drive001` via **Settings > Resources > File sharing**.

> [!IMPORTANT]
> Detailed instructions on Docker installation, drive formatting, drive permissions, and macOS sandboxing settings are documented in [docs/macos.md](file:///Users/server/github/home/docs/macos.md).

---

## 🚀 Quick Start (Installation)

To get the infrastructure running on each machine:

### 1. Mac mini (Home Server & Orchestrator Node)
```bash
# Clone the repository
git clone <your-repository-url>
cd home

# Start the full Mac mini stack (initializes .env/storage, starts Docker Compose, and launches TaskEngine server)
make macmini
```

### 2. MacBook Air (Distributed Worker Node)
```bash
# Start the TaskEngine worker on MacBook Air (connecting to Mac mini server URL)
make macbookair SERVER_URL=http://<macmini-ip>:8080
```

### 3. VPS (Public Node — TaskEngine + ntfy)
```bash
# On the VPS, clone the repo, then bring up its own standalone stack
docker compose -f docker-compose.vps.yml up -d --build

# Create the first ntfy admin user (required once — default access is deny-all)
docker compose -f docker-compose.vps.yml exec ntfy ntfy user add --role=admin <username>
```

---

## 🛠️ Operational Commands Reference

The [Makefile](file:///Users/server/github/home/Makefile) acts as the unified CLI for the infrastructure.

### Machine Entrypoints
| Command | Action |
|:---|:---|
| `make macmini` | **Mac mini Entrypoint**: Runs Docker Compose services in the background and starts TaskEngine server. |
| `make macbookair` | **MacBook Air Entrypoint**: Runs TaskEngine worker with `WORKER_ID=macbook-air` (accepts `SERVER_URL=...`). |

### Docker & Infrastructure Commands
| Command | Action |
|:---|:---|
| `make init` | Copies `.env.example` to `.env` and initializes storage directories on `/Volumes/drive001`. |
| `make up` | Automatically runs `init` (if needed) and brings up the Docker services. |
| `make down` | Stops the containers and removes temporary resources. |
| `make restart` | Restarts the running services. |
| `make logs` | Tails console logs for all containers (`Ctrl+C` to quit). |
| `make status` | Displays detailed status, health check states, and port mapping info. |
| `make update` | Pulls the latest image versions and runs containers seamlessly. |
| `make backup` | Gracefully stops databases, tars the config directory, and restarts the containers. |
| `make restore` | Launches the recovery process to restore configurations from a backup file. |
| `make config-check` | Validates YAML syntax and verifies environment file merges. |
| `make clean` | Prunes stopped containers, unused networks, and dangling docker objects. |

### TaskEngine Orchestrator Commands
| Command | Action |
|:---|:---|
| `make taskengine-build` | Compiles the distributed TaskEngine binary (`bin/taskengine`). |
| `make taskengine-server` | Starts TaskEngine in server mode on `PORT=8080`. |
| `make taskengine-worker` | Starts TaskEngine in worker mode (`SERVER_URL=... WORKER_ID=...`). |
| `make taskengine-reload` | Triggers hot configuration reload from the `tasks/` directory. |
| `make taskengine-status` | Displays server status, task counts, and registered workers. |
| `make taskengine-test` | Runs the full Go unit and integration test suite. |
| `make taskengine-e2e` | Executes end-to-end integration test with real task execution. |
| `make taskengine-release` | Cross-compiles binaries for macOS & Linux (ARM64/AMD64). |


---

## 🐋 Service Documentation

### TaskEngine Distributed Orchestrator
- **Source Path**: [src/taskengine/](file:///Users/server/github/home/src/taskengine/)
- **Configuration Directory**: [tasks/](file:///Users/server/github/home/tasks/)
- **Documentation**: [docs/taskengine.md](file:///Users/server/github/home/docs/taskengine.md)
- **Host Port**: `8080` (Web UI & REST API)
- **Features**: Dual-mode binary (`server`/`worker`), generic multi-language `command-runner`, 1080p Jellyfin Direct Play video transcoder, task-level prerequisites & git-synced assets, per-worker path translation, reactive HTMX + SSE dashboard.

### Jellyfin Media Server
- **Compose Path**: [compose/jellyfin.yml](file:///Users/server/github/home/compose/jellyfin.yml)
- **Host Port**: `8096`
- **Config Storage Path**: `/Volumes/drive001/docker/jellyfin/config` & `/cache`
- **Media Libraries Path**: `/Volumes/drive001/media`
- **Media Directory Structure**:
  ```text
  media/
  ├── Movies/
  ├── TV Shows/
  ├── Anime/
  ├── Music/
  └── Photos/
  ```
- **Pre-Transcoding**: Use TaskEngine's `video-transcoder` to pre-transcode media to 1080p H.264 + AAC Stereo with `+faststart` MP4 containers. This guarantees **100% Direct Play** on all clients with 0% CPU transcoding on the server.

### Home Assistant Core
- **Compose Path**: [compose/homeassistant.yml](file:///Users/server/github/home/compose/homeassistant.yml)
- **Host Port**: `8123`
- **Config Storage Path**: `/Volumes/drive001/docker/homeassistant`
- **USB Passthrough (Zigbee/Z-Wave)**: macOS Docker does not support native mapping of USB serial dongles. We recommend using a Network-Attached Coordinator (e.g. SLZB-06 over Ethernet) or implementing the USB/IP protocol inside Docker Desktop. See [docs/macos.md](file:///Users/server/github/home/docs/macos.md) for options.

---

## 🌐 VPS Stack

A separate, standalone stack for a public VPS, defined entirely in [docker-compose.vps.yml](file:///Users/server/github/home/docker-compose.vps.yml). It does not share the `home-network` or `.env` with the Mac mini stack — the VPS keeps its own `.env` (if you need to override defaults) and is managed with plain `docker compose -f docker-compose.vps.yml <cmd>` rather than the Makefile targets above.

### TaskEngine (VPS, server mode)
- **Compose Path**: [docker-compose.vps.yml](file:///Users/server/github/home/docker-compose.vps.yml)
- **Host Port**: `${TASKENGINE_PORT:-8080}`
- **Data**: `./data` and `./tasks` (bind-mounted from the repo checkout on the VPS), independent of the Mac mini's TaskEngine server/database.

### ntfy (Push Notifications)
- **Compose Path**: [docker-compose.vps.yml](file:///Users/server/github/home/docker-compose.vps.yml)
- **Host Port**: `${NTFY_PORT:-8090}`
- **Data**: `./data/ntfy` — holds the message cache, attachment cache, and the auth database.
- **Auth**: `NTFY_AUTH_DEFAULT_ACCESS=deny-all` — the topic space is closed by default since this is exposed on the public internet. Create users with `docker compose -f docker-compose.vps.yml exec ntfy ntfy user add --role=admin <username>`.
- **Base URL**: set `NTFY_BASE_URL` in the VPS's `.env` to your real domain once one is pointed at the box (defaults to `http://localhost:8090`).

> [!NOTE]
> Once more services are added to the VPS, split `docker-compose.vps.yml` into `compose/<service>.yml` files and merge them via `COMPOSE_FILE`, matching the Mac mini pattern described in [docs/architecture.md](file:///Users/server/github/home/docs/architecture.md).

---

## 🔒 Backup and Restoration

### To Back Up:
Run:
```bash
make backup
```
Backups are compressed as `.tar.gz` and saved in the `backups/` directory (or as defined by `BACKUP_DIR` in `.env`).

### To Restore:
Run:
```bash
make restore
```
Follow the interactive prompt to select the backup file. The script will move the active directory to a `.old` path before extracting the backup.

> [!TIP]
> A step-by-step restoration walkthrough is located in [docs/restore.md](file:///Users/server/github/home/docs/restore.md).

---

## ➕ Adding New Services

The infrastructure is designed to scale with services like Traefik, Mosquitto, Prometheus, ESPHome, and Uptime Kuma without needing to rewrite any code.

Simply:
1. Create `compose/<service-name>.yml`.
2. Map data folders to `${DOCKER_DIR}/<service-name>`.
3. Pre-create directories in [scripts/init.sh](file:///Users/server/github/home/scripts/init.sh).
4. Append your compose file to the `COMPOSE_FILE` variable in `.env`.

> [!NOTE]
> View detailed step-by-step examples for adding services in [docs/architecture.md](file:///Users/server/github/home/docs/architecture.md).

---

## ⚠️ Troubleshooting & macOS Notes

### SQLite Database Locks & Failures
If Jellyfin or Home Assistant crashes with file lock errors, check the filesystem of your external drive. It must be formatted as **APFS**. macOS exFAT volumes do not support Unix file locks, which are required by SQLite.

### Service is Unhealthy
Ensure the service has finished starting up. Home Assistant and Jellyfin may take up to 60 seconds to pass health checks during their initial initialization. Use `make logs` to inspect startup progress.

---

## 🛡️ License
Managed as code. Licensed under the MIT License.
