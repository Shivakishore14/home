package models

import (
	"encoding/json"
	"time"
)

// Task status constants
const (
	StatusPending   = "PENDING"
	StatusRunning   = "RUNNING"
	StatusCompleted = "COMPLETED"
	StatusFailed    = "FAILED"
	StatusCancelled = "CANCELLED"
)

// Worker status constants
const (
	WorkerStatusActive  = "ACTIVE"
	WorkerStatusOffline = "OFFLINE"
)

// Task represents a unit of work stored in SQLite.
type Task struct {
	ID            string          `json:"id"`
	PluginName    string          `json:"plugin_name"`
	Status        string          `json:"status"`
	Payload       json.RawMessage `json:"payload"`
	TargetFile    string          `json:"target_file,omitempty"`
	WorkerID      string          `json:"worker_id,omitempty"`
	Priority      int             `json:"priority"`
	Progress      float64         `json:"progress"`
	Speed         string          `json:"speed,omitempty"`
	StatusMessage string          `json:"status_message,omitempty"`
	LogOutput     string          `json:"log_output,omitempty"`
	RetryCount    int             `json:"retry_count"`
	MaxRetries    int             `json:"max_retries"`
	ClaimedAt     *time.Time      `json:"claimed_at,omitempty"`
	LastHeartbeat *time.Time      `json:"last_heartbeat,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	// Output holds plugin-reported result details (set on completion).
	Output json.RawMessage `json:"output,omitempty"`
	// Callback is where the server reports the task's final state, if set.
	Callback *TaskCallback `json:"callback,omitempty"`
}

// TaskCallback asks the server to POST the task's final state to URL once it
// completes, fails for good, or is cancelled. Ref is echoed back unchanged so
// the client can find its own record without storing the task ID.
type TaskCallback struct {
	URL string `json:"url"`
	Ref string `json:"ref,omitempty"`
}

// Callback events, one per terminal task state.
const (
	EventTaskCompleted = "task.completed"
	EventTaskFailed    = "task.failed"
	EventTaskCancelled = "task.cancelled"
)

// Callback delivery statuses.
const (
	CallbackPending   = "PENDING"
	CallbackDelivered = "DELIVERED"
	CallbackFailed    = "FAILED" // gave up after the last retry
)

// CallbackDelivery is one queued webhook call (outbox row).
type CallbackDelivery struct {
	ID            string     `json:"id"`
	TaskID        string     `json:"task_id"`
	Event         string     `json:"event"`
	URL           string     `json:"url"`
	Ref           string     `json:"ref,omitempty"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	LastStatus    int        `json:"last_status,omitempty"` // HTTP status of the last attempt
	LastError     string     `json:"last_error,omitempty"`
	NextAttemptAt time.Time  `json:"next_attempt_at"`
	CreatedAt     time.Time  `json:"created_at"`
	DeliveredAt   *time.Time `json:"delivered_at,omitempty"`
}

// CallbackPayload is the JSON body POSTed to a task's callback URL.
type CallbackPayload struct {
	DeliveryID string          `json:"delivery_id"`
	Event      string          `json:"event"`
	TaskID     string          `json:"task_id"`
	Ref        string          `json:"ref,omitempty"`
	PluginName string          `json:"plugin_name"`
	Status     string          `json:"status"`
	Message    string          `json:"message,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	// Files are the task's outputs; ready ones carry a presigned GET URL minted at send time.
	Files      []File    `json:"files"`
	OccurredAt time.Time `json:"occurred_at"`
}

// Worker represents a registered worker node.
type Worker struct {
	ID             string    `json:"id"`
	Hostname       string    `json:"hostname"`
	EnabledPlugins []string  `json:"enabled_plugins"`
	LastSeen       time.Time `json:"last_seen"`
	Status         string    `json:"status"`
}

// RegisterWorkerRequest payload sent from worker during startup.
type RegisterWorkerRequest struct {
	WorkerID       string   `json:"worker_id"`
	Hostname       string   `json:"hostname"`
	EnabledPlugins []string `json:"enabled_plugins"`
}

// RegisterWorkerResponse returns effective worker config & path mappings.
type RegisterWorkerResponse struct {
	WorkerID           string                 `json:"worker_id"`
	MaxConcurrentTasks int                    `json:"max_concurrent_tasks"`
	EnabledPlugins     []string               `json:"enabled_plugins,omitempty"`
	PluginLimits       map[string]int         `json:"plugin_limits,omitempty"`
	ScratchDir         string                 `json:"scratch_dir,omitempty"`
	PathMappings       map[string]string      `json:"path_mappings"`
	PluginConfigs      map[string]interface{} `json:"plugin_configs"`
	HeartbeatInterval  int                    `json:"heartbeat_interval_seconds"`
}

// ClaimTaskRequest payload sent by worker to request available tasks.
type ClaimTaskRequest struct {
	WorkerID string   `json:"worker_id"`
	Plugins  []string `json:"plugins"`
}

// ProgressRequest payload sent by worker to update task execution state.
type ProgressRequest struct {
	Progress float64 `json:"progress"` // 0.0 - 100.0
	Speed    string  `json:"speed,omitempty"`
	Message  string  `json:"message,omitempty"`
}

// LogChunkRequest payload sent by worker to append log lines.
type LogChunkRequest struct {
	LogChunk string `json:"log_chunk"`
}

// CompleteTaskRequest payload sent by worker when task completes successfully.
type CompleteTaskRequest struct {
	OutputDetails map[string]interface{} `json:"output_details,omitempty"`
	Message       string                 `json:"message,omitempty"`
	// Files lists output file IDs (from POST /tasks/{id}/outputs) the worker uploaded.
	Files []string `json:"files,omitempty"`
}

// FailTaskRequest payload sent by worker when task errors out.
type FailTaskRequest struct {
	ErrorMessage string `json:"error_message"`
	CanRetry     bool   `json:"can_retry"`
}

// TaskPrerequisites represents task-level dependency checks.
type TaskPrerequisites struct {
	CheckCommand string `json:"check_command,omitempty" yaml:"check_command,omitempty"`
}

// TaskAssetItem represents a single asset file and its checksum.
type TaskAssetItem struct {
	Path     string `json:"path"`     // Relative path e.g. "scripts/transcribe.py"
	Checksum string `json:"checksum"` // SHA256 checksum
	Size     int64  `json:"size"`
}

// TaskAssets represents task-level files/scripts to sync to workers.
type TaskAssets struct {
	TaskName string          `json:"task_name,omitempty" yaml:"task_name,omitempty"`
	Version  string          `json:"version,omitempty" yaml:"version,omitempty"` // Git commit hash or asset hash
	Files    []TaskAssetItem `json:"files,omitempty" yaml:"files,omitempty"`
}

// CreateTaskRequest payload to manually enqueue a task via API or UI.
type CreateTaskRequest struct {
	PluginName string          `json:"plugin_name"`
	TargetFile string          `json:"target_file,omitempty"`
	Priority   int             `json:"priority,omitempty"`
	MaxRetries int             `json:"max_retries,omitempty"`
	Params     json.RawMessage `json:"params"`
	// Callback, if set, is called once the task reaches a final state.
	Callback *TaskCallback `json:"callback,omitempty"`
}

// SSEEvent represents a live event broadcasted to connected web clients.
type SSEEvent struct {
	Type      string      `json:"type"`
	Timestamp time.Time   `json:"timestamp"`
	Data      interface{} `json:"data"`
}

// File kinds and statuses for stored objects.
const (
	FileKindUpload = "upload" // uploaded by a client, referenced from task params
	FileKindOutput = "output" // produced by a worker for a task

	FileStatusPending = "PENDING" // presigned, upload not yet confirmed
	FileStatusReady   = "READY"
	FileStatusExpired = "EXPIRED" // past retention; object deleted by bucket lifecycle
)

// File is a stored object (R2) tracked by the server.
type File struct {
	ID        string            `json:"id"`
	Plugin    string            `json:"plugin"`
	TaskID    string            `json:"task_id,omitempty"`
	Kind      string            `json:"kind"`
	ObjectKey string            `json:"-"`
	Filename  string            `json:"filename"`
	MIME      string            `json:"mime"`
	Size      int64             `json:"size"`
	Status    string            `json:"status"`
	Meta      map[string]string `json:"meta,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	ExpiresAt time.Time         `json:"expires_at"`
	URL       string            `json:"url,omitempty"` // presigned GET, only in responses
}

// PresignUploadRequest asks for an upload URL (client -> server).
type PresignUploadRequest struct {
	Plugin   string `json:"plugin"`
	Filename string `json:"filename"`
	MIME     string `json:"mime"`
	Size     int64  `json:"size"`
}

// PresignOutputRequest asks for an output upload URL (worker -> server).
type PresignOutputRequest struct {
	Filename string            `json:"filename"`
	MIME     string            `json:"mime"`
	Size     int64             `json:"size"`
	Meta     map[string]string `json:"meta,omitempty"`
}

// PresignResponse returns the new file ID and the request to upload it.
type PresignResponse struct {
	FileID  string            `json:"file_id"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	// ExpiresAt is when the upload URL stops working.
	ExpiresAt time.Time `json:"expires_at"`
}

// TaskDetail is a task plus its stored files (GET /api/v1/tasks/{id}).
type TaskDetail struct {
	Task
	Files     []File             `json:"files"`
	Callbacks []CallbackDelivery `json:"callbacks,omitempty"`
}
