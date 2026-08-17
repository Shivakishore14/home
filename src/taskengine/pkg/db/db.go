package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"taskengine/pkg/models"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// DB encapsulates SQLite database operations.
type DB struct {
	db *sql.DB
	mu sync.Mutex
}

// Stats provides summary metrics for dashboard and monitoring.
type Stats struct {
	PendingTasks   int `json:"pending_tasks"`
	RunningTasks   int `json:"running_tasks"`
	CompletedTasks int `json:"completed_tasks"`
	FailedTasks    int `json:"failed_tasks"`
	CancelledTasks int `json:"cancelled_tasks"`
	TotalTasks     int `json:"total_tasks"`
	ActiveWorkers  int `json:"active_workers"`
}

// Open initializes SQLite connection and runs schema migrations.
func Open(dbPath string) (*DB, error) {
	if dbPath == "" {
		dbPath = "taskengine.db"
	}

	dir := filepath.Dir(dbPath)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create db directory %s: %w", dir, err)
		}
	}

	// SQLite connection string with pragmas
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)", dbPath)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite db: %w", err)
	}

	// SQLite single-writer safety
	sqlDB.SetMaxOpenConns(1)

	db := &DB{db: sqlDB}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("failed to run database migrations: %w", err)
	}

	return db, nil
}

// Close closes the underlying SQLite database.
func (d *DB) Close() error {
	return d.db.Close()
}

// migrate creates necessary tables and indices.
func (d *DB) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS tasks (
		id TEXT PRIMARY KEY,
		plugin_name TEXT NOT NULL,
		status TEXT NOT NULL CHECK (status IN ('PENDING', 'RUNNING', 'COMPLETED', 'FAILED', 'CANCELLED')),
		payload TEXT NOT NULL,
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

	CREATE TABLE IF NOT EXISTS workers (
		id TEXT PRIMARY KEY,
		hostname TEXT NOT NULL,
		enabled_plugins TEXT NOT NULL,
		last_seen DATETIME DEFAULT CURRENT_TIMESTAMP,
		status TEXT NOT NULL DEFAULT 'ACTIVE'
	);

	CREATE INDEX IF NOT EXISTS idx_tasks_claim ON tasks(status, plugin_name, priority DESC, created_at ASC);
	CREATE INDEX IF NOT EXISTS idx_tasks_worker ON tasks(worker_id, status);
	CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status, updated_at DESC);
	`
	_, err := d.db.Exec(schema)
	return err
}

// CreateTask inserts a new task into the database.
func (d *DB) CreateTask(ctx context.Context, req models.CreateTaskRequest) (*models.Task, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	id := uuid.New().String()
	priority := req.Priority
	maxRetries := req.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	payloadBytes := req.Params
	if len(payloadBytes) == 0 {
		payloadBytes = []byte("{}")
	}

	now := time.Now().UTC()
	query := `
	INSERT INTO tasks (id, plugin_name, status, payload, target_file, priority, max_retries, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := d.db.ExecContext(ctx, query, id, req.PluginName, models.StatusPending, string(payloadBytes), req.TargetFile, priority, maxRetries, now, now)
	if err != nil {
		return nil, fmt.Errorf("failed to insert task: %w", err)
	}

	return &models.Task{
		ID:         id,
		PluginName: req.PluginName,
		Status:     models.StatusPending,
		Payload:    payloadBytes,
		TargetFile: req.TargetFile,
		Priority:   priority,
		MaxRetries: maxRetries,
		CreatedAt:  now,
		UpdatedAt:  now,
	}, nil
}

// GetTask retrieves a single task by ID.
func (d *DB) GetTask(ctx context.Context, id string) (*models.Task, error) {
	query := `
	SELECT id, plugin_name, status, payload, target_file, worker_id, priority, progress, speed, status_message, log_output, retry_count, max_retries, claimed_at, last_heartbeat, created_at, updated_at
	FROM tasks WHERE id = ?
	`
	row := d.db.QueryRowContext(ctx, query, id)

	var t models.Task
	var payloadStr string
	var targetFile, workerID, speed, statusMsg, logOut sql.NullString
	var claimedAt, lastHb sql.NullTime

	err := row.Scan(
		&t.ID, &t.PluginName, &t.Status, &payloadStr, &targetFile, &workerID,
		&t.Priority, &t.Progress, &speed, &statusMsg, &logOut,
		&t.RetryCount, &t.MaxRetries, &claimedAt, &lastHb, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to query task %s: %w", id, err)
	}

	t.Payload = json.RawMessage(payloadStr)
	if targetFile.Valid {
		t.TargetFile = targetFile.String
	}
	if workerID.Valid {
		t.WorkerID = workerID.String
	}
	if speed.Valid {
		t.Speed = speed.String
	}
	if statusMsg.Valid {
		t.StatusMessage = statusMsg.String
	}
	if logOut.Valid {
		t.LogOutput = logOut.String
	}
	if claimedAt.Valid {
		t.ClaimedAt = &claimedAt.Time
	}
	if lastHb.Valid {
		t.LastHeartbeat = &lastHb.Time
	}

	return &t, nil
}

// HasPendingOrRunningTask checks if there is already an active (PENDING or RUNNING) task for a plugin and target.
func (d *DB) HasPendingOrRunningTask(ctx context.Context, pluginName, targetFile string) (bool, error) {
	var count int
	query := `SELECT COUNT(1) FROM tasks WHERE status IN ('PENDING', 'RUNNING') AND plugin_name = ? AND target_file = ?`
	err := d.db.QueryRowContext(ctx, query, pluginName, targetFile).Scan(&count)
	return count > 0, err
}

// ListTasks returns tasks filtered by optional status, ordered by recent updates.
func (d *DB) ListTasks(ctx context.Context, statusFilter string, limit, offset int) ([]models.Task, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	var query string
	var args []interface{}

	if statusFilter != "" {
		query = `
		SELECT id, plugin_name, status, payload, target_file, worker_id, priority, progress, speed, status_message, log_output, retry_count, max_retries, claimed_at, last_heartbeat, created_at, updated_at
		FROM tasks
		WHERE status = ?
		ORDER BY created_at DESC
		LIMIT ? OFFSET ?
		`
		args = append(args, statusFilter, limit, offset)
	} else {
		query = `
		SELECT id, plugin_name, status, payload, target_file, worker_id, priority, progress, speed, status_message, log_output, retry_count, max_retries, claimed_at, last_heartbeat, created_at, updated_at
		FROM tasks
		ORDER BY created_at DESC
		LIMIT ? OFFSET ?
		`
		args = append(args, limit, offset)
	}

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []models.Task
	for rows.Next() {
		var t models.Task
		var payloadStr string
		var targetFile, workerID, speed, statusMsg, logOut sql.NullString
		var claimedAt, lastHb sql.NullTime

		if err := rows.Scan(
			&t.ID, &t.PluginName, &t.Status, &payloadStr, &targetFile, &workerID,
			&t.Priority, &t.Progress, &speed, &statusMsg, &logOut,
			&t.RetryCount, &t.MaxRetries, &claimedAt, &lastHb, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan task row: %w", err)
		}

		t.Payload = json.RawMessage(payloadStr)
		if targetFile.Valid {
			t.TargetFile = targetFile.String
		}
		if workerID.Valid {
			t.WorkerID = workerID.String
		}
		if speed.Valid {
			t.Speed = speed.String
		}
		if statusMsg.Valid {
			t.StatusMessage = statusMsg.String
		}
		if logOut.Valid {
			t.LogOutput = logOut.String
		}
		if claimedAt.Valid {
			t.ClaimedAt = &claimedAt.Time
		}
		if lastHb.Valid {
			t.LastHeartbeat = &lastHb.Time
		}

		tasks = append(tasks, t)
	}

	return tasks, nil
}

// ClaimNextTask atomically claims the highest priority pending task matching the worker's enabled plugins.
func (d *DB) ClaimNextTask(ctx context.Context, workerID string, plugins []string) (*models.Task, error) {
	if len(plugins) == 0 {
		return nil, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin tx: %w", err)
	}
	defer tx.Rollback()

	placeholders := make([]string, len(plugins))
	args := make([]interface{}, len(plugins))
	for i, p := range plugins {
		placeholders[i] = "?"
		args[i] = p
	}

	// Select top candidate
	selectQuery := fmt.Sprintf(`
	SELECT id, plugin_name, payload, target_file, priority, retry_count, max_retries, created_at
	FROM tasks
	WHERE status = 'PENDING' AND plugin_name IN (%s)
	ORDER BY priority DESC, created_at ASC
	LIMIT 1
	`, strings.Join(placeholders, ","))

	row := tx.QueryRowContext(ctx, selectQuery, args...)

	var t models.Task
	var payloadStr string
	var targetFile sql.NullString

	err = row.Scan(&t.ID, &t.PluginName, &payloadStr, &targetFile, &t.Priority, &t.RetryCount, &t.MaxRetries, &t.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // No task available
		}
		return nil, fmt.Errorf("failed to query next task: %w", err)
	}

	now := time.Now().UTC()
	updateQuery := `
	UPDATE tasks
	SET status = 'RUNNING', worker_id = ?, claimed_at = ?, last_heartbeat = ?, updated_at = ?
	WHERE id = ? AND status = 'PENDING'
	`
	res, err := tx.ExecContext(ctx, updateQuery, workerID, now, now, now, t.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to claim task %s: %w", t.ID, err)
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil || rowsAffected == 0 {
		return nil, nil // Race condition; task was claimed elsewhere
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit claim tx: %w", err)
	}

	t.Status = models.StatusRunning
	t.WorkerID = workerID
	t.Payload = json.RawMessage(payloadStr)
	if targetFile.Valid {
		t.TargetFile = targetFile.String
	}
	t.ClaimedAt = &now
	t.LastHeartbeat = &now
	t.UpdatedAt = now

	return &t, nil
}

// UpdateTaskProgress updates progress percentage, speed, and current message.
func (d *DB) UpdateTaskProgress(ctx context.Context, id string, progress float64, speed, message string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now().UTC()
	query := `
	UPDATE tasks
	SET progress = ?, speed = ?, status_message = ?, last_heartbeat = ?, updated_at = ?
	WHERE id = ? AND status = 'RUNNING'
	`
	_, err := d.db.ExecContext(ctx, query, progress, speed, message, now, now, id)
	return err
}

// AppendTaskLog appends a chunk of log output to the task.
func (d *DB) AppendTaskLog(ctx context.Context, id, logChunk string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now().UTC()
	query := `
	UPDATE tasks
	SET log_output = log_output || ?, last_heartbeat = ?, updated_at = ?
	WHERE id = ?
	`
	_, err := d.db.ExecContext(ctx, query, logChunk, now, now, id)
	return err
}

// CompleteTask marks a task as COMPLETED.
func (d *DB) CompleteTask(ctx context.Context, id, message string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now().UTC()
	query := `
	UPDATE tasks
	SET status = 'COMPLETED', progress = 100.0, status_message = ?, updated_at = ?
	WHERE id = ? AND status = 'RUNNING'
	`
	_, err := d.db.ExecContext(ctx, query, message, now, id)
	return err
}

// FailTask marks a task as FAILED or re-queues it if retries remain.
func (d *DB) FailTask(ctx context.Context, id, errorMessage string, canRetry bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	var retryCount, maxRetries int
	row := d.db.QueryRowContext(ctx, "SELECT retry_count, max_retries FROM tasks WHERE id = ?", id)
	if err := row.Scan(&retryCount, &maxRetries); err != nil {
		return err
	}

	now := time.Now().UTC()
	if canRetry && retryCount < maxRetries {
		query := `
		UPDATE tasks
		SET status = 'PENDING', worker_id = NULL, retry_count = retry_count + 1, status_message = ?, updated_at = ?
		WHERE id = ?
		`
		_, err := d.db.ExecContext(ctx, query, fmt.Sprintf("Retrying: %s", errorMessage), now, id)
		return err
	}

	query := `
	UPDATE tasks
	SET status = 'FAILED', status_message = ?, updated_at = ?
	WHERE id = ?
	`
	_, err := d.db.ExecContext(ctx, query, errorMessage, now, id)
	return err
}

// CancelTask marks a pending or running task as CANCELLED.
func (d *DB) CancelTask(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now().UTC()
	query := `
	UPDATE tasks
	SET status = 'CANCELLED', status_message = 'Cancelled by user', updated_at = ?
	WHERE id = ? AND status IN ('PENDING', 'RUNNING')
	`
	_, err := d.db.ExecContext(ctx, query, now, id)
	return err
}

// RegisterWorker registers or updates a worker's heartbeat and capabilities.
func (d *DB) RegisterWorker(ctx context.Context, workerID, hostname string, plugins []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	pluginsJSON, err := json.Marshal(plugins)
	if err != nil {
		pluginsJSON = []byte("[]")
	}

	now := time.Now().UTC()
	query := `
	INSERT INTO workers (id, hostname, enabled_plugins, last_seen, status)
	VALUES (?, ?, ?, ?, 'ACTIVE')
	ON CONFLICT(id) DO UPDATE SET
		hostname = excluded.hostname,
		enabled_plugins = excluded.enabled_plugins,
		last_seen = excluded.last_seen,
		status = 'ACTIVE'
	`
	_, err = d.db.ExecContext(ctx, query, workerID, hostname, string(pluginsJSON), now)
	return err
}

// WorkerHeartbeat updates last_seen timestamp and extends running task heartbeats.
func (d *DB) WorkerHeartbeat(ctx context.Context, workerID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now().UTC()
	// Update worker
	_, err := d.db.ExecContext(ctx, "UPDATE workers SET last_seen = ?, status = 'ACTIVE' WHERE id = ?", now, workerID)
	if err != nil {
		return err
	}

	// Update active tasks for this worker
	_, err = d.db.ExecContext(ctx, "UPDATE tasks SET last_heartbeat = ?, updated_at = ? WHERE worker_id = ? AND status = 'RUNNING'", now, now, workerID)
	return err
}

// ListWorkers returns all registered workers.
func (d *DB) ListWorkers(ctx context.Context) ([]models.Worker, error) {
	query := `
	SELECT id, hostname, enabled_plugins, last_seen, status
	FROM workers
	ORDER BY last_seen DESC
	`
	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list workers: %w", err)
	}
	defer rows.Close()

	var workers []models.Worker
	for rows.Next() {
		var w models.Worker
		var pluginsJSON string
		if err := rows.Scan(&w.ID, &w.Hostname, &pluginsJSON, &w.LastSeen, &w.Status); err != nil {
			return nil, fmt.Errorf("failed to scan worker row: %w", err)
		}
		json.Unmarshal([]byte(pluginsJSON), &w.EnabledPlugins)
		workers = append(workers, w)
	}
	return workers, nil
}

// RecoverStaleTasks detects missing heartbeats, marks workers OFFLINE, and recovers tasks.
func (d *DB) RecoverStaleTasks(ctx context.Context, timeoutSeconds int) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}

	// Mark workers offline
	threshold := time.Now().UTC().Add(-time.Duration(timeoutSeconds) * time.Second)
	_, _ = d.db.ExecContext(ctx, "UPDATE workers SET status = 'OFFLINE' WHERE last_seen < ? AND status = 'ACTIVE'", threshold)

	// Find stale running tasks
	query := `
	SELECT id, retry_count, max_retries
	FROM tasks
	WHERE status = 'RUNNING' AND last_heartbeat < ?
	`
	rows, err := d.db.QueryContext(ctx, query, threshold)
	if err != nil {
		return 0, fmt.Errorf("failed to query stale tasks: %w", err)
	}
	defer rows.Close()

	type staleTask struct {
		id         string
		retryCount int
		maxRetries int
	}

	var stale []staleTask
	for rows.Next() {
		var st staleTask
		if err := rows.Scan(&st.id, &st.retryCount, &st.maxRetries); err == nil {
			stale = append(stale, st)
		}
	}

	recoveredCount := 0
	now := time.Now().UTC()
	for _, st := range stale {
		if st.retryCount < st.maxRetries {
			_, err := d.db.ExecContext(ctx, `
			UPDATE tasks
			SET status = 'PENDING', worker_id = NULL, retry_count = retry_count + 1, status_message = 'Worker timed out; task requeued', updated_at = ?
			WHERE id = ? AND status = 'RUNNING'
			`, now, st.id)
			if err == nil {
				recoveredCount++
			}
		} else {
			_, _ = d.db.ExecContext(ctx, `
			UPDATE tasks
			SET status = 'FAILED', status_message = 'Worker timed out; max retries exceeded', updated_at = ?
			WHERE id = ? AND status = 'RUNNING'
			`, now, st.id)
		}
	}

	return recoveredCount, nil
}

// GetStats returns aggregated counts of tasks and active workers.
func (d *DB) GetStats(ctx context.Context) (Stats, error) {
	var stats Stats

	query := `
	SELECT
		COALESCE(SUM(CASE WHEN status = 'PENDING' THEN 1 ELSE 0 END), 0) AS pending_tasks,
		COALESCE(SUM(CASE WHEN status = 'RUNNING' THEN 1 ELSE 0 END), 0) AS running_tasks,
		COALESCE(SUM(CASE WHEN status = 'COMPLETED' THEN 1 ELSE 0 END), 0) AS completed_tasks,
		COALESCE(SUM(CASE WHEN status = 'FAILED' THEN 1 ELSE 0 END), 0) AS failed_tasks,
		COALESCE(SUM(CASE WHEN status = 'CANCELLED' THEN 1 ELSE 0 END), 0) AS cancelled_tasks,
		COUNT(id) AS total_tasks
	FROM tasks
	`
	row := d.db.QueryRowContext(ctx, query)
	if err := row.Scan(&stats.PendingTasks, &stats.RunningTasks, &stats.CompletedTasks, &stats.FailedTasks, &stats.CancelledTasks, &stats.TotalTasks); err != nil {
		return stats, err
	}

	// Active workers (seen in last 60s)
	threshold := time.Now().UTC().Add(-60 * time.Second)
	wRow := d.db.QueryRowContext(ctx, "SELECT COUNT(id) FROM workers WHERE status = 'ACTIVE' AND last_seen >= ?", threshold)
	_ = wRow.Scan(&stats.ActiveWorkers)

	return stats, nil
}

// RetryFailedTasks resets all FAILED tasks back to PENDING.
func (d *DB) RetryFailedTasks(ctx context.Context) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now().UTC()
	query := `
	UPDATE tasks
	SET status = 'PENDING', retry_count = 0, status_message = '', worker_id = NULL, progress = 0.0, speed = '', updated_at = ?
	WHERE status = 'FAILED'
	`
	res, err := d.db.ExecContext(ctx, query, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
