# ⚡ TaskEngine: Distributed Task Orchestration System

`TaskEngine` is a lightweight, distributed task execution system written in Go. It operates as a single binary with dual roles (`server` or `worker`), featuring an open plugin architecture, atomic task claiming via pure-Go SQLite, file-based configuration tracked in git (`tasks/`), task-level prerequisites & asset synchronization, hierarchical configuration inheritance with per-worker path translation, and an embedded reactive Web UI (HTMX + SSE).

---

## 📖 Table of Contents
1. [Architecture & Topology](#-architecture--topology)
2. [Configuration Structure (`tasks/`)](#-configuration-structure-tasks)
3. [Universal Plugin System](#-universal-plugin-system)
   - [Generic Command Runner (`command-runner`)](#1-generic-command-runner-command-runner)
   - [Video Transcoder (`video-transcoder`) for Jellyfin](#2-video-transcoder-video-transcoder)
4. [Task Prerequisites & Git-Synced Assets](#-task-prerequisites--git-synced-assets)
5. [Worker Path Translation](#-worker-path-translation)
6. [Operational Commands Reference](#-operational-commands-reference)
7. [Embedded Web Dashboard](#-embedded-web-dashboard)
8. [Cross-Platform Compilation & Releases](#-cross-platform-compilation--releases)
9. [Authentication](#-authentication)
10. [Object Storage (Cloudflare R2)](#-object-storage-cloudflare-r2)
11. [ComfyUI Plugin (`comfyui`)](#-comfyui-plugin-comfyui)

---

## 📐 Architecture & Topology

```mermaid
flowchart TD
    subgraph ServerNode ["Server Node (Mac mini / Home Server)"]
        CLI_S[taskengine server]
        CFG["Config Manager (tasks/)"]
        DB[(SQLite DB: WAL Mode)]
        API["HTTP REST & SSE API (:8080)"]
        UI["Embedded Web UI Dashboard"]
        SCHED["Task Generator & Scheduler"]

        CLI_S --> CFG
        CLI_S --> API
        API <--> DB
        UI <--> API
        SCHED -->|Enqueue TaskPayload| DB
    end

    subgraph Worker1 ["Worker Node 1 (MacBook Air)"]
        CLI_W1["taskengine worker --server-url http://..."]
        CACHE1["~/.taskengine/ (Assets & Cache)"]
        PT1["Path Translator (/Volumes -> ~/Desktop)"]
        EXEC1["Task Runner (Transcoder / Command)"]

        CLI_W1 -->|1. Claim Task via HTTP| API
        CLI_W1 -->|2. Sync Assets & Check Prereqs| CACHE1
        CLI_W1 -->|3. Translate Paths| PT1
        PT1 -->|4. Execute Job| EXEC1
        EXEC1 -->|5. Stream Progress, Logs, Heartbeat| API
    end

    subgraph Worker2 ["Worker Node 2 (Linux / GPU Node)"]
        CLI_W2["taskengine worker --server-url http://..."]
        CACHE2["~/.taskengine/"]
        EXEC2["Task Runner (NVENC / Python / Bash)"]

        CLI_W2 -->|HTTP Claim / Run / Complete| API
    end
```

### Core Components
* **Dual-Mode Binary**: Runs as `taskengine server` or `taskengine worker`.
* **Database**: Pure-Go SQLite (`modernc.org/sqlite`) running in WAL mode with immediate transactions for atomic task claiming. Zero CGO dependencies.
* **Network & Security Scope**: Lightweight HTTP/SSE communication designed for local LAN, Tailscale, or WireGuard mesh.
* **Dynamic Configuration**: All settings reside in the `tasks/` directory on the server host and can be hot-reloaded without restarting the process.

---

## 📁 Configuration Structure (`tasks/`)

The repository root contains the `tasks/` directory, acting as the single source of truth:

```text
home/
└── tasks/
    ├── config.yaml            # Server runtime settings and global defaults
    ├── workers/               # Per-worker override specifications
    │   ├── laptop1.yaml
    │   └── desktop_gpu.yaml
    ├── definitions/           # Task schedules & definitions
    │   ├── ping.yaml
    │   ├── system_metrics.yaml
    │   └── video_transcode.yaml
    └── assets/                # Task scripts and helper files synced to workers
        └── system-metrics/
            └── gather_metrics.py
```

### 1. Global Settings: `tasks/config.yaml`
```yaml
server:
  port: 8080
  db_path: "data/taskengine.db"
  heartbeat_timeout_seconds: 30
  stale_task_check_interval_seconds: 10

defaults:
  max_concurrent_tasks: 2
  path_mappings:
    "/Volumes/drive001/media": "/Volumes/drive001/media"
  plugin_configs:
    command-runner:
      shell: "/bin/zsh"
    video-transcoder:
      target_codec: "libx264"
      target_height: 1080
      crf: 21
      preset: "medium"
      audio_bitrate: "192k"
```

### 2. Worker Overrides: `tasks/workers/<worker-id>.yaml`
```yaml
worker_id: "laptop1"
max_concurrent_tasks: 2
path_mappings:
  "/Volumes/drive001/media": "/Users/air/Desktop/macmini_media"
plugin_configs:
  video-transcoder:
    target_codec: "h264_videotoolbox"
    ffmpeg_binary: "/opt/homebrew/bin/ffmpeg"
  command-runner:
    shell: "/bin/zsh"
```

---

## 🔌 Universal Plugin System

### 1. Generic Command Runner (`command-runner`)
Executes arbitrary scripts or system commands in any language (Python, Bash, Node.js, Rust, Docker, or CLI binaries) without recompiling the TaskEngine binary.

* **Environment Variables Injected**:
  * `TASK_ID`, `TASK_PLUGIN`, `TASK_NAME`, `TASK_TARGET_FILE`, `TASK_PARAMS_JSON`
  * `TASK_DIR`: Dedicated working directory (`~/.taskengine/tasks/<name>/`)
  * `TASK_ASSETS_DIR`: Path to synced asset files (`~/.taskengine/tasks/<name>/assets/`)
  * `TASK_CACHE_DIR`: Root cache directory (`~/.taskengine/`)
  * `SERVER_URL`, `WORKER_ID`
* **Real-Time Progress Parsing**:
  * Text markers: `PROGRESS: 45%`
  * JSON lines: `{"progress": 45, "speed": "1.8x", "message": "processing"}`
* **Live Log Streaming**: Captures `stdout` and `stderr` line-by-line and streams them over SSE.

### 2. Video Transcoder (`video-transcoder`)
Optimized for **Jellyfin 1080p Universal Direct Play** (100% Direct Play on Web, Mobile, and TV with 0% CPU transcoding on the server):
* **Resolution**: Scaled to 1080p (`scale='min(1920,iw)':-2`).
* **Video Profile**: H.264 High Profile Level 4.1, 8-bit `yuv420p` (`-crf 21 -preset medium`).
* **Audio Profile**: AAC Stereo 2.0 @ 192 kbps (`-c:a aac -b:a 192k -ac 2`).
* **Streaming Optimization**: `-movflags +faststart` for zero-buffer immediate start.
* **Clean In-Place Replacement**: Encodes to `.temp.<base>.mp4`, deletes original, and atomically renames to clean `<base>.mp4` (no `.transcoded` suffix clutter in Jellyfin).
* **Local SSD Scratch Buffering (`scratch_dir`)**:
  * If `scratch_dir` is configured (e.g. `~/.taskengine/scratch`), the worker pulls the source file sequentially to its high-speed local NVMe SSD first.
  * FFmpeg runs with 100% local I/O (zero network latency or HDD seek thrashing).
  * Upon successful transcode, the clean `.mp4` is uploaded back to the media storage, the original file is deleted, `.transcode_cache` is updated, and the local scratch folder is cleaned up.
* **Local Folder State Tracking (`.transcode_cache`)**:
  * Each directory maintains a `.transcode_cache` file listing completed media filenames.
  * State is completely portable and lives with the files. Even if SQLite is reset or media is moved to a new drive, files are never re-encoded.
  * The scanner checks `.transcode_cache` first and skips cached files in $O(1)$ time.

---

## 📦 Task Prerequisites & Git-Synced Assets

Task definitions can declare generic dependency checks and asset files:

```yaml
name: "system-metrics-reporter"
plugin_name: "command-runner"
schedule: "*/10 * * * *"
enabled: true
priority: 5

# 1. Generic Task-Level Prerequisites
# Must exit with code 0 on the worker before the job runs
prerequisites:
  check_command: "which python3 || (echo 'python3 is required' && exit 1)"

# 2. Task-Level Assets Synced to Worker
assets:
  files:
    - "tasks/assets/system-metrics/gather_metrics.py"

params:
  command: "python3 $TASK_ASSETS_DIR/gather_metrics.py"
```

### How Asset Sync Operates:
1. **On Every Task Execution**: The worker queries `GET /api/v1/tasks/{name}/assets` for the current git commit hash and file list.
2. **Cache Verification**: Checks `~/.taskengine/tasks/<name>/.version` and verifies all files physically exist on disk.
3. **Auto-Refresh**: If git commit changed or files are missing, fresh files are downloaded automatically over HTTP from the server before executing.
4. **Prerequisites Check**: Runs `prerequisites.check_command`. If any prerequisite fails, the task fails with clear diagnostic logs.

---

## 🗺️ Worker Path Translation

When jobs operate on shared storage with different mount paths across machines:
* **Server**: `/Volumes/drive001/media/Movies/sample.mkv`
* **Worker Mapping** in `tasks/workers/laptop1.yaml`:
  ```yaml
  path_mappings:
    "/Volumes/drive001/media": "/Users/air/Desktop/macmini_media"
  ```
* **Resolved on Worker**: `/Users/air/Desktop/macmini_media/Movies/sample.mkv`

Path translation uses longest-prefix matching before invoking worker plugins.

---

## 🛠️ Operational Commands Reference

All commands are managed through the root [Makefile](file:///Users/server/github/home/Makefile):

### Primary Machine Entrypoints
| Entrypoint | Command | Description |
| :--- | :--- | :--- |
| **Mac mini (Server Node)** | `make macmini` | Starts Docker Compose stack in background + launches TaskEngine server |
| **MacBook Air (Worker Node)** | `make macbookair` | Starts TaskEngine worker (`WORKER_ID=macbook-air`) connecting to `SERVER_URL` |
| **MacBook Air (Remote Server)** | `make macbookair SERVER_URL=http://<macmini-ip>:8080` | MacBook Air worker connecting over LAN / Tailscale |

### TaskEngine CLI Commands
| Task | Command | Description |
| :--- | :--- | :--- |
| **Build Binary** | `make taskengine-build` | Compiles `src/taskengine/bin/taskengine` |
| **Start Server** | `make taskengine-server` | Starts server on `PORT=8080` (or `PORT=9000`) |
| **Start Worker** | `make taskengine-worker` | Starts worker daemon connecting to server |
| **Custom Worker** | `make taskengine-worker SERVER_URL=http://... WORKER_ID=laptop1 CONCURRENCY=2` | Parameterized worker start |
| **Reload Config** | `make taskengine-reload` | Re-parses `tasks/` YAML files without restarting |
| **Check Status** | `make taskengine-status` | Displays server status, task counts, and workers |
| **Run Unit Tests**| `make taskengine-test` | Runs full Go test suite across all packages |
| **Run E2E Tests** | `make taskengine-e2e` | Runs automated end-to-end integration test |
| **Cross-Compile** | `make taskengine-release` | Builds Linux & macOS binaries (ARM64 & AMD64) |


---

## 🖥️ Embedded Web Dashboard

Accessible by default at `http://localhost:8080`:
* **Real-time Stats**: Counters for active workers, running tasks, pending queue, completed, and failed tasks.
* **Registered Workers List**: Hostname, enabled plugins, status badge, and heartbeat.
* **Live Task Table**: Task ID, plugin, target file, assigned worker, status, and animated percentage progress bar with encoding speed.
* **Live Terminal Log Modal**: Streams subprocess `stdout`/`stderr` logs in real-time over Server-Sent Events (SSE).
* **New Task Modal**: Form to manually enqueue tasks with custom plugins and JSON parameters.
* **Reload Config Button**: Triggers `POST /api/v1/config/reload` with instant UI toast feedback.

---

## 📦 Cross-Platform Compilation & Releases

Because `TaskEngine` uses pure-Go SQLite (`modernc.org/sqlite`) with zero CGO dependencies (`CGO_ENABLED=0`), cross-compilation is instantaneous:

```bash
make taskengine-release
```

Outputs in `src/taskengine/bin/`:
* `taskengine_darwin_arm64` — macOS Apple Silicon (M1/M2/M3/M4)
* `taskengine_darwin_amd64` — macOS Intel
* `taskengine_linux_arm64` — Linux ARM64 (Raspberry Pi 4/5, AWS Graviton)
* `taskengine_linux_amd64` — Linux x86_64 (Servers, VMs, Synology/TrueNAS)

---

## 🔐 Authentication

Auth is enabled as soon as any token is set on the server; with no tokens the API is open (LAN-only Mac mini setup).

| Env var (server) | Access |
|---|---|
| `TASKENGINE_API_KEY`, `TASKENGINE_CLIENT_TOKENS` (comma-separated) | Everything (dashboard, create/cancel tasks, uploads, downloads) |
| `TASKENGINE_WORKER_TOKENS` (comma-separated) | Worker endpoints only (register, heartbeat, claim, progress, logs, complete, fail, outputs, assets) |
| `TASKENGINE_REQUIRE_AUTH=true` / `--require-auth` | Refuse to start without tokens (set in `docker-compose.vps.yml`) |

* API clients send `Authorization: Bearer <token>`.
* Workers, the producer and CLI commands read `TASKENGINE_TOKEN` (workers also accept `--token`). The token is only attached to requests for the server's own host, never to presigned storage URLs or ComfyUI.
* The dashboard redirects to `/login`, which sets an HttpOnly cookie (90 days). `GET /healthz` is public.
* `/api/v1/files/...` only serves files inside `tasks/`.

---

## 🪣 Object Storage (Cloudflare R2)

Task inputs and outputs live in the R2 bucket `taskengine`; **file bytes never pass through the server**, which only signs short-lived URLs.

```
taskengine/
  <plugin>/in/<date>/<file id>/<name>          client uploads
  <plugin>/out/<task id>/<id prefix>-<name>    task outputs
  tmp/                                          scratch (1-day lifecycle)
```

* **Credentials** (server env only): `R2_ACCOUNT_ID`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_BUCKET` (default `taskengine`). Without them storage endpoints return 503.
* **Policy** per plugin in `tasks/config.yaml` → `storage.namespaces.<plugin>` (`retention_days`, `max_upload_mb`, `max_output_mb`, `allowed_mime`). A plugin without an entry cannot use storage. `retention_days` must match the bucket lifecycle rule for `<plugin>/` (`wrangler r2 bucket lifecycle add taskengine comfyui-expire comfyui/ --expire-days 10`).
* Presigned PUTs sign `Content-Type` and `Content-Length`, so R2 rejects anything other than the declared file.

### Flow

```bash
TOKEN=...; B=$TASKENGINE_URL   # your public TaskEngine URL
# 1. Ask for an upload URL (input files)
curl -s -H "Authorization: Bearer $TOKEN" -X POST $B/api/v1/uploads \
  -d '{"plugin":"comfyui","filename":"fox.png","mime":"image/png","size":123456}'
# -> {"file_id":"…","method":"PUT","url":"https://…r2.cloudflarestorage.com/…","headers":{"Content-Type":"image/png"},…}
# 2. PUT the bytes to that URL with exactly those headers
curl -X PUT -H "Content-Type: image/png" --data-binary @fox.png "<url>"
# 3. Reference it in task params as {"upload": "<file_id>"}
curl -s -H "Authorization: Bearer $TOKEN" -X POST $B/api/v1/tasks \
  -d '{"plugin_name":"comfyui","params":{"workflow":"…","inputs":{"image":{"upload":"<file_id>"}}}}'
# 4. Poll the task; completed tasks list output files with presigned URLs (1h)
curl -s -H "Authorization: Bearer $TOKEN" $B/api/v1/tasks/<task id>   # .output, .files[].url
# or redirect to a fresh URL any time: GET /api/v1/blobs/<file id>[?download=1]
```

* Task creation fails with 400 if an upload is missing, a different size than declared, expired, or belongs to another plugin.
* When a worker claims the task, the server adds `url`, `filename`, `mime`, `size` to each `{"upload": …}` ref (6h URLs, never stored in the DB). If an input expired while queued the task fails permanently.
* Workers upload outputs via `POST /api/v1/tasks/{id}/outputs` (plugins use `plugin.IOFrom(reporter).UploadOutput(...)`); completion marks them READY and stores the plugin's `output` details on the task.
* A sweep marks files `EXPIRED` after retention; R2 lifecycle deletes the objects.

---

## 🎨 ComfyUI Plugin (`comfyui`)

Runs ComfyUI workflows from templates in `tasks/assets/comfyui/`:

* `<name>.json`: workflow in **API format** (ComfyUI → Workflow → Export (API)). UI-format files are rejected.
* `<name>.yaml`: manifest declaring the parameters a task may set; everything else is fixed.

```yaml
title: "Jeveration text-to-image"
workflow: jeveration_t2i.json
params:
  prompt:     {node: "2", input: text, type: text, required: true, max_length: 2000}
  seed:       {node: "3", input: seed, type: seed}            # random when omitted
  batch_size: {node: "3", input: batch_size, type: int, default: 4, min: 1, max: 4}
  # image:    {node: "12", input: image, type: image}        # value: {"upload": "<file id>"}
```

Types: `text`, `int`, `number`, `bool`, `seed` (0..2^64-1), `choice` (`options: [...]`), `image` (any uploaded file staged into ComfyUI's `input/taskengine/<task id>/`).

```bash
curl -s -H "Authorization: Bearer $TOKEN" -X POST $B/api/v1/tasks \
  -d '{"plugin_name":"comfyui","params":{"workflow":"jeveration_t2i","inputs":{"prompt":"a red fox in snow"}}}'
```

**Execution:** download inputs → `POST /upload/image` → listen on `/ws` → `POST /prompt` → progress from websocket events (completion confirmed via `/history`) → download each `type: output` file via `/view` → upload to R2 (+ 384px JPEG thumbnail for images). Task `output`:

```json
{"workflow":"jeveration_t2i","prompt_id":"…","params":{"prompt":"…","seed":1773552633208855,"batch_size":1},
 "outputs":[{"file_id":"…","thumbnail_id":"…","filename":"jeveration_00013_.png","node":"5","kind":"images","mime":"image/png"}]}
```

**Errors:** invalid params, ComfyUI validation errors and node execution errors fail permanently (no GPU-burning retries); ComfyUI being unreachable is retried. Cancelling/timeout (`timeout_minutes`, default 60) deletes the prompt from ComfyUI's queue and interrupts it.

### Running the worker (root-pc)

A dedicated worker process (`tasks/workers/rootpc-comfy.yaml`, concurrency 1) connects to the public VPS server, alongside the existing `rootpc` worker that serves the Mac mini:

```bash
TASKENGINE_URL=https://<your-taskengine-host> TASKENGINE_TOKEN=<worker token> make taskengine-comfy-worker
# = taskengine worker --server-url $TASKENGINE_URL --worker-id rootpc-comfy
```

Plugin config (`plugin_configs.comfyui`): `url` (default `http://127.0.0.1:8188`), `assets_path`, `timeout_minutes`, `thumbnail_size` (`-1` disables).

---

## 🔮 Future Extensions & Roadmap

### 1. 🌐 Dedicated Multi-Host Topology & Webhooks
* **Decoupled Architecture**: Media/Jellyfin server running on a separate host, TaskEngine server on Mac mini, and workers distributed across laptops, PCs, and microcontrollers.
* **Webhook Integration**:
  * Ingress: Sonarr/Radarr triggers tasks immediately upon new media arrival.
  * Egress: Automatically calls Jellyfin `/Library/Refresh` API when video transcoding completes.

### 2. ⚡ Generic IoT / Microcontroller Workers
* Pure HTTP/JSON worker protocol allows ESP32, Raspberry Pi Pico W, and Arduino devices to act as distributed workers for:
  * Scheduled garden irrigation and relay switching.
  * Environment sensor telemetry collection (BME280, SCD40, DHT22).
  * ESP32-CAM periodic photo capture.

### 3. 🔋 Battery-Aware Scheduling
* Dynamic throttling of heavy video transcoding jobs when worker laptops are on battery power.
