package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"taskengine/pkg/models"
	"taskengine/pkg/plugin"
)

// Config configures worker daemon operation.
type Config struct {
	ServerURL    string
	WorkerID     string
	Hostname     string
	PollInterval time.Duration
	Concurrency  int
}

// Worker handles task polling, execution, and heartbeats.
type Worker struct {
	cfg             Config
	client          *http.Client
	registry        *plugin.Registry
	effectiveConfig models.RegisterWorkerResponse
	stopChan        chan struct{}
	wg              sync.WaitGroup
	semaphore       chan struct{}
}

// NewWorker initializes a worker instance.
func NewWorker(cfg Config, registry *plugin.Registry) *Worker {
	if registry == nil {
		registry = plugin.DefaultRegistry
	}
	if cfg.Hostname == "" {
		h, _ := os.Hostname()
		cfg.Hostname = h
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = cfg.Hostname
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}

	return &Worker{
		cfg:      cfg,
		client:   &http.Client{Timeout: 30 * time.Second},
		registry: registry,
		stopChan: make(chan struct{}),
	}
}

// Start begins worker registration, heartbeat, and polling loops.
func (w *Worker) Start(ctx context.Context) error {
	// Initialize plugins with empty config first for defaults
	for _, pName := range w.registry.EnabledWorkerPlugins() {
		if p, ok := w.registry.GetWorkerPlugin(pName); ok {
			_ = p.Init(ctx, nil)
		}
	}

	if err := w.register(ctx); err != nil {
		return fmt.Errorf("worker registration failed: %w", err)
	}

	concurrency := w.effectiveConfig.MaxConcurrentTasks
	if w.cfg.Concurrency > 0 {
		concurrency = w.cfg.Concurrency
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	w.semaphore = make(chan struct{}, concurrency)

	log.Printf("[Worker %s] Successfully registered at %s. Concurrency: %d. Enabled plugins: %v",
		w.cfg.WorkerID, w.cfg.ServerURL, concurrency, w.registry.EnabledWorkerPlugins())

	// Start Heartbeat loop
	hbInterval := w.effectiveConfig.HeartbeatInterval
	if hbInterval <= 0 {
		hbInterval = 10
	}
	w.wg.Add(1)
	go w.heartbeatLoop(ctx, time.Duration(hbInterval)*time.Second)

	// Start Polling loop
	w.wg.Add(1)
	go w.pollLoop(ctx)

	return nil
}

// Stop gracefully stops worker routines.
func (w *Worker) Stop() {
	close(w.stopChan)
	w.wg.Wait()
}

func (w *Worker) register(ctx context.Context) error {
	plugins := w.registry.EnabledWorkerPlugins()
	reqBody := models.RegisterWorkerRequest{
		WorkerID:       w.cfg.WorkerID,
		Hostname:       w.cfg.Hostname,
		EnabledPlugins: plugins,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/api/v1/workers/register", w.cfg.ServerURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("register returned status: %d", resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(&w.effectiveConfig); err != nil {
		return fmt.Errorf("failed to decode register response: %w", err)
	}

	// Initialize plugins with server-provided worker overrides
	for pName, pCfg := range w.effectiveConfig.PluginConfigs {
		if p, ok := w.registry.GetWorkerPlugin(pName); ok {
			cfgBytes, _ := json.Marshal(pCfg)
			_ = p.Init(ctx, cfgBytes)
		}
	}

	return nil
}

func (w *Worker) heartbeatLoop(ctx context.Context, interval time.Duration) {
	defer w.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	url := fmt.Sprintf("%s/api/v1/workers/%s/heartbeat", w.cfg.ServerURL, w.cfg.WorkerID)

	for {
		select {
		case <-w.stopChan:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
			if err == nil {
				resp, err := w.client.Do(req)
				if err == nil {
					resp.Body.Close()
				}
			}
		}
	}
}

func (w *Worker) pollLoop(ctx context.Context) {
	defer w.wg.Done()
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopChan:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Check if we have capacity in semaphore
			select {
			case w.semaphore <- struct{}{}:
				// Capacity available, try claiming task
				task, err := w.claimTask(ctx)
				if err != nil || task == nil {
					<-w.semaphore // release slot
					continue
				}

				// Spawn task runner goroutine
				w.wg.Add(1)
				go func(t *models.Task) {
					defer w.wg.Done()
					defer func() { <-w.semaphore }()
					w.runTask(ctx, t)
				}(task)

			default:
				// Worker busy at max concurrency; skip poll tick
			}
		}
	}
}

func (w *Worker) claimTask(ctx context.Context) (*models.Task, error) {
	plugins := w.registry.EnabledWorkerPlugins()
	if len(plugins) == 0 {
		return nil, nil
	}

	reqBody := models.ClaimTaskRequest{
		WorkerID: w.cfg.WorkerID,
		Plugins:  plugins,
	}

	data, _ := json.Marshal(reqBody)
	url := fmt.Sprintf("%s/api/v1/tasks/claim", w.cfg.ServerURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claim returned status %d", resp.StatusCode)
	}

	var task models.Task
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		return nil, err
	}
	return &task, nil
}

func (w *Worker) runTask(ctx context.Context, task *models.Task) {
	log.Printf("[Worker %s] Running task %s (Plugin: %s, Target: %s)", w.cfg.WorkerID, task.ID, task.PluginName, task.TargetFile)

	p, ok := w.registry.GetWorkerPlugin(task.PluginName)
	if !ok {
		w.failTask(ctx, task.ID, fmt.Sprintf("Worker does not have plugin %s installed", task.PluginName), false)
		return
	}

	reporter := &httpProgressReporter{
		serverURL: w.cfg.ServerURL,
		taskID:    task.ID,
		client:    w.client,
	}

	// Export runtime variables for plugins and scripts
	os.Setenv("SERVER_URL", w.cfg.ServerURL)
	os.Setenv("WORKER_ID", w.cfg.WorkerID)

	payload := plugin.TaskPayload{
		ID:         task.ID,
		PluginName: task.PluginName,
		TargetFile: task.TargetFile,
		Params:     task.Payload,
	}

	err := p.Execute(ctx, payload, reporter)
	if err != nil {
		log.Printf("[Worker %s] Task %s failed: %v", w.cfg.WorkerID, task.ID, err)
		w.failTask(ctx, task.ID, err.Error(), true)
	} else {
		log.Printf("[Worker %s] Task %s completed successfully", w.cfg.WorkerID, task.ID)
		w.completeTask(ctx, task.ID, "Task completed successfully")
	}
}

func (w *Worker) completeTask(ctx context.Context, taskID, message string) {
	url := fmt.Sprintf("%s/api/v1/tasks/%s/complete", w.cfg.ServerURL, taskID)
	reqBody := models.CompleteTaskRequest{Message: message}
	data, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(data))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		resp, err := w.client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}
}

func (w *Worker) failTask(ctx context.Context, taskID, errMsg string, canRetry bool) {
	url := fmt.Sprintf("%s/api/v1/tasks/%s/fail", w.cfg.ServerURL, taskID)
	reqBody := models.FailTaskRequest{ErrorMessage: errMsg, CanRetry: canRetry}
	data, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(data))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		resp, err := w.client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}
}

// httpProgressReporter streams progress and logs over HTTP to server.
type httpProgressReporter struct {
	serverURL string
	taskID    string
	client    *http.Client
	mu        sync.Mutex
	lastProg  time.Time
}

func (r *httpProgressReporter) Report(ctx context.Context, report plugin.ProgressReport) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// If log chunk present, send log
	if report.LogChunk != "" {
		url := fmt.Sprintf("%s/api/v1/tasks/%s/logs", r.serverURL, r.taskID)
		data, _ := json.Marshal(models.LogChunkRequest{LogChunk: report.LogChunk})
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(data))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			if resp, err := r.client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}

	// If progress changed or throttle elapsed, send progress update
	if report.Progress > 0 || report.Message != "" {
		if time.Since(r.lastProg) > 250*time.Millisecond || report.Progress >= 100.0 {
			r.lastProg = time.Now()
			url := fmt.Sprintf("%s/api/v1/tasks/%s/progress", r.serverURL, r.taskID)
			data, _ := json.Marshal(models.ProgressRequest{
				Progress: report.Progress,
				Speed:    report.Speed,
				Message:  report.Message,
			})
			req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(data))
			if err == nil {
				req.Header.Set("Content-Type", "application/json")
				if resp, err := r.client.Do(req); err == nil {
					resp.Body.Close()
				}
			}
		}
	}

	return nil
}
