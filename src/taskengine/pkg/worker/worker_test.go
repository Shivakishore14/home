package worker

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"taskengine/pkg/config"
	"taskengine/pkg/db"
	"taskengine/pkg/models"
	"taskengine/pkg/plugin"
	_ "taskengine/pkg/plugins/runner"
	"taskengine/pkg/server"
)

func TestWorkerEndToEndFlow(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "taskengine_worker_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	configYAML := `
server:
  port: 8080
  db_path: "test.db"
  heartbeat_timeout_seconds: 5
  stale_task_check_interval_seconds: 2

defaults:
  max_concurrent_tasks: 2
`
	if err := os.WriteFile(filepath.Join(tmpDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		t.Fatalf("failed to write config.yaml: %v", err)
	}

	cfgMgr, err := config.NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	database, err := db.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("db.Open failed: %v", err)
	}
	defer database.Close()

	srv := server.NewServer(cfgMgr, database, plugin.DefaultRegistry)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	// Create a task on the server
	task, err := database.CreateTask(context.Background(), models.CreateTaskRequest{
		PluginName: "command-runner",
		Params:     json.RawMessage(`{"command": "echo 'Hello from worker!'; echo 'PROGRESS: 100%'"}`),
	})
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}

	// Create and start worker
	w := NewWorker(Config{
		ServerURL:    httpServer.URL,
		WorkerID:     "test-worker-1",
		Hostname:     "test-host",
		PollInterval: 100 * time.Millisecond,
		Concurrency:  1,
	}, plugin.DefaultRegistry)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := w.Start(ctx); err != nil {
		t.Fatalf("w.Start failed: %v", err)
	}
	defer w.Stop()

	// Wait for task to be processed
	var finalTask *models.Task
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		finalTask, _ = database.GetTask(context.Background(), task.ID)
		if finalTask != nil && finalTask.Status == models.StatusCompleted {
			break
		}
	}

	if finalTask == nil || finalTask.Status != models.StatusCompleted {
		t.Fatalf("expected task status COMPLETED, got %+v", finalTask)
	}
	if finalTask.WorkerID != "test-worker-1" {
		t.Errorf("expected workerID 'test-worker-1', got %s", finalTask.WorkerID)
	}
}

func TestWorkerPathTranslation(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/Users/test"
	}

	w := &Worker{
		effectiveConfig: models.RegisterWorkerResponse{
			PathMappings: map[string]string{
				"/Volumes/drive001/media": "~/Desktop/macmini_media",
			},
		},
	}

	translated := w.translatePath("/Volumes/drive001/media/Movies/avatar.mkv")
	expected := filepath.Join(home, "Desktop/macmini_media/Movies/avatar.mkv")

	if translated != expected {
		t.Errorf("expected translated path %q, got %q", expected, translated)
	}
}
