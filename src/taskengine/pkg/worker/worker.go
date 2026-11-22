package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"taskengine/pkg/apiclient"
	"taskengine/pkg/models"
	"taskengine/pkg/plugin"
)

// Config configures worker daemon operation.
type Config struct {
	ServerURL      string
	WorkerID       string
	Hostname       string
	PollInterval   time.Duration
	Concurrency    int
	EnabledPlugins []string
	// Token is the bearer token sent to the server (defaults to $TASKENGINE_TOKEN).
	Token string
}

// Worker handles task polling, execution, and heartbeats.
type Worker struct {
	cfg             Config
	client          *http.Client
	transferClient  *http.Client // no overall timeout; for file uploads/downloads
	registry        *plugin.Registry
	enabledPlugins  []string
	effectiveConfig models.RegisterWorkerResponse
	stopChan        chan struct{}
	wg              sync.WaitGroup
	semaphore       chan struct{}

	runningMu sync.Mutex
	running   map[string]int // in-flight tasks per plugin
}

// NewWorker initializes a worker instance.
func NewWorker(cfg Config, registry *plugin.Registry) *Worker {
	if registry == nil {
		registry = plugin.DefaultRegistry
	}
	cfg.ServerURL = strings.TrimRight(cfg.ServerURL, "/")
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

	enabledPlugins := cfg.EnabledPlugins
	if len(enabledPlugins) == 0 {
		// Opt-in plugins (e.g. comfyui) only run when explicitly enabled.
		enabledPlugins = registry.DefaultWorkerPlugins()
	}

	if cfg.Token == "" {
		cfg.Token = apiclient.TokenFromEnv()
	}

	return &Worker{
		cfg:            cfg,
		client:         &http.Client{Timeout: 30 * time.Second, Transport: apiclient.Transport(cfg.ServerURL, cfg.Token, nil)},
		transferClient: &http.Client{Transport: apiclient.Transport(cfg.ServerURL, cfg.Token, nil)},
		registry:       registry,
		enabledPlugins: enabledPlugins,
		stopChan:       make(chan struct{}),
		running:        make(map[string]int),
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
		w.cfg.WorkerID, w.cfg.ServerURL, concurrency, w.enabledPlugins)

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
	reqBody := models.RegisterWorkerRequest{
		WorkerID:       w.cfg.WorkerID,
		Hostname:       w.cfg.Hostname,
		EnabledPlugins: w.enabledPlugins,
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

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("register rejected (401): set a valid worker token via --token or $%s", apiclient.TokenEnv)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("register returned status: %d", resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(&w.effectiveConfig); err != nil {
		return fmt.Errorf("failed to decode register response: %w", err)
	}

	if len(w.effectiveConfig.EnabledPlugins) > 0 {
		w.enabledPlugins = w.effectiveConfig.EnabledPlugins
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
				w.trackStart(task.PluginName)
				w.wg.Add(1)
				go func(t *models.Task) {
					defer w.wg.Done()
					defer func() { <-w.semaphore }()
					defer w.trackDone(t.PluginName)
					w.runTask(ctx, t)
				}(task)

			default:
				// Worker busy at max concurrency; skip poll tick
			}
		}
	}
}

func (w *Worker) claimTask(ctx context.Context) (*models.Task, error) {
	plugins := w.claimablePlugins()
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

func resolveHomePath(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~/") || p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			trimmed := strings.TrimPrefix(p, "~")
			trimmed = strings.TrimPrefix(trimmed, "/")
			return filepath.Join(home, trimmed)
		}
	}
	return p
}

func (w *Worker) translatePath(serverPath string) string {
	if serverPath == "" {
		return ""
	}

	var bestMatchKey string
	var bestMatchVal string

	for serverPrefix, workerPrefix := range w.effectiveConfig.PathMappings {
		cleanedServerPrefix := filepath.Clean(serverPrefix)
		cleanedServerPath := filepath.Clean(serverPath)

		if strings.HasPrefix(cleanedServerPath, cleanedServerPrefix) {
			if len(cleanedServerPrefix) > len(bestMatchKey) {
				bestMatchKey = cleanedServerPrefix
				bestMatchVal = workerPrefix
			}
		}
	}

	if bestMatchKey != "" {
		cleanedServerPath := filepath.Clean(serverPath)
		relPath := strings.TrimPrefix(cleanedServerPath, bestMatchKey)
		relPath = strings.TrimPrefix(relPath, string(filepath.Separator))
		relPath = strings.TrimPrefix(relPath, "/")

		resolvedWorkerPrefix := resolveHomePath(bestMatchVal)
		return filepath.Join(resolvedWorkerPrefix, relPath)
	}

	return resolveHomePath(serverPath)
}

func (w *Worker) runTask(ctx context.Context, task *models.Task) {
	localTargetFile := w.translatePath(task.TargetFile)
	if localTargetFile != task.TargetFile && task.TargetFile != "" {
		log.Printf("[Worker %s] Running task %s (Plugin: %s, Target: %s -> %s)", w.cfg.WorkerID, task.ID, task.PluginName, task.TargetFile, localTargetFile)
	} else {
		log.Printf("[Worker %s] Running task %s (Plugin: %s, Target: %s)", w.cfg.WorkerID, task.ID, task.PluginName, task.TargetFile)
	}

	p, ok := w.registry.GetWorkerPlugin(task.PluginName)
	if !ok {
		w.failTask(ctx, task.ID, fmt.Sprintf("Worker does not have plugin %s installed", task.PluginName), false)
		return
	}

	reporter := &httpProgressReporter{
		serverURL:      w.cfg.ServerURL,
		workerID:       w.cfg.WorkerID,
		taskID:         task.ID,
		client:         w.client,
		transferClient: w.transferClient,
	}

	// Export runtime variables for plugins and scripts
	os.Setenv("SERVER_URL", w.cfg.ServerURL)
	os.Setenv("WORKER_ID", w.cfg.WorkerID)
	if w.cfg.Token != "" {
		os.Setenv(apiclient.TokenEnv, w.cfg.Token)
	}
	if w.effectiveConfig.ScratchDir != "" {
		os.Setenv("SCRATCH_DIR", w.effectiveConfig.ScratchDir)
	}

	payload := plugin.TaskPayload{
		ID:         task.ID,
		PluginName: task.PluginName,
		TargetFile: localTargetFile,
		Params:     task.Payload,
	}

	err := p.Execute(ctx, payload, reporter)
	var skipErr *plugin.SkipError
	var permErr *plugin.PermanentError
	switch {
	case errors.As(err, &skipErr):
		log.Printf("[Worker %s] Task %s skipped: %s", w.cfg.WorkerID, task.ID, skipErr.Reason)
		w.completeTask(ctx, task.ID, "Skipped: "+skipErr.Reason, reporter)
	case errors.As(err, &permErr):
		log.Printf("[Worker %s] Task %s failed permanently (no retry): %v", w.cfg.WorkerID, task.ID, err)
		w.failTask(ctx, task.ID, err.Error(), false)
	case err != nil:
		log.Printf("[Worker %s] Task %s failed: %v", w.cfg.WorkerID, task.ID, err)
		w.failTask(ctx, task.ID, err.Error(), true)
	default:
		log.Printf("[Worker %s] Task %s completed successfully", w.cfg.WorkerID, task.ID)
		w.completeTask(ctx, task.ID, "Task completed successfully", reporter)
	}
}

func (w *Worker) completeTask(ctx context.Context, taskID, message string, rep *httpProgressReporter) {
	url := fmt.Sprintf("%s/api/v1/tasks/%s/complete", w.cfg.ServerURL, taskID)
	reqBody := models.CompleteTaskRequest{Message: message}
	if rep != nil {
		reqBody.Files, reqBody.OutputDetails = rep.results()
	}
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

// httpProgressReporter streams progress and logs over HTTP to server and prints terminal progress.
type httpProgressReporter struct {
	serverURL      string
	workerID       string
	taskID         string
	client         *http.Client
	transferClient *http.Client
	mu             sync.Mutex
	lastProg       time.Time
	lastLog        time.Time

	ioMu    sync.Mutex
	fileIDs []string
	output  map[string]interface{}
}

func (r *httpProgressReporter) Report(ctx context.Context, report plugin.ProgressReport) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Terminal progress log (throttled to every 3s or on completion)
	if report.Message != "" && (time.Since(r.lastLog) >= 3*time.Second || report.Progress >= 100.0) {
		r.lastLog = time.Now()
		speedStr := ""
		if report.Speed != "" {
			speedStr = fmt.Sprintf(" [%s]", report.Speed)
		}
		log.Printf("[Worker %s] [%.1f%%%s] %s", r.workerID, report.Progress, speedStr, report.Message)
	}

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

// claimablePlugins returns enabled plugins that are below their per-plugin
// concurrency limit (plugin_limits in the worker config).
func (w *Worker) claimablePlugins() []string {
	w.runningMu.Lock()
	defer w.runningMu.Unlock()
	out := make([]string, 0, len(w.enabledPlugins))
	for _, p := range w.enabledPlugins {
		if limit, ok := w.effectiveConfig.PluginLimits[p]; ok && limit > 0 && w.running[p] >= limit {
			continue
		}
		out = append(out, p)
	}
	return out
}

func (w *Worker) trackStart(pluginName string) {
	w.runningMu.Lock()
	w.running[pluginName]++
	w.runningMu.Unlock()
}

func (w *Worker) trackDone(pluginName string) {
	w.runningMu.Lock()
	w.running[pluginName]--
	w.runningMu.Unlock()
}
