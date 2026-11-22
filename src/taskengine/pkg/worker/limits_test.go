package worker

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"taskengine/pkg/config"
	"taskengine/pkg/db"
	"taskengine/pkg/models"
	"taskengine/pkg/plugin"
	"taskengine/pkg/server"
)

// gatePlugin blocks each task until released and records peak concurrency.
type gatePlugin struct {
	name    string
	release chan struct{}
	mu      sync.Mutex
	cur     int
	peak    int
}

func (g *gatePlugin) Name() string                                { return g.name }
func (g *gatePlugin) Init(context.Context, json.RawMessage) error { return nil }
func (g *gatePlugin) Execute(ctx context.Context, _ plugin.TaskPayload, _ plugin.ProgressReporter) error {
	g.mu.Lock()
	g.cur++
	if g.cur > g.peak {
		g.peak = g.cur
	}
	g.mu.Unlock()
	defer func() { g.mu.Lock(); g.cur--; g.mu.Unlock() }()
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	return nil
}
func (g *gatePlugin) stats() (cur, peak int) { g.mu.Lock(); defer g.mu.Unlock(); return g.cur, g.peak }

func TestPluginLimits(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "workers"), 0755))
	must(os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("server: {heartbeat_timeout_seconds: 30}\n"), 0644))
	must(os.WriteFile(filepath.Join(dir, "workers", "w.yaml"), []byte(`
worker_id: w
max_concurrent_tasks: 3
enabled_plugins: [limited, free]
plugin_limits:
  limited: 1
`), 0644))
	cfg, err := config.NewManager(dir)
	must(err)
	database, err := db.Open(filepath.Join(dir, "t.db"))
	must(err)
	defer database.Close()

	limited := &gatePlugin{name: "limited", release: make(chan struct{})}
	free := &gatePlugin{name: "free", release: make(chan struct{})}
	reg := plugin.NewRegistry()
	reg.RegisterWorkerPlugin(limited)
	reg.RegisterWorkerPlugin(free)

	srv := server.NewServer(cfg, database, reg)
	srv.SetAuth(server.NewAuth(nil, nil))
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := database.CreateTask(ctx, models.CreateTaskRequest{PluginName: "limited", Priority: 10})
		must(err)
	}
	for i := 0; i < 2; i++ {
		_, err := database.CreateTask(ctx, models.CreateTaskRequest{PluginName: "free"})
		must(err)
	}

	w := NewWorker(Config{ServerURL: hs.URL, WorkerID: "w", PollInterval: 20 * time.Millisecond}, reg)
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	must(w.Start(wctx))
	defer w.Stop()

	// The limited tasks have higher priority, yet only one may run; the other
	// two slots go to the free plugin.
	waitFor(t, func() bool { c1, _ := limited.stats(); c2, _ := free.stats(); return c1 == 1 && c2 == 2 })
	time.Sleep(200 * time.Millisecond)
	if c, _ := limited.stats(); c != 1 {
		t.Fatalf("limited running = %d, want 1", c)
	}

	// Release everything progressively; the limit must hold throughout.
	close(free.release)
	close(limited.release)
	waitFor(t, func() bool {
		s, _ := database.GetStats(ctx)
		return s.CompletedTasks == 5
	})
	if _, peak := limited.stats(); peak != 1 {
		t.Errorf("limited peak concurrency = %d, want 1", peak)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
