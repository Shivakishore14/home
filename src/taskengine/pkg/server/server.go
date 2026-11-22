package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"taskengine/pkg/config"
	"taskengine/pkg/db"
	"taskengine/pkg/models"
	"taskengine/pkg/plugin"
	"taskengine/pkg/storage"
	"taskengine/web"
)

// Server coordinates HTTP API, SSE streaming, task scheduling, and worker lifecycle.
type Server struct {
	cfg            *config.Manager
	db             *db.DB
	pluginRegistry *plugin.Registry
	httpServer     *http.Server
	mux            *http.ServeMux

	sseMu      sync.Mutex
	sseClients map[chan models.SSEEvent]bool

	stopChan chan struct{}
	wg       sync.WaitGroup

	jellyfin  *jellyfinNotifier
	auth      *Auth
	store     storage.Store
	callbacks *callbackSender
}

// NewServer initializes a new Server instance.
func NewServer(cfg *config.Manager, database *db.DB, registry *plugin.Registry) *Server {
	if registry == nil {
		registry = plugin.DefaultRegistry
	}

	s := &Server{
		cfg:            cfg,
		db:             database,
		pluginRegistry: registry,
		mux:            http.NewServeMux(),
		sseClients:     make(map[chan models.SSEEvent]bool),
		stopChan:       make(chan struct{}),
		jellyfin:       newJellyfinNotifierFromEnv(),
		auth:           AuthFromEnv(),
		callbacks:      newCallbackSenderFromEnv(),
	}

	s.setupRoutes()
	return s
}

// setupRoutes registers all HTTP API and Web UI endpoints.
func (s *Server) setupRoutes() {
	// Public
	s.route("GET /healthz", accessPublic, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	s.route("GET /login", accessPublic, s.handleLoginPage)
	s.route("POST /login", accessPublic, s.handleLogin)
	s.route("POST /logout", accessPublic, s.handleLogout)

	// API v1 Endpoints (client)
	s.route("POST /api/v1/config/reload", accessClient, s.handleConfigReload)
	s.route("GET /api/v1/config", accessClient, s.handleGetConfig)
	s.route("GET /api/v1/stats", accessClient, s.handleGetStats)

	// Worker endpoints
	s.route("POST /api/v1/workers/register", accessWorker, s.handleRegisterWorker)
	s.route("POST /api/v1/workers/{id}/heartbeat", accessWorker, s.handleWorkerHeartbeat)
	s.route("GET /api/v1/workers", accessClient, s.handleListWorkers)

	// Task endpoints
	s.route("POST /api/v1/tasks/claim", accessWorker, s.handleClaimTask)
	s.route("POST /api/v1/tasks/retry-failed", accessClient, s.handleRetryFailedTasks)
	s.route("POST /api/v1/tasks/{id}/progress", accessWorker, s.handleTaskProgress)
	s.route("POST /api/v1/tasks/{id}/logs", accessWorker, s.handleTaskLog)
	s.route("POST /api/v1/tasks/{id}/complete", accessWorker, s.handleCompleteTask)
	s.route("POST /api/v1/tasks/{id}/fail", accessWorker, s.handleFailTask)
	s.route("POST /api/v1/tasks/{id}/cancel", accessClient, s.handleCancelTask)
	s.route("POST /api/v1/tasks", accessClient, s.handleCreateTask)
	s.route("GET /api/v1/tasks", accessClient, s.handleListTasks)
	s.route("GET /api/v1/tasks/{id}", accessClient, s.handleGetTask)
	s.route("GET /api/v1/tasks/{name}/assets", accessWorker, s.handleGetTaskAssets)
	s.route("POST /api/v1/callbacks/{id}/redeliver", accessClient, s.handleRedeliverCallback)
	s.route("GET /api/v1/files/{path...}", accessWorker, s.handleGetFile)
	s.route("GET /api/v1/binaries/{name}", accessWorker, s.handleGetBinary)

	// SSE Real-time stream
	s.route("GET /api/v1/events", accessClient, s.handleSSE)

	// Object storage (presigned uploads/downloads)
	s.storageRoutes()

	// Embedded Static Assets
	if staticFS, err := web.GetStaticFS(); err == nil {
		s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(staticFS)))
	}

	// Embedded Web Dashboard
	tmpl, err := web.GetTemplate()
	s.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/dashboard" {
			http.NotFound(w, r)
			return
		}
		if !s.auth.allowed(r, accessClient) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("Template error: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tmpl.Execute(w, nil)
	})
}

// SetAuth replaces the server's auth configuration (checked per request).
// Intended for tests and embedding.
func (s *Server) SetAuth(a *Auth) {
	s.auth = a
}

// AuthEnabled reports whether token auth is active.
func (s *Server) AuthEnabled() bool { return s.auth.Enabled() }

// Handler returns the HTTP handler (for testing or embedding).
func (s *Server) Handler() http.Handler {
	return s.mux
}

// Broadcast sends a real-time event to all connected SSE clients.
func (s *Server) Broadcast(eventType string, data interface{}) {
	event := models.SSEEvent{
		Type:      eventType,
		Timestamp: time.Now().UTC(),
		Data:      data,
	}

	s.sseMu.Lock()
	defer s.sseMu.Unlock()

	for ch := range s.sseClients {
		select {
		case ch <- event:
		default:
			// Client channel full; skip to prevent blocking
		}
	}
}

// Start starts background workers and the HTTP listener.
func (s *Server) Start(addr string) error {
	s.startBackgroundSweeper()
	s.startTaskGenerators()
	s.startCallbackDispatcher()

	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: s.mux,
	}

	log.Printf("[TaskEngine Server] Listening on http://%s", addr)
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server failed: %w", err)
	}
	return nil
}

// Shutdown gracefully shuts down background loops and the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	close(s.stopChan)
	s.wg.Wait()

	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

// startBackgroundSweeper periodically checks worker heartbeats and recovers stale tasks.
func (s *Server) startBackgroundSweeper() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		global := s.cfg.GetGlobal()
		interval := global.Server.StaleTaskCheckIntervalSeconds
		if interval <= 0 {
			interval = 10
		}
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()
		fileTicker := time.NewTicker(10 * time.Minute)
		defer fileTicker.Stop()

		for {
			select {
			case <-s.stopChan:
				return
			case <-fileTicker.C:
				s.expireFiles(context.Background())
			case <-ticker.C:
				currentGlobal := s.cfg.GetGlobal()
				timeout := currentGlobal.Server.HeartbeatTimeoutSeconds
				if timeout <= 0 {
					timeout = 30
				}
				recovered, err := s.db.RecoverStaleTasks(context.Background(), timeout)
				if err != nil {
					log.Printf("[Sweeper Error] %v", err)
				} else if recovered > 0 {
					log.Printf("[Sweeper] Recovered %d stale tasks back to PENDING", recovered)
					s.Broadcast("tasks_recovered", map[string]int{"count": recovered})
				}
			}
		}
	}()
}

// startTaskGenerators periodically runs registered ServerPlugins defined in tasks/definitions.
func (s *Server) startTaskGenerators() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-s.stopChan:
				return
			case <-ticker.C:
				defs := s.cfg.GetDefinitions()
				for _, def := range defs {
					if !def.Enabled {
						continue
					}
					pluginInstance, ok := s.pluginRegistry.GetServerPlugin(def.PluginName)
					if ok {
						// Generate tasks via ServerPlugin (e.g. folder scanner)
						paramsJSON, _ := json.Marshal(def.Params)
						_ = pluginInstance.Init(context.Background(), paramsJSON)
						generated, err := pluginInstance.GenerateTasks(context.Background())
						if err != nil {
							log.Printf("[Generator Error %s] %v", def.Name, err)
							continue
						}
						for _, item := range generated {
							hasActive, err := s.db.HasPendingOrRunningTask(context.Background(), item.PluginName, item.TargetFile)
							if err == nil && !hasActive {
								t, err := s.db.CreateTask(context.Background(), models.CreateTaskRequest{
									PluginName: item.PluginName,
									TargetFile: item.TargetFile,
									Priority:   def.Priority,
									Params:     item.Params,
								})
								if err == nil && t != nil {
									s.Broadcast("task_created", t)
								}
							}
						}
					} else {
						// Generic periodic task definition (e.g. command-runner)
						hasActive, err := s.db.HasPendingOrRunningTask(context.Background(), def.PluginName, def.Name)
						if err == nil && !hasActive {
							paramsMap := make(map[string]interface{})
							for k, v := range def.Params {
								paramsMap[k] = v
							}
							paramsMap["task_name"] = def.Name
							if def.Prerequisites.CheckCommand != "" {
								paramsMap["prerequisites"] = def.Prerequisites
							}

							paramsJSON, _ := json.Marshal(paramsMap)
							t, err := s.db.CreateTask(context.Background(), models.CreateTaskRequest{
								PluginName: def.PluginName,
								TargetFile: def.Name,
								Priority:   def.Priority,
								Params:     paramsJSON,
							})
							if err == nil && t != nil {
								s.Broadcast("task_created", t)
							}
						}
					}
				}
			}
		}
	}()
}

// --- HTTP Handlers ---

func (s *Server) handleConfigReload(w http.ResponseWriter, r *http.Request) {
	if err := s.cfg.Reload(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error": "%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	s.Broadcast("config_reloaded", map[string]string{"status": "ok"})
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status": "ok", "message": "Configuration reloaded successfully"}`))
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	global := s.cfg.GetGlobal()
	defs := s.cfg.GetDefinitions()
	resp := map[string]interface{}{
		"global":      global,
		"definitions": defs,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleGetStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.db.GetStats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (s *Server) handleRegisterWorker(w http.ResponseWriter, r *http.Request) {
	var req models.RegisterWorkerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.WorkerID == "" {
		http.Error(w, "worker_id is required", http.StatusBadRequest)
		return
	}

	// Record the plugins the worker will actually claim: the server-side
	// worker config overrides whatever the worker reports.
	wCfg := s.cfg.GetWorkerConfig(req.WorkerID)
	plugins := req.EnabledPlugins
	if len(wCfg.EnabledPlugins) > 0 {
		plugins = wCfg.EnabledPlugins
	}
	if err := s.db.RegisterWorker(r.Context(), req.WorkerID, req.Hostname, plugins); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	global := s.cfg.GetGlobal()
	hbInterval := global.Server.HeartbeatTimeoutSeconds / 3
	if hbInterval < 5 {
		hbInterval = 5
	}

	resp := models.RegisterWorkerResponse{
		WorkerID:           req.WorkerID,
		MaxConcurrentTasks: wCfg.MaxConcurrentTasks,
		EnabledPlugins:     wCfg.EnabledPlugins,
		PluginLimits:       wCfg.PluginLimits,
		ScratchDir:         wCfg.ScratchDir,
		PathMappings:       wCfg.PathMappings,
		PluginConfigs:      wCfg.PluginConfigs,
		HeartbeatInterval:  hbInterval,
	}

	s.Broadcast("worker_registered", req)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	workerID := r.PathValue("id")
	if workerID == "" {
		http.Error(w, "missing worker id", http.StatusBadRequest)
		return
	}

	if err := s.db.WorkerHeartbeat(r.Context(), workerID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "ok"}`))
}

func (s *Server) handleListWorkers(w http.ResponseWriter, r *http.Request) {
	workers, err := s.db.ListWorkers(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(workers)
}

func (s *Server) handleClaimTask(w http.ResponseWriter, r *http.Request) {
	var req models.ClaimTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.WorkerID == "" || len(req.Plugins) == 0 {
		http.Error(w, "worker_id and plugins are required", http.StatusBadRequest)
		return
	}

	task, err := s.db.ClaimNextTask(r.Context(), req.WorkerID, req.Plugins)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if task == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Mint download URLs for referenced input files
	payload, err := s.injectInputURLs(r.Context(), task.Payload)
	if err != nil {
		msg := fmt.Sprintf("Input files unavailable: %v", err)
		_ = s.db.FailTask(r.Context(), task.ID, msg, false)
		s.Broadcast("task_failed", map[string]interface{}{"id": task.ID, "error_message": msg, "can_retry": false})
		s.kickCallbacks()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	task.Payload = payload

	// Path translation on target file before sending to worker
	if task.TargetFile != "" {
		task.TargetFile = s.cfg.TranslatePath(req.WorkerID, task.TargetFile)
	}

	s.Broadcast("task_claimed", task)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(task)
}

func (s *Server) handleTaskProgress(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	var req models.ProgressRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid progress body", http.StatusBadRequest)
		return
	}

	if err := s.db.UpdateTaskProgress(r.Context(), taskID, req.Progress, req.Speed, req.Message); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.Broadcast("task_progress", map[string]interface{}{
		"id":       taskID,
		"progress": req.Progress,
		"speed":    req.Speed,
		"message":  req.Message,
	})
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "ok"}`))
}

func (s *Server) handleTaskLog(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	var req models.LogChunkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid log body", http.StatusBadRequest)
		return
	}

	if err := s.db.AppendTaskLog(r.Context(), taskID, req.LogChunk); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.Broadcast("task_log", map[string]interface{}{
		"id":        taskID,
		"log_chunk": req.LogChunk,
	})
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "ok"}`))
}

func (s *Server) handleCompleteTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	var req models.CompleteTaskRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	msg := req.Message
	if msg == "" {
		msg = "Task completed successfully"
	}

	s.confirmOutputs(r.Context(), taskID, req.Files)
	if err := s.db.CompleteTaskWithOutput(r.Context(), taskID, msg, req.OutputDetails); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.Broadcast("task_completed", map[string]interface{}{
		"id":      taskID,
		"message": msg,
	})
	s.kickCallbacks()

	if t, err := s.db.GetTask(r.Context(), taskID); err == nil && t.PluginName == "video-transcoder" {
		s.jellyfin.Schedule()
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "ok"}`))
}

func (s *Server) handleFailTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	var req models.FailTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid fail body", http.StatusBadRequest)
		return
	}

	if err := s.db.FailTask(r.Context(), taskID, req.ErrorMessage, req.CanRetry); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.Broadcast("task_failed", map[string]interface{}{
		"id":            taskID,
		"error_message": req.ErrorMessage,
		"can_retry":     req.CanRetry,
	})
	s.kickCallbacks()
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "ok"}`))
}

func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if err := s.db.CancelTask(r.Context(), taskID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Broadcast("task_cancelled", map[string]interface{}{"id": taskID})
	s.kickCallbacks()
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "ok"}`))
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req models.CreateTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid create task body", http.StatusBadRequest)
		return
	}
	if req.PluginName == "" {
		http.Error(w, "plugin_name is required", http.StatusBadRequest)
		return
	}
	if err := s.validateUploadRefs(r.Context(), req.PluginName, req.Params); err != nil {
		http.Error(w, err.Error(), storageErrStatus(err))
		return
	}
	if err := s.validateCallback(req.Callback); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errCallbacksDisabled) {
			code = http.StatusServiceUnavailable
		}
		http.Error(w, err.Error(), code)
		return
	}

	// Idempotent duplicate prevention
	if req.TargetFile != "" {
		hasActive, err := s.db.HasPendingOrRunningTask(r.Context(), req.PluginName, req.TargetFile)
		if err == nil && hasActive {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":  "skipped",
				"message": "task already pending or running",
			})
			return
		}
	}

	task, err := s.db.CreateTask(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.Broadcast("task_created", task)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(task)
}

func (s *Server) handleRetryFailedTasks(w http.ResponseWriter, r *http.Request) {
	count, err := s.db.RetryFailedTasks(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Broadcast("tasks_retried", map[string]interface{}{"count": count})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":        "ok",
		"retried_count": count,
	})
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limitStr := r.URL.Query().Get("limit")
	offsetStr := r.URL.Query().Get("offset")

	limit := 50
	if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
		limit = l
	}
	offset := 0
	if o, err := strconv.Atoi(offsetStr); err == nil && o >= 0 {
		offset = o
	}

	tasks, err := s.db.ListTasks(r.Context(), status, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tasks)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	task, err := s.db.GetTask(r.Context(), taskID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if task == nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	files, err := s.taskFiles(r.Context(), task.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	callbacks, err := s.db.ListTaskCallbacks(r.Context(), task.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, models.TaskDetail{Task: *task, Files: files, Callbacks: callbacks})
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	clientChan := make(chan models.SSEEvent, 100)
	s.sseMu.Lock()
	s.sseClients[clientChan] = true
	s.sseMu.Unlock()

	defer func() {
		s.sseMu.Lock()
		delete(s.sseClients, clientChan)
		close(clientChan)
		s.sseMu.Unlock()
	}()

	// Send initial connection event
	initData, _ := json.Marshal(map[string]string{"status": "connected"})
	fmt.Fprintf(w, "event: connected\ndata: %s\n\n", initData)
	flusher.Flush()

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			return
		case <-s.stopChan:
			return
		case event := <-clientChan:
			eventJSON, err := json.Marshal(event.Data)
			if err == nil {
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, eventJSON)
				flusher.Flush()
			}
		}
	}
}

// ServeHTTP implements http.Handler to allow mounting frontend and static assets alongside API routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleGetTaskAssets(w http.ResponseWriter, r *http.Request) {
	taskName := r.PathValue("name")
	assets, err := s.cfg.GetTaskAssets(taskName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(assets)
}

func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	relPath := r.PathValue("path")
	if relPath == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}

	absPath, err := s.cfg.GetAssetFilePath(relPath)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	http.ServeFile(w, r, absPath)
}

func (s *Server) handleGetBinary(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "missing binary name", http.StatusBadRequest)
		return
	}

	cleanName := filepath.Base(name)
	possiblePaths := []string{
		filepath.Join("bin", cleanName),
		filepath.Join("bin", "taskengine_"+cleanName),
		filepath.Join("src", "taskengine", "bin", cleanName),
		filepath.Join("src", "taskengine", "bin", "taskengine_"+cleanName),
	}

	for _, p := range possiblePaths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", cleanName))
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeFile(w, r, p)
			return
		}
	}

	http.Error(w, fmt.Sprintf("binary %q not found on server", cleanName), http.StatusNotFound)
}
