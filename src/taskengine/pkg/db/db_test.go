package db

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"taskengine/pkg/models"
)

func TestDBOperations(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "taskengine_db_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open db failed: %v", err)
	}
	defer database.Close()

	ctx := context.Background()

	// 1. Test Task Creation
	task1, err := database.CreateTask(ctx, models.CreateTaskRequest{
		PluginName: "video-transcoder",
		TargetFile: "/media/movie.mkv",
		Priority:   10,
		MaxRetries: 2,
		Params:     json.RawMessage(`{"crf": 23}`),
	})
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}
	if task1.Status != models.StatusPending {
		t.Errorf("expected status PENDING, got %s", task1.Status)
	}

	task2, err := database.CreateTask(ctx, models.CreateTaskRequest{
		PluginName: "command-runner",
		Priority:   5,
		MaxRetries: 1,
		Params:     json.RawMessage(`{"command": "echo hello"}`),
	})
	if err != nil {
		t.Fatalf("CreateTask 2 failed: %v", err)
	}

	// 2. Test GetTask
	fetched, err := database.GetTask(ctx, task1.ID)
	if err != nil || fetched == nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if fetched.Priority != 10 || fetched.TargetFile != "/media/movie.mkv" {
		t.Errorf("mismatched fetched task: %+v", fetched)
	}

	// 3. Test Worker Registration
	err = database.RegisterWorker(ctx, "worker1", "macbook-air", []string{"video-transcoder", "command-runner"})
	if err != nil {
		t.Fatalf("RegisterWorker failed: %v", err)
	}

	workers, err := database.ListWorkers(ctx)
	if err != nil || len(workers) != 1 {
		t.Fatalf("ListWorkers failed: %v, count: %d", err, len(workers))
	}
	if workers[0].ID != "worker1" || len(workers[0].EnabledPlugins) != 2 {
		t.Errorf("unexpected worker data: %+v", workers[0])
	}

	// 4. Test Atomic Claiming
	claimed, err := database.ClaimNextTask(ctx, "worker1", []string{"video-transcoder"})
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextTask failed: %v", err)
	}
	if claimed.ID != task1.ID {
		t.Errorf("expected to claim task1 (higher priority), got %s", claimed.ID)
	}
	if claimed.Status != models.StatusRunning || claimed.WorkerID != "worker1" {
		t.Errorf("expected status RUNNING and worker1, got %+v", claimed)
	}

	// Claiming again with only video-transcoder should yield nil (no pending video tasks)
	claimedAgain, err := database.ClaimNextTask(ctx, "worker1", []string{"video-transcoder"})
	if err != nil {
		t.Fatalf("ClaimNextTask failed: %v", err)
	}
	if claimedAgain != nil {
		t.Errorf("expected nil claim, got %+v", claimedAgain)
	}

	// 5. Test UpdateTaskProgress & AppendTaskLog
	err = database.UpdateTaskProgress(ctx, task1.ID, 45.5, "1.8x", "Encoding frame 1200")
	if err != nil {
		t.Fatalf("UpdateTaskProgress failed: %v", err)
	}

	err = database.AppendTaskLog(ctx, task1.ID, "frame= 1200 fps= 48 q=28.0\n")
	if err != nil {
		t.Fatalf("AppendTaskLog failed: %v", err)
	}

	updated, _ := database.GetTask(ctx, task1.ID)
	if updated.Progress != 45.5 || updated.Speed != "1.8x" || updated.StatusMessage != "Encoding frame 1200" {
		t.Errorf("unexpected updated task: %+v", updated)
	}
	if updated.LogOutput != "frame= 1200 fps= 48 q=28.0\n" {
		t.Errorf("unexpected log output: %s", updated.LogOutput)
	}

	// 6. Test CompleteTask
	err = database.CompleteTask(ctx, task1.ID, "Transcode finished successfully")
	if err != nil {
		t.Fatalf("CompleteTask failed: %v", err)
	}
	completed, _ := database.GetTask(ctx, task1.ID)
	if completed.Status != models.StatusCompleted || completed.Progress != 100.0 {
		t.Errorf("expected status COMPLETED, got %+v", completed)
	}

	// 7. Test FailTask & Retry
	claimed2, err := database.ClaimNextTask(ctx, "worker1", []string{"command-runner"})
	if err != nil || claimed2 == nil {
		t.Fatalf("ClaimNextTask for task2 failed: %v", err)
	}

	// Fail with canRetry=true
	err = database.FailTask(ctx, task2.ID, "Temporary network blip", true)
	if err != nil {
		t.Fatalf("FailTask failed: %v", err)
	}
	retried, _ := database.GetTask(ctx, task2.ID)
	if retried.Status != models.StatusPending || retried.RetryCount != 1 {
		t.Errorf("expected task2 to be reset to PENDING with retryCount=1, got %+v", retried)
	}

	// Claim again and fail beyond maxRetries
	claimed3, _ := database.ClaimNextTask(ctx, "worker1", []string{"command-runner"})
	if claimed3 == nil {
		t.Fatalf("expected to re-claim retried task2")
	}
	err = database.FailTask(ctx, task2.ID, "Permanent error", true)
	if err != nil {
		t.Fatalf("FailTask permanent failed: %v", err)
	}
	failed, _ := database.GetTask(ctx, task2.ID)
	if failed.Status != models.StatusFailed {
		t.Errorf("expected task2 to be FAILED, got %+v", failed)
	}

	// 8. Test Stats
	stats, err := database.GetStats(ctx)
	if err != nil {
		t.Fatalf("GetStats failed: %v", err)
	}
	if stats.CompletedTasks != 1 || stats.FailedTasks != 1 || stats.TotalTasks != 2 {
		t.Errorf("unexpected stats: %+v", stats)
	}
}

func TestConcurrentTaskClaiming(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "taskengine_concurrent_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "concurrent.db")
	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open db failed: %v", err)
	}
	defer database.Close()

	ctx := context.Background()

	// Create 1 single task
	task, err := database.CreateTask(ctx, models.CreateTaskRequest{
		PluginName: "single-job",
		Priority:   1,
	})
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}

	var wg sync.WaitGroup
	claimedCount := 0
	var mu sync.Mutex

	// 10 concurrent workers try to claim the same 1 task
	for i := 0; i < 10; i++ {
		wg.Add(1)
		workerID := filepath.Join("worker", string(rune('A'+i)))
		go func(wID string) {
			defer wg.Done()
			claimed, err := database.ClaimNextTask(ctx, wID, []string{"single-job"})
			if err == nil && claimed != nil {
				mu.Lock()
				claimedCount++
				mu.Unlock()
			}
		}(workerID)
	}

	wg.Wait()

	if claimedCount != 1 {
		t.Fatalf("expected exactly 1 worker to claim task, but %d workers claimed it!", claimedCount)
	}

	claimedTask, _ := database.GetTask(ctx, task.ID)
	if claimedTask.Status != models.StatusRunning {
		t.Errorf("expected task status RUNNING, got %s", claimedTask.Status)
	}
}
