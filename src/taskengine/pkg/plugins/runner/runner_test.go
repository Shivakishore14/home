package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"taskengine/pkg/models"
	"taskengine/pkg/plugin"
)

type mockReporter struct {
	mu       sync.Mutex
	reports  []plugin.ProgressReport
	logLines []string
}

func (m *mockReporter) Report(ctx context.Context, report plugin.ProgressReport) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reports = append(m.reports, report)
	if report.LogChunk != "" {
		m.logLines = append(m.logLines, report.LogChunk)
	}
	return nil
}

func TestCommandRunnerExecution(t *testing.T) {
	p := &CommandRunnerPlugin{}
	_ = p.Init(context.Background(), nil)

	rep := &mockReporter{}

	params := CommandRunnerParams{
		Command: "echo 'Step 1'; echo 'PROGRESS: 50%'; echo 'Step 2'; echo 'PROGRESS: 100%'",
	}
	paramsJSON, _ := json.Marshal(params)

	payload := plugin.TaskPayload{
		ID:         "test-task-1",
		PluginName: "command-runner",
		Params:     paramsJSON,
	}

	err := p.Execute(context.Background(), payload, rep)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	rep.mu.Lock()
	defer rep.mu.Unlock()

	has50 := false
	has100 := false
	for _, r := range rep.reports {
		if r.Progress == 50 {
			has50 = true
		}
		if r.Progress == 100 {
			has100 = true
		}
	}

	if !has50 || !has100 {
		t.Errorf("expected 50%% and 100%% progress reports, got: %+v", rep.reports)
	}
}

func TestCommandRunnerPrerequisites(t *testing.T) {
	p := &CommandRunnerPlugin{}
	_ = p.Init(context.Background(), nil)

	rep := &mockReporter{}

	// 1. Passing Prerequisite check
	paramsPass := CommandRunnerParams{
		Command: "echo 'Executed with satisfied prerequisites!'",
		Prerequisites: &models.TaskPrerequisites{
			CheckCommand: "which sh",
		},
	}
	paramsPassJSON, _ := json.Marshal(paramsPass)

	err := p.Execute(context.Background(), plugin.TaskPayload{
		ID:         "test-prereq-pass",
		PluginName: "command-runner",
		Params:     paramsPassJSON,
	}, rep)
	if err != nil {
		t.Fatalf("expected prerequisite check to pass, got: %v", err)
	}

	// 2. Failing Prerequisite check
	paramsFail := CommandRunnerParams{
		Command: "echo 'Should not run'",
		Prerequisites: &models.TaskPrerequisites{
			CheckCommand: "nonexistent_binary_xyz_12345",
		},
	}
	paramsFailJSON, _ := json.Marshal(paramsFail)

	err = p.Execute(context.Background(), plugin.TaskPayload{
		ID:         "test-prereq-fail",
		PluginName: "command-runner",
		Params:     paramsFailJSON,
	}, rep)
	if err == nil {
		t.Fatalf("expected prerequisite check to fail, but it succeeded")
	}
}

func TestCommandRunnerAssetSync(t *testing.T) {
	// Mock server that serves assets
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/tasks/test-task/assets" {
			assets := models.TaskAssets{
				TaskName: "test-task",
				Version:  "v1-hash",
				Files: []models.TaskAssetItem{
					{Path: "scripts/helper.sh", Checksum: "abc123", Size: 25},
				},
			}
			json.NewEncoder(w).Encode(assets)
			return
		}
		if r.URL.Path == "/api/v1/files/scripts/helper.sh" {
			fmt.Fprintln(w, "echo 'Hello from synced asset helper'")
			return
		}
		http.NotFound(w, r)
	}))
	defer mockServer.Close()

	os.Setenv("SERVER_URL", mockServer.URL)
	defer os.Unsetenv("SERVER_URL")

	p := &CommandRunnerPlugin{}
	_ = p.Init(context.Background(), nil)

	rep := &mockReporter{}

	params := CommandRunnerParams{
		TaskName: "test-task",
		Command:  "sh $TASK_ASSETS_DIR/helper.sh",
	}
	paramsJSON, _ := json.Marshal(params)

	err := p.Execute(context.Background(), plugin.TaskPayload{
		ID:         "test-asset-task",
		PluginName: "command-runner",
		Params:     paramsJSON,
	}, rep)
	if err != nil {
		t.Fatalf("Execute with synced assets failed: %v", err)
	}
}
