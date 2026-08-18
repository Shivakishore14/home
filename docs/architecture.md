# System Architecture & Service Extension Guide

This document describes the design principles of the home server infrastructure, file organization, and instructions on how to scale the repository by adding new services over the next several years.

---

## 1. Directory Structure

```text
home/
│
├── README.md                # Main documentation & operator guide
├── Makefile                 # Automation interface
├── .env.example             # Environment configuration template
├── docker-compose.yml       # Base Compose configuration & network definitions
│
├── compose/                 # Service-specific compose files
│   ├── jellyfin.yml         # Jellyfin service container settings
│   └── homeassistant.yml    # Home Assistant service container settings
│
├── configs/                 # Service-specific config files (tracked in git)
│   └── README.md
│
├── scripts/                 # Utility scripts (init, backup, restore, etc.)
│   ├── init.sh
│   ├── backup.sh
│   └── restore.sh
│
├── backups/                 # Local directory for tar backups
│
├── docs/                    # Technical deep dives (macOS setup, restore guides)
│   ├── architecture.md
│   ├── macos.md
│   └── restore.md
│
├── assets/                  # Diagrams or documentation graphics
│
└── .github/
    └── workflows/
        └── docker-compose-validate.yml # Automated CI config check
```

---

## 2. Infrastructure Design Decisions

1. **Declarative Composition**: Rather than writing one giant, unmaintainable `docker-compose.yml` file, each logical service has its own YAML definition under the `compose/` directory.
2. **Environment Merging**: Services are combined using Docker Compose's native environment merging capabilities (`COMPOSE_FILE` list in `.env`). This matches the installed Docker Compose v2.15 client and is backward-compatible while preserving modularity.
3. **Shared Bridge Networking**: A custom network named `home-network` is declared as the common bus. Containers can reach each other internally by their `container_name` hostname (e.g., `http://homeassistant:8123`) without having to traverse host IP paths or expose security vectors to the LAN.
4. **Strict Bind Mounts**: We avoid anonymous Docker volumes. All application data is mounted as explicit folders under the external drive root (`DOCKER_DIR`), ensuring that the system is fully auditable, backup-friendly, and portable.

---

## 3. Adding New Services

To extend the home server with a new service (e.g., Traefik, Mosquitto, Prometheus):

### Step 1: Create the Compose File
Create a new file `compose/<service-name>.yml`. Keep the configuration clean, leverage variables, specify a healthcheck, and connect the service to the `home-network`.

**Example: Adding Mosquitto (MQTT Broker)**
`compose/mosquitto.yml`:
```yaml
services:
  mosquitto:
    image: eclipse-mosquitto:latest
    container_name: mosquitto
    restart: unless-stopped
    ports:
      - "1883:1883"
      - "9001:9001"
    volumes:
      - "${DOCKER_DIR}/mosquitto/config:/mosquitto/config"
      - "${DOCKER_DIR}/mosquitto/data:/mosquitto/data"
      - "${DOCKER_DIR}/mosquitto/log:/mosquitto/log"
    networks:
      - home-network
    healthcheck:
      test: ["CMD-SHELL", "mosquitto_sub -h localhost -t '$SYS/#' -C 1 || exit 1"]
      interval: 30s
      timeout: 10s
      retries: 3
```

### Step 2: Register Config/Data Folders
Add any directory initialization commands (if the service requires specific folders to be pre-created, like `mosquitto/config`) to `scripts/init.sh`:
```bash
# Add this inside scripts/init.sh under Section 4:
mkdir -p "${DOCKER_DIR}/mosquitto/config"
mkdir -p "${DOCKER_DIR}/mosquitto/data"
```

### Step 3: Register Service in .env & .env.example
Append your compose file to the `COMPOSE_FILE` variable in `.env` and `.env.example`:
```ini
COMPOSE_FILE=docker-compose.yml:compose/jellyfin.yml:compose/homeassistant.yml:compose/mosquitto.yml
```

### Step 4: Validate Configuration
Verify the merged configurations parse correctly:
```bash
make config-check
```

### Step 5: Start Service
Redeploy the stack to start the new service:
```bash
make up
```

---

## 4. Design Guidelines for Future Services

- **Traefik**: Will act as the reverse proxy. Place it in `compose/traefik.yml`. Expose ports `80` and `443` there. Connect it to `home-network` so it can proxy traffic to Jellyfin/Home Assistant using their internal container names.
- **Prometheus & Grafana**: Add `compose/prometheus.yml` and `compose/grafana.yml`. Map Prometheus configs from `configs/prometheus/prometheus.yml`.
- **Mosquitto & Zigbee2MQTT**: Add `compose/mosquitto.yml` and `compose/zigbee2mqtt.yml`. Zigbee2MQTT will communicate with Mosquitto internally via `mqtt://mosquitto:1883` on `home-network`.
- **Node-RED / ESPHome**: Can easily join `home-network` and talk directly to Home Assistant.
- **Ollama / Open WebUI**: Ollama can run with CPU or utilize macOS GPU natively if run outside Docker. If run inside Docker, use CPU-based container and point Open WebUI to `http://host.docker.internal:11434`.

---

## 5. VPS Stack (Separate from the Mac mini)

The public VPS is a different machine with different constraints (no external APFS drive, no macOS PUID/GID mapping, publicly reachable ports), so it is deliberately **not** merged into the Mac mini's `docker-compose.yml` + `compose/` + `.env` system. It lives entirely in [docker-compose.vps.yml](file:///Users/server/github/home/docker-compose.vps.yml) at the repo root, with its own `vps-network` bridge network and, if needed, its own `.env` on that machine (never shared with the Mac mini's `.env`).

It currently defines two services: `taskengine` (server mode, its own DB/tasks bind mounts) and `ntfy` (push notifications, with `deny-all` default auth since the box is public).

This single-file approach is intentional while the stack is small. Once more services are added to the VPS, apply the same modularization used for the Mac mini:
1. Move each service out of `docker-compose.vps.yml` into `compose/<service>.yml`.
2. Keep a minimal `docker-compose.vps.yml` (or reuse the root `docker-compose.yml`) as the base network declaration.
3. Merge files via `COMPOSE_FILE` in the VPS's `.env`, exactly as described in Section 3 above.
