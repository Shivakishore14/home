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
}

// SSEEvent represents a live event broadcasted to connected web clients.
type SSEEvent struct {
	Type      string      `json:"type"`
	Timestamp time.Time   `json:"timestamp"`
	Data      interface{} `json:"data"`
}
