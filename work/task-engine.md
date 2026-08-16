# 🚀 TaskEngine: Distributed Go Task Orchestration Architecture & Plan

`TaskEngine` is a lightweight, distributed task execution system written in Go. It operates as a single binary with dual roles (`server` or `worker`), featuring an open plugin architecture, atomic task claiming via pure-Go SQLite, file-based configuration tracked in git (`tasks/`), hierarchical configuration inheritance with per-worker path translation, and an embedded reactive Web UI (HTMX + SSE).

---

## 📐 Key Architecture & Design Decisions

### 1. Dual-Mode Single Binary
* **Server Mode (`taskengine server`)**:
  * Runs pure-Go SQLite (`modernc.org/sqlite`, CGO-free) in WAL mode.
  * Reads configuration exclusively from files checked into the `tasks/` directory in the repository.
  * Serves as the single source of truth for all tasks, worker settings, and path mappings.
  * Supports dynamic reload via `POST /api/v1/config/reload` (as well as clean server restart).
  * Exposes HTTP REST API and SSE endpoints for worker registration, task claiming, progress streaming, log tailing, and status updates.
  * Serves the embedded HTMX dashboard.
  * Manages atomic task queuing, worker heartbeats, and stale task recovery.
  * Runs server-side task producers (e.g. cron schedules, directory monitors, custom plugin generators).
* **Worker Mode (`taskengine worker --server-url http://<server>:8080 --worker-id <id>`)**:
  * Connects to the server strictly via `--server-url`.
  * Fetches its configuration and path translation mappings dynamically from the server upon registration.
  * Polls the server for available tasks matching its capabilities.
  * Translates server file paths to worker-local paths before task execution.
  * Executes plugin tasks (either built-in Go worker plugins or external scripts/binaries).
  * Reports progress, logs, heartbeats, and completion/failure status over HTTP back to the server.

### 2. Configuration Authority & Reloading
* **Single Source of Truth**: The server's `tasks/` directory contains all global configs, worker overrides, and task definitions. Workers do not need local config files.
* **Config Updates**:
  1. **Server Restart**: Standard process restart reloads all YAML files under `tasks/`.
  2. **Reload Endpoint (`POST /api/v1/config/reload`)**: Triggers an in-memory re-parse of `tasks/` and updates active schedules and worker configurations without restarting the server. Also triggerable via a "Reload Config" button on the Web UI.

### 3. Networking & Security Scope
* **Strictly Local / LAN**: Designed for local home network, Tailscale, or wireguard mesh without overhead. Authentication tokens and encryption layers are omitted for lightweight, zero-friction local deployment.

### 4. Database Engine
* **Pure Go SQLite**: Uses `modernc.org/sqlite` to ensure 100% CGO-free cross-compilation across macOS (Apple Silicon & Intel), Linux (ARM64 & AMD64), and Windows.

```mermaid
flowchart TD
    subgraph MacMini ["Server Node (Mac mini)"]
        CLI_S[taskengine server]
        CFG[Config Loader: tasks/*.yaml]
        DB[(SQLite DB: WAL Mode)]
        API[HTTP REST & SSE API]
        UI[Embedded HTMX Dashboard]
        SP[Server Generators / Schedulers]

        CLI_S --> CFG
        CLI_S --> API
        API <--> DB
        UI <--> API
        SP -->|Enqueue Tasks| DB
    end

    subgraph MacbookAir ["Worker Node 1 (MacBook Air)"]
        CLI_W1[taskengine worker --server-url ...]
        WP1[Worker Plugin: FFmpeg Transcoder]
        PT1[Path Translator: /Volumes -> ~/Desktop]

        CLI_W1 -->|1. Register & Fetch Config| API
        CLI_W1 -->|2. Claim Task via HTTP| API
        CLI_W1 -->|3. Translate Paths| PT1
        PT1 -->|4. Run Job| WP1
        WP1 -->|5. Stream Progress / Logs / Heartbeat via HTTP| API
    end

    subgraph GenericWorker ["Worker Node 2 (Custom / Script Worker)"]
        CLI_W2[External Script / Runner (Python, Bash, Go)]
        
        CLI_W2 -->|HTTP Claim / Progress / Complete| API
    end
```

---

## 🔌 Universal Plugin Architecture & HTTP Protocol

Plugins are **not** restricted to video processing or Go code. Any process in any language (Go, Python, Bash, Node.js, Rust, etc.) can generate tasks or execute them by interacting with the TaskEngine server over HTTP.

### HTTP Server & Worker API Contract

| Endpoint | Method | Payload | Description |
| :--- | :--- | :--- | :--- |
| `/api/v1/config/reload` | `POST` | `{}` | Reloads all configuration files from `tasks/` into server memory. |
| `/api/v1/workers/register` | `POST` | `{"worker_id": "...", "hostname": "...", "plugins": [...]}` | Registers worker; returns effective worker config & path mappings. |
| `/api/v1/workers/{id}/heartbeat` | `POST` | `{}` | Keeps the worker alive; prevents task re-queueing. |
| `/api/v1/tasks/claim` | `POST` | `{"worker_id": "...", "plugins": [...]}` | Atomically claims the next pending task. |
| `/api/v1/tasks/{id}/progress` | `POST` | `{"progress": 45.2, "speed": "1.8x", "message": "..."}` | Reports incremental execution progress. |
| `/api/v1/tasks/{id}/logs` | `POST` | `{"log_chunk": "..."}` | Appends log snippets to the task log stream. |
| `/api/v1/tasks/{id}/complete` | `POST` | `{"output_details": {...}}` | Marks task as successfully completed. |
| `/api/v1/tasks/{id}/fail` | `POST` | `{"error_message": "...", "can_retry": true}` | Marks task as failed or schedules retry. |

### Go Plugin Interfaces (Built-in & Custom)

```go
package plugin

import (
	"context"
	"encoding/json"
)

// TaskPayload represents the data passed from Server to Worker for a job.
type TaskPayload struct {
	ID         string          `json:"id"`
	PluginName string          `json:"plugin_name"`
	TargetFile string          `json:"target_file,omitempty"`
	Params     json.RawMessage `json:"params"`
}

// ProgressReport is sent from Worker to Server during task execution.
type ProgressReport struct {
	Progress float64 `json:"progress"` // 0.0 to 100.0
	Speed    string  `json:"speed"`    // e.g. "1.5x"
	Message  string  `json:"message"`  // Current operation status
	LogChunk string  `json:"log_chunk"`// Output snippet
}

// ProgressReporter allows worker plugins to send real-time updates.
type ProgressReporter interface {
	Report(ctx context.Context, report ProgressReport) error
}

// ServerPlugin generates/schedules tasks on the server.
type ServerPlugin interface {
	Name() string
	Init(ctx context.Context, config json.RawMessage) error
	GenerateTasks(ctx context.Context) ([]TaskPayload, error)
}

// WorkerPlugin executes claimed tasks on a worker.
type WorkerPlugin interface {
	Name() string
	Init(ctx context.Context, config json.RawMessage) error
	Execute(ctx context.Context, payload TaskPayload, reporter ProgressReporter) error
}
```

### Generic Command Runner Plugin (`command-runner`)
In addition to native Go plugins, TaskEngine provides a built-in `command-runner` plugin supporting any language/script (Python, Bash, Node.js, binaries):
* **Generic Task-Level Prerequisites**:
  * Any task can define a `prerequisites.check_command` (e.g. `which ffmpeg`, `python3 -c "import whisper"`).
  * The worker executes the check before running the job. If exit code is 0, the job proceeds; if non-zero, the task fails with clear error logs.
* **Task-Level Asset Syncing (Git Commit Tracked)**:
  * Tasks can define task assets (e.g., `tasks/assets/<task_name>/*` or specific script files).
  * Assets are versioned using the repository's git commit hash and SHA256 checksums.
  * When a worker claims a task, it checks its local cache in `~/.taskengine/tasks/<task_name>/assets/`. If the version has changed or files are missing, it refreshes them automatically from the server.
* **Injected Environment Variables**:
  * `TASK_ID`, `TASK_PLUGIN`, `TASK_NAME`, `TASK_TARGET_FILE`, `TASK_PARAMS_JSON`
  * `TASK_DIR` (`~/.taskengine/tasks/<task_name>`)
  * `TASK_ASSETS_DIR` (`~/.taskengine/tasks/<task_name>/assets`)
  * `TASK_CACHE_DIR` (`~/.taskengine`)
  * `SERVER_URL`, `WORKER_ID`
* **Real-time Progress & Log Interception**:
  * Automatically captures stdout/stderr into the task's live log stream.
  * Extracts structured progress via text patterns (e.g. `PROGRESS: 45%`) or JSON lines (`{"progress": 45, "speed": "1.8x"}`).

---

## 📁 File-Based Configuration in `tasks/`

All task configurations, schedules, and worker defaults are checked into git under the `tasks/` directory on the server host.

### Directory Structure of `tasks/`
```text
home/
├── tasks/
│   ├── config.yaml            # Global server settings (polling intervals, defaults)
│   ├── workers/               # Per-worker override definitions
│   │   ├── laptop1.yaml       # Worker path mappings, concurrency, plugin configs
│   │   └── desktop_gpu.yaml
│   └── definitions/           # Task generators, cron jobs, and plugin definitions
│       ├── video_transcode.yaml
│       ├── backups.yaml
│       └── custom_script.yaml
```

### Sample: `tasks/config.yaml`
```yaml
server:
  port: 8080
  db_path: "/Volumes/drive001/taskengine/tasks.db"
  heartbeat_timeout_seconds: 60
  stale_task_check_interval_seconds: 30

defaults:
  max_concurrent_tasks: 1
  path_mappings:
    "/Volumes/drive001/media": "/media"
```

### Sample: `tasks/workers/laptop1.yaml`
```yaml
worker_id: "laptop1"
max_concurrent_tasks: 2
path_mappings:
  "/Volumes/drive001/media": "/Users/air/Desktop/macmini_media"
plugin_configs:
  video-transcoder:
    target_codec: "hevc_videotoolbox"
    ffmpeg_binary: "/opt/homebrew/bin/ffmpeg"
  command-runner:
    shell: "/bin/zsh"
```

### Sample: `tasks/definitions/video_transcode.yaml`
```yaml
name: "video-transcoder"
type: "scanner"
schedule: "*/30 * * * *"
enabled: true
scanner:
  directory: "/Volumes/drive001/media/incoming"
  target_extensions: [".mkv", ".mp4", ".mov"]
  target_codec: "hevc"
params:
  crf: 23
  preset: "medium"
  container: "mp4"
```

---

## 🗄️ Database Schema (Pure-Go SQLite WAL Mode)

```sql
-- Tasks table
CREATE TABLE IF NOT EXISTS tasks (
    id TEXT PRIMARY KEY,
    plugin_name TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'RUNNING', 'COMPLETED', 'FAILED', 'CANCELLED')),
    payload TEXT NOT NULL,          -- JSON task payload
    target_file TEXT,
    worker_id TEXT,
    priority INTEGER DEFAULT 0,
    progress REAL DEFAULT 0.0,
    speed TEXT DEFAULT '',
    status_message TEXT DEFAULT '',
    log_output TEXT DEFAULT '',
    retry_count INTEGER DEFAULT 0,
    max_retries INTEGER DEFAULT 3,
    claimed_at DATETIME,
    last_heartbeat DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Workers registration and heartbeats
CREATE TABLE IF NOT EXISTS workers (
    id TEXT PRIMARY KEY,
    hostname TEXT NOT NULL,
    enabled_plugins TEXT NOT NULL,  -- JSON array
    last_seen DATETIME DEFAULT CURRENT_TIMESTAMP,
    status TEXT NOT NULL DEFAULT 'ACTIVE'
);

-- Indices for performance
CREATE INDEX IF NOT EXISTS idx_tasks_claim ON tasks(status, plugin_name, priority DESC, created_at ASC);
CREATE INDEX IF NOT EXISTS idx_tasks_worker ON tasks(worker_id, status);
```

---

## 📁 Repository Directory Layout

```text
home/
├── tasks/                     # Checked-in task, worker, and scheduler configs
│   ├── config.yaml
│   ├── workers/
│   │   └── sample-worker.yaml
│   └── definitions/
│       └── sample-task.yaml
├── work/
│   └── task-engine.md         # Architecture specification & plan
└── src/
    └── taskengine/
        ├── go.mod
        ├── go.sum
        ├── Makefile
        ├── cmd/
        │   └── taskengine/
        │       └── main.go     # Entry point for server & worker subcommands
        ├── pkg/
        │   ├── config/         # YAML loader from tasks/, path translator
        │   ├── db/             # SQLite (modernc.org/sqlite), schema migrations
        │   ├── plugin/         # Plugin interfaces and HTTP client/server bridges
        │   ├── plugins/
        │   │   ├── transcoder/ # Sample video transcoder plugin (Go native)
        │   │   └── runner/     # Generic script/command runner plugin
        │   ├── server/         # REST API, SSE hub, scheduler, heartbeat sweeper
        │   └── worker/         # Worker loop, task claimer, heartbeat sender
        └── web/
            ├── embed.go        # go:embed for static files and templates
            ├── static/         # HTMX, CSS, JS
            └── templates/      # HTML templates (dashboard, task list, workers)
```

---

## 🛠️ Step-by-Step Implementation Roadmap

1. **Phase 1: Project Setup & Config Engine (`tasks/`)** [✓ Completed]
   - Created `tasks/` directory structure with base YAML files.
   - Initialized Go module in `src/taskengine/` with `modernc.org/sqlite` and `gopkg.in/yaml.v3`.
   - Implemented `pkg/config` (loads `tasks/` YAML files, merges worker overrides, executes longest-prefix path translation).
   - Implemented `pkg/db` (pure-Go SQLite connection in WAL mode with auto-migrations and atomic transactions).

2. **Phase 2: Core Server Engine & REST/SSE API** [✓ Completed]
   - Implemented REST endpoints for worker registration, atomic task claiming, progress reports, log streaming, task creation, and completions.
   - Implemented `POST /api/v1/config/reload` endpoint.
   - Implemented Server-Sent Events (SSE) broadcaster for real-time frontend updates.
   - Implemented background stale task recovery and heartbeat cleaner.

3. **Phase 3: Plugin Engine & Workers** [✓ Completed]
   - Implemented generic Worker loop (poll, claim, path translate, execute, report).
   - Implemented `pkg/plugins/runner` (Generic script/command executor with progress interception).
   - Implemented `pkg/plugins/transcoder` (1080p Universal Jellyfin Direct Play FFmpeg wrapper with progress/speed parsing).

4. **Phase 4: Embedded Web UI** [✓ Completed]
   - Embedded HTMX, vanilla CSS, and JavaScript templates via `go:embed`.
   - Built real-time reactive dashboard (Task queue, active workers, live progress bars, log viewer modal, new task creator, config reload button).

5. **Phase 5: Task Prerequisites & Git-Synced Assets** [✓ Completed]
   - Implemented generic task-level prerequisites (`prerequisites.check_command`).
   - Implemented git commit-hash & checksum asset synchronization into `~/.taskengine/tasks/<name>/assets/`.
   - Verified auto-refresh on every task execution.

6. **Phase 6: Cross-Compilation, Automation & Documentation** [✓ Completed]
   - Added Makefile automation for server, worker, reload, status, e2e, and cross-compilation (`make taskengine-release`).
   - Cross-compiled binaries for macOS (ARM64/Intel) and Linux (x86_64/ARM64).
   - Created comprehensive documentation manual in [docs/taskengine.md](file:///Users/server/github/home/docs/taskengine.md) and updated [README.md](file:///Users/server/github/home/README.md).
