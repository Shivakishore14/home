package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// TaskPayload represents the data passed from Server to Worker for a job.
type TaskPayload struct {
	ID         string          `json:"id"`
	PluginName string          `json:"plugin_name"`
	TargetFile string          `json:"target_file,omitempty"`
	Params     json.RawMessage `json:"params"`
}

// ProgressReport is sent from Worker to Server during task execution.
type ProgressReport struct {
	Progress float64 `json:"progress"` // 0.0 to 100.0
	Speed    string  `json:"speed,omitempty"`
	Message  string  `json:"message,omitempty"`
	LogChunk string  `json:"log_chunk,omitempty"`
}

// ProgressReporter allows worker plugins to send real-time updates.
type ProgressReporter interface {
	Report(ctx context.Context, report ProgressReport) error
}

// ServerPlugin generates or schedules tasks on the server.
type ServerPlugin interface {
	Name() string
	Init(ctx context.Context, config json.RawMessage) error
	GenerateTasks(ctx context.Context) ([]TaskPayload, error)
}

// WorkerPlugin executes claimed tasks on a worker.
type WorkerPlugin interface {
	Name() string
	Init(ctx context.Context, config json.RawMessage) error
	Execute(ctx context.Context, payload TaskPayload, reporter ProgressReporter) error
}

// Registry manages registered plugins.
type Registry struct {
	mu            sync.RWMutex
	serverPlugins map[string]ServerPlugin
	workerPlugins map[string]WorkerPlugin
}

// NewRegistry creates an empty plugin registry.
func NewRegistry() *Registry {
	return &Registry{
		serverPlugins: make(map[string]ServerPlugin),
		workerPlugins: make(map[string]WorkerPlugin),
	}
}

// RegisterServerPlugin registers a server-side plugin.
func (r *Registry) RegisterServerPlugin(plugin ServerPlugin) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.serverPlugins[plugin.Name()] = plugin
}

// RegisterWorkerPlugin registers a worker-side plugin.
func (r *Registry) RegisterWorkerPlugin(plugin WorkerPlugin) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workerPlugins[plugin.Name()] = plugin
}

// GetServerPlugin retrieves a server plugin by name.
func (r *Registry) GetServerPlugin(name string) (ServerPlugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.serverPlugins[name]
	return p, ok
}

// GetWorkerPlugin retrieves a worker plugin by name.
func (r *Registry) GetWorkerPlugin(name string) (WorkerPlugin, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.workerPlugins[name]
	return p, ok
}

// EnabledWorkerPlugins returns the list of registered worker plugin names.
func (r *Registry) EnabledWorkerPlugins() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.workerPlugins))
	for name := range r.workerPlugins {
		names = append(names, name)
	}
	return names
}

// DefaultRegistry is the global plugin registry instance.
var DefaultRegistry = NewRegistry()

// RegisterServer registers a plugin to the default registry.
func RegisterServer(p ServerPlugin) {
	DefaultRegistry.RegisterServerPlugin(p)
}

// RegisterWorker registers a worker plugin to the default registry.
func RegisterWorker(p WorkerPlugin) {
	DefaultRegistry.RegisterWorkerPlugin(p)
}

// FuncProgressReporter helper implementation of ProgressReporter.
type FuncProgressReporter struct {
	Func func(ctx context.Context, report ProgressReport) error
}

func (f *FuncProgressReporter) Report(ctx context.Context, report ProgressReport) error {
	if f.Func == nil {
		return fmt.Errorf("nil progress callback")
	}
	return f.Func(ctx, report)
}
