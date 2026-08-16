package transcoder

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"taskengine/pkg/plugin"
)

type mockReporter struct {
	mu      sync.Mutex
	reports []plugin.ProgressReport
}

func (m *mockReporter) Report(ctx context.Context, report plugin.ProgressReport) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reports = append(m.reports, report)
	return nil
}

func TestTranscoderServerAndWorker(t *testing.T) {
	// Check if ffmpeg is present
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed; skipping live transcoding test")
	}

	tmpDir, err := os.MkdirTemp("", "taskengine_transcode_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create synthetic 1-second video file using ffmpeg
	sampleInput := filepath.Join(tmpDir, "sample.mkv")
	genCmd := exec.Command(ffmpegPath, "-y", "-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=10", "-c:v", "libx264", sampleInput)
	if out, err := genCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to generate sample test video: %v (%s)", err, string(out))
	}

	// 1. Test Server Plugin GenerateTasks
	srvPlugin := &VideoTranscoderServerPlugin{}
	srvConfig := map[string]interface{}{
		"directory":         tmpDir,
		"target_extensions": []string{".mkv"},
		"target_codec":      "libx264",
		"target_height":     1080,
		"crf":               28,
		"preset":            "ultrafast",
		"container":         "mp4",
	}
	srvConfigJSON, _ := json.Marshal(srvConfig)
	if err := srvPlugin.Init(context.Background(), srvConfigJSON); err != nil {
		t.Fatalf("srvPlugin.Init failed: %v", err)
	}

	tasks, err := srvPlugin.GenerateTasks(context.Background())
	if err != nil {
		t.Fatalf("GenerateTasks failed: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task generated, got %d", len(tasks))
	}
	if tasks[0].TargetFile != sampleInput {
		t.Errorf("expected target file %s, got %s", sampleInput, tasks[0].TargetFile)
	}

	// 2. Test Worker Plugin Execute
	workerPlugin := &VideoTranscoderWorkerPlugin{}
	if err := workerPlugin.Init(context.Background(), nil); err != nil {
		t.Fatalf("workerPlugin.Init failed: %v", err)
	}

	rep := &mockReporter{}
	err = workerPlugin.Execute(context.Background(), tasks[0], rep)
	if err != nil {
		t.Fatalf("workerPlugin.Execute failed: %v", err)
	}

	// Verify clean output file was created (sample.mp4) and original sample.mkv removed
	expectedCleanOutput := filepath.Join(tmpDir, "sample.mp4")
	if _, err := os.Stat(expectedCleanOutput); os.IsNotExist(err) {
		t.Fatalf("expected clean output file %s does not exist", expectedCleanOutput)
	}
	if _, err := os.Stat(sampleInput); !os.IsNotExist(err) {
		t.Errorf("expected original sample.mkv to be removed after successful clean transcode")
	}

	// Verify .transcode_cache exists and contains sample.mp4
	cache := readTranscodeCache(tmpDir)
	if !cache["sample.mp4"] {
		t.Errorf("expected sample.mp4 in .transcode_cache, got %+v", cache)
	}

	// Verify server plugin skips sample.mp4 on subsequent scan
	tasksAfter, err := srvPlugin.GenerateTasks(context.Background())
	if err != nil || len(tasksAfter) != 0 {
		t.Errorf("expected 0 tasks after transcode and cache write, got %d", len(tasksAfter))
	}
}

func TestTranscoderWithScratchDir(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed; skipping test")
	}

	storageDir, err := os.MkdirTemp("", "taskengine_storage_test")
	if err != nil {
		t.Fatalf("failed to create storage dir: %v", err)
	}
	defer os.RemoveAll(storageDir)

	scratchDir, err := os.MkdirTemp("", "taskengine_scratch_test")
	if err != nil {
		t.Fatalf("failed to create scratch dir: %v", err)
	}
	defer os.RemoveAll(scratchDir)

	// Create test video on "remote storage"
	inputVideo := filepath.Join(storageDir, "remote_movie.mkv")
	genCmd := exec.Command(ffmpegPath, "-y", "-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=10", "-c:v", "libx264", inputVideo)
	if out, err := genCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to generate test video: %v (%s)", err, string(out))
	}

	workerPlugin := &VideoTranscoderWorkerPlugin{}
	_ = workerPlugin.Init(context.Background(), nil)

	params := map[string]interface{}{
		"target_codec":  "libx264",
		"target_height": 1080,
		"crf":           28,
		"preset":        "ultrafast",
		"container":     "mp4",
		"scratch_dir":   scratchDir,
	}
	paramsJSON, _ := json.Marshal(params)

	payload := plugin.TaskPayload{
		ID:         "test-scratch-task-1",
		PluginName: "video-transcoder",
		TargetFile: inputVideo,
		Params:     paramsJSON,
	}

	rep := &mockReporter{}
	err = workerPlugin.Execute(context.Background(), payload, rep)
	if err != nil {
		t.Fatalf("workerPlugin.Execute with scratch failed: %v", err)
	}

	// Verify clean final output exists on storage
	expectedFinal := filepath.Join(storageDir, "remote_movie.mp4")
	if _, err := os.Stat(expectedFinal); os.IsNotExist(err) {
		t.Fatalf("expected final output on storage %s does not exist", expectedFinal)
	}
	if _, err := os.Stat(inputVideo); !os.IsNotExist(err) {
		t.Errorf("expected original remote_movie.mkv to be removed from storage")
	}

	// Verify .transcode_cache on storage
	cache := readTranscodeCache(storageDir)
	if !cache["remote_movie.mp4"] {
		t.Errorf("expected remote_movie.mp4 in storage .transcode_cache, got %+v", cache)
	}
}
