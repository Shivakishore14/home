package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigManagerAndPathTranslation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "taskengine_config_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create config.yaml
	configYAML := `
server:
  port: 9090
  db_path: "test.db"
  heartbeat_timeout_seconds: 45
  stale_task_check_interval_seconds: 15

defaults:
  max_concurrent_tasks: 1
  path_mappings:
    "/Volumes/drive001/media": "/mnt/media"
  plugin_configs:
    video-transcoder:
      preset: "fast"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		t.Fatalf("failed to write config.yaml: %v", err)
	}

	// Create workers/laptop1.yaml
	if err := os.MkdirAll(filepath.Join(tmpDir, "workers"), 0755); err != nil {
		t.Fatalf("failed to create workers dir: %v", err)
	}
	workerYAML := `
worker_id: "laptop1"
max_concurrent_tasks: 3
path_mappings:
  "/Volumes/drive001/media": "/Users/air/Desktop/media"
plugin_configs:
  video-transcoder:
    preset: "ultrafast"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "workers", "laptop1.yaml"), []byte(workerYAML), 0644); err != nil {
		t.Fatalf("failed to write worker yaml: %v", err)
	}

	// Create helper script asset
	if err := os.MkdirAll(filepath.Join(tmpDir, "scripts"), 0755); err != nil {
		t.Fatalf("failed to create scripts dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "scripts", "helper.py"), []byte("print('hello')"), 0644); err != nil {
		t.Fatalf("failed to write helper.py: %v", err)
	}

	// Create definitions/test_job.yaml
	if err := os.MkdirAll(filepath.Join(tmpDir, "definitions"), 0755); err != nil {
		t.Fatalf("failed to create defs dir: %v", err)
	}
	defYAML := `
name: "sample-job"
plugin_name: "command-runner"
schedule: "* * * * *"
enabled: true
priority: 10
prerequisites:
  check_command: "which python3"
assets:
  files:
    - "scripts/helper.py"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "definitions", "test_job.yaml"), []byte(defYAML), 0644); err != nil {
		t.Fatalf("failed to write def yaml: %v", err)
	}

	// Test NewManager
	mgr, err := NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	global := mgr.GetGlobal()
	if global.Server.Port != 9090 {
		t.Errorf("expected port 9090, got %d", global.Server.Port)
	}

	defs := mgr.GetDefinitions()
	if len(defs) != 1 || defs[0].Name != "sample-job" {
		t.Errorf("expected 1 def named sample-job, got %v", defs)
	}
	if defs[0].Prerequisites.CheckCommand != "which python3" {
		t.Errorf("expected check_command 'which python3', got %s", defs[0].Prerequisites.CheckCommand)
	}

	// Test GetTaskAssets
	assets, err := mgr.GetTaskAssets("sample-job")
	if err != nil {
		t.Fatalf("GetTaskAssets failed: %v", err)
	}
	if len(assets.Files) != 1 || assets.Files[0].Path != "scripts/helper.py" {
		t.Errorf("expected 1 asset file scripts/helper.py, got %+v", assets)
	}
	if assets.Version == "" {
		t.Errorf("expected non-empty asset version")
	}

	// Test Worker Override
	wCfg := mgr.GetWorkerConfig("laptop1")
	if wCfg.MaxConcurrentTasks != 3 {
		t.Errorf("expected max concurrent tasks 3, got %d", wCfg.MaxConcurrentTasks)
	}
	if wCfg.PluginConfigs["video-transcoder"].(map[string]interface{})["preset"] != "ultrafast" {
		t.Errorf("expected preset ultrafast, got %v", wCfg.PluginConfigs)
	}

	// Test Default worker fallback
	defaultWorker := mgr.GetWorkerConfig("unknown_worker")
	if defaultWorker.MaxConcurrentTasks != 1 {
		t.Errorf("expected default max concurrent tasks 1, got %d", defaultWorker.MaxConcurrentTasks)
	}

	// Test Path Translation for laptop1
	translated := mgr.TranslatePath("laptop1", "/Volumes/drive001/media/movies/test.mkv")
	expected := "/Users/air/Desktop/media/movies/test.mkv"
	if translated != expected {
		t.Errorf("expected path %q, got %q", expected, translated)
	}

	// Test Path Translation for unknown_worker (should use global default)
	translatedDefault := mgr.TranslatePath("unknown_worker", "/Volumes/drive001/media/movies/test.mkv")
	expectedDefault := "/mnt/media/movies/test.mkv"
	if translatedDefault != expectedDefault {
		t.Errorf("expected path %q, got %q", expectedDefault, translatedDefault)
	}
}
