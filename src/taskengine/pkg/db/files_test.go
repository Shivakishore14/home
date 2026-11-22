package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// An existing database from before the files feature must gain the new
// tasks.output column on open, keeping its rows.
func TestMigrateAddsOutputColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE tasks (
		id TEXT PRIMARY KEY, plugin_name TEXT NOT NULL, status TEXT NOT NULL, payload TEXT NOT NULL,
		target_file TEXT, worker_id TEXT, priority INTEGER DEFAULT 0, progress REAL DEFAULT 0.0,
		speed TEXT DEFAULT '', status_message TEXT DEFAULT '', log_output TEXT DEFAULT '',
		retry_count INTEGER DEFAULT 0, max_retries INTEGER DEFAULT 3, claimed_at DATETIME,
		last_heartbeat DATETIME, created_at DATETIME DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO tasks (id, plugin_name, status, payload) VALUES ('t1', 'x', 'COMPLETED', '{}');`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.GetTask(context.Background(), "t1")
	if err != nil || task == nil {
		t.Fatalf("get old task: %v", err)
	}
	// Re-opening is idempotent.
	d.Close()
	d2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d2.Close()
}
