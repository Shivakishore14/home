package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"taskengine/pkg/config"
	"taskengine/pkg/db"
	"taskengine/pkg/models"
)

func setupTestServer(t *testing.T) (*Server, *db.DB, *config.Manager, func()) {
	tmpDir, err := os.MkdirTemp("", "taskengine_server_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	configYAML := `
server:
  port: 8080
  db_path: "test.db"
  heartbeat_timeout_seconds: 2
  stale_task_check_interval_seconds: 1

defaults:
  max_concurrent_tasks: 2
  path_mappings:
    "/Volumes/drive001/media": "/Users/air/media"
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

	srv := NewServer(cfgMgr, database, nil)

	cleanup := func() {
		database.Close()
		os.RemoveAll(tmpDir)
	}

	return srv, database, cfgMgr, cleanup
}

func TestServerAPIE2E(t *testing.T) {
	srv, _, _, cleanup := setupTestServer(t)
	defer cleanup()

	handler := srv.Handler()

	// 1. Register Worker
	regPayload := `{"worker_id": "laptop1", "hostname": "macbook-air", "enabled_plugins": ["video-transcoder", "command-runner"]}`
	req := httptest.NewRequest("POST", "/api/v1/workers/register", bytes.NewBufferString(regPayload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for register, got %d: %s", w.Code, w.Body.String())
	}

	var regResp models.RegisterWorkerResponse
	if err := json.NewDecoder(w.Body).Decode(&regResp); err != nil {
		t.Fatalf("failed to decode register response: %v", err)
	}
	if regResp.WorkerID != "laptop1" || regResp.MaxConcurrentTasks != 2 {
		t.Errorf("unexpected register response: %+v", regResp)
	}

	// 2. Create Task
	taskPayload := `{"plugin_name": "video-transcoder", "target_file": "/Volumes/drive001/media/movie.mkv", "priority": 10, "params": {"crf": 23}}`
	req = httptest.NewRequest("POST", "/api/v1/tasks", bytes.NewBufferString(taskPayload))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created for task, got %d: %s", w.Code, w.Body.String())
	}

	var createdTask models.Task
	if err := json.NewDecoder(w.Body).Decode(&createdTask); err != nil {
		t.Fatalf("failed to decode created task: %v", err)
	}

	// 3. Claim Task with path translation check
	claimPayload := `{"worker_id": "laptop1", "plugins": ["video-transcoder"]}`
	req = httptest.NewRequest("POST", "/api/v1/tasks/claim", bytes.NewBufferString(claimPayload))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for claim, got %d: %s", w.Code, w.Body.String())
	}

	var claimedTask models.Task
	if err := json.NewDecoder(w.Body).Decode(&claimedTask); err != nil {
		t.Fatalf("failed to decode claimed task: %v", err)
	}
	if claimedTask.ID != createdTask.ID {
		t.Errorf("expected claimed ID %s, got %s", createdTask.ID, claimedTask.ID)
	}
	// Path should be translated to /Users/air/media/movie.mkv
	expectedTarget := "/Users/air/media/movie.mkv"
	if claimedTask.TargetFile != expectedTarget {
		t.Errorf("expected translated target_file %q, got %q", expectedTarget, claimedTask.TargetFile)
	}

	// 4. Update Progress
	progPayload := `{"progress": 65.5, "speed": "2.1x", "message": "Encoding..."}`
	req = httptest.NewRequest("POST", "/api/v1/tasks/"+claimedTask.ID+"/progress", bytes.NewBufferString(progPayload))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for progress, got %d", w.Code)
	}

	// 5. Append Log Chunk
	logPayload := `{"log_chunk": "frame 500\n"}`
	req = httptest.NewRequest("POST", "/api/v1/tasks/"+claimedTask.ID+"/logs", bytes.NewBufferString(logPayload))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for logs, got %d", w.Code)
	}

	// 6. Complete Task
	compPayload := `{"message": "Transcoded in 12s"}`
	req = httptest.NewRequest("POST", "/api/v1/tasks/"+claimedTask.ID+"/complete", bytes.NewBufferString(compPayload))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for complete, got %d", w.Code)
	}

	// 7. Get Task Status
	req = httptest.NewRequest("GET", "/api/v1/tasks/"+claimedTask.ID, nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for get task, got %d", w.Code)
	}

	var finalTask models.Task
	json.NewDecoder(w.Body).Decode(&finalTask)
	if finalTask.Status != models.StatusCompleted || finalTask.Progress != 100.0 || finalTask.LogOutput != "frame 500\n" {
		t.Errorf("unexpected final task state: %+v", finalTask)
	}

	// 8. Stats
	req = httptest.NewRequest("GET", "/api/v1/stats", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var stats models.SSEEvent // temporary container or map
	var statsMap map[string]interface{}
	json.NewDecoder(w.Body).Decode(&statsMap)
	_ = stats
	if statsMap["completed_tasks"].(float64) != 1 {
		t.Errorf("expected 1 completed task in stats, got %+v", statsMap)
	}

	// 9. Config Reload
	req = httptest.NewRequest("POST", "/api/v1/config/reload", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for reload, got %d", w.Code)
	}
}
