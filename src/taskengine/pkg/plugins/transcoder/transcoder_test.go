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

	// Verify transcoded output file was created
	expectedOutput := filepath.Join(tmpDir, "sample.transcoded.mp4")
	if _, err := os.Stat(expectedOutput); os.IsNotExist(err) {
		t.Fatalf("expected output file %s does not exist", expectedOutput)
	}

	// Verify server plugin does not generate task again for the already transcoded file
	tasksAfter, err := srvPlugin.GenerateTasks(context.Background())
	if err != nil || len(tasksAfter) != 0 {
		t.Errorf("expected 0 tasks after transcode, got %d", len(tasksAfter))
	}
}
