package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"taskengine/pkg/models"

	"gopkg.in/yaml.v3"
)

// ServerConfig holds server runtime parameters.
type ServerConfig struct {
	Port                          int    `yaml:"port"`
	DBPath                        string `yaml:"db_path"`
	HeartbeatTimeoutSeconds       int    `yaml:"heartbeat_timeout_seconds"`
	StaleTaskCheckIntervalSeconds int    `yaml:"stale_task_check_interval_seconds"`
}

// DefaultsConfig holds fallback settings for workers.
type DefaultsConfig struct {
	MaxConcurrentTasks int                    `yaml:"max_concurrent_tasks"`
	PathMappings       map[string]string      `yaml:"path_mappings"`
	PluginConfigs      map[string]interface{} `yaml:"plugin_configs"`
}

// StorageNamespace holds per-plugin storage policy. Object keys live under
// "<plugin>/in/..." (client uploads) and "<plugin>/out/<task_id>/..." (outputs).
// RetentionDays should mirror the bucket's lifecycle rule for that prefix.
type StorageNamespace struct {
	RetentionDays int      `yaml:"retention_days" json:"retention_days"`
	MaxUploadMB   int      `yaml:"max_upload_mb" json:"max_upload_mb"`
	MaxOutputMB   int      `yaml:"max_output_mb" json:"max_output_mb"`
	AllowedMIME   []string `yaml:"allowed_mime" json:"allowed_mime"`
}

// StorageConfig configures presigned object storage. Credentials come from
// the environment (R2_*); this file only holds policy.
type StorageConfig struct {
	PresignPutTTL string `yaml:"presign_put_ttl" json:"presign_put_ttl"`
	PresignGetTTL string `yaml:"presign_get_ttl" json:"presign_get_ttl"`
	// WorkerGetTTL is the lifetime of input URLs handed to workers at claim time.
	WorkerGetTTL string `yaml:"worker_get_ttl" json:"worker_get_ttl"`
	// Plugins may only use storage when they have a namespace entry here.
	Namespaces map[string]StorageNamespace `yaml:"namespaces" json:"namespaces"`
}

// CallbacksConfig controls task completion webhooks. The signing secret comes
// from the environment (TASKENGINE_CALLBACK_SECRET); this only holds policy.
type CallbacksConfig struct {
	// AllowedHosts lists the hosts (optionally host:port) a task's callback URL
	// may point at. Empty means callbacks are refused.
	AllowedHosts []string `yaml:"allowed_hosts" json:"allowed_hosts"`
}

// GlobalConfig represents the root config.yaml file.
type GlobalConfig struct {
	Server    ServerConfig    `yaml:"server"`
	Defaults  DefaultsConfig  `yaml:"defaults"`
	Storage   StorageConfig   `yaml:"storage"`
	Callbacks CallbacksConfig `yaml:"callbacks"`
}

// WorkerConfig represents per-worker overrides in tasks/workers/*.yaml.
type WorkerConfig struct {
	WorkerID           string   `yaml:"worker_id"`
	MaxConcurrentTasks int      `yaml:"max_concurrent_tasks"`
	EnabledPlugins     []string `yaml:"enabled_plugins"`
	// PluginLimits caps concurrent tasks per plugin within MaxConcurrentTasks
	// (e.g. comfyui: 1 so GPU generation never runs more than one at a time).
	PluginLimits  map[string]int         `yaml:"plugin_limits"`
	ScratchDir    string                 `yaml:"scratch_dir"`
	PathMappings  map[string]string      `yaml:"path_mappings"`
	PluginConfigs map[string]interface{} `yaml:"plugin_configs"`
}

// TaskAssetsDefinition represents asset configuration in task definition YAML.
type TaskAssetsDefinition struct {
	Directory string   `yaml:"directory"`
	Files     []string `yaml:"files"`
}

// TaskDefinition represents periodic task definitions in tasks/definitions/*.yaml.
type TaskDefinition struct {
	Name          string                   `yaml:"name"`
	PluginName    string                   `yaml:"plugin_name"`
	Schedule      string                   `yaml:"schedule"`
	Enabled       bool                     `yaml:"enabled"`
	Priority      int                      `yaml:"priority"`
	Scanner       map[string]interface{}   `yaml:"scanner"`
	Prerequisites models.TaskPrerequisites `yaml:"prerequisites"`
	Assets        TaskAssetsDefinition     `yaml:"assets"`
	Params        map[string]interface{}   `yaml:"params"`
}

// Manager loads and resolves configs from the tasks directory.
type Manager struct {
	tasksDir    string
	repoRoot    string
	mu          sync.RWMutex
	global      GlobalConfig
	workers     map[string]WorkerConfig
	definitions map[string]TaskDefinition
}

// NewManager creates a new config Manager pointing to the tasks directory.
func NewManager(tasksDir string) (*Manager, error) {
	absTasksDir, _ := filepath.Abs(tasksDir)
	repoRoot := filepath.Dir(absTasksDir)

	m := &Manager{
		tasksDir:    tasksDir,
		repoRoot:    repoRoot,
		workers:     make(map[string]WorkerConfig),
		definitions: make(map[string]TaskDefinition),
	}
	if err := m.Load(); err != nil {
		return nil, err
	}
	return m, nil
}

// Load parses all YAML files in the tasks directory.
func (m *Manager) Load() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Default fallback values
	global := GlobalConfig{
		Server: ServerConfig{
			Port:                          8080,
			DBPath:                        "taskengine.db",
			HeartbeatTimeoutSeconds:       30,
			StaleTaskCheckIntervalSeconds: 10,
		},
		Defaults: DefaultsConfig{
			MaxConcurrentTasks: 1,
			PathMappings:       make(map[string]string),
			PluginConfigs:      make(map[string]interface{}),
		},
	}

	configPath := filepath.Join(m.tasksDir, "config.yaml")
	if data, err := os.ReadFile(configPath); err == nil {
		if err := yaml.Unmarshal(data, &global); err != nil {
			return fmt.Errorf("failed to parse %s: %w", configPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to read %s: %w", configPath, err)
	}

	workers := make(map[string]WorkerConfig)
	workersDir := filepath.Join(m.tasksDir, "workers")
	if entries, err := os.ReadDir(workersDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml")) {
				continue
			}
			wPath := filepath.Join(workersDir, entry.Name())
			data, err := os.ReadFile(wPath)
			if err != nil {
				return fmt.Errorf("failed to read worker config %s: %w", wPath, err)
			}
			var wCfg WorkerConfig
			if err := yaml.Unmarshal(data, &wCfg); err != nil {
				return fmt.Errorf("failed to parse worker config %s: %w", wPath, err)
			}
			if wCfg.WorkerID == "" {
				wCfg.WorkerID = strings.TrimSuffix(strings.TrimSuffix(entry.Name(), ".yaml"), ".yml")
			}
			workers[wCfg.WorkerID] = wCfg
		}
	}

	definitions := make(map[string]TaskDefinition)
	defsDir := filepath.Join(m.tasksDir, "definitions")
	if entries, err := os.ReadDir(defsDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml")) {
				continue
			}
			dPath := filepath.Join(defsDir, entry.Name())
			data, err := os.ReadFile(dPath)
			if err != nil {
				return fmt.Errorf("failed to read task definition %s: %w", dPath, err)
			}
			var def TaskDefinition
			if err := yaml.Unmarshal(data, &def); err != nil {
				return fmt.Errorf("failed to parse task definition %s: %w", dPath, err)
			}
			if def.Name == "" {
				def.Name = strings.TrimSuffix(strings.TrimSuffix(entry.Name(), ".yaml"), ".yml")
			}
			definitions[def.Name] = def
		}
	}

	m.global = global
	m.workers = workers
	m.definitions = definitions
	return nil
}

// Reload re-reads configuration files from disk.
func (m *Manager) Reload() error {
	return m.Load()
}

// GetGlobal returns a copy of the current global configuration.
func (m *Manager) GetGlobal() GlobalConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.global
}

// GetDefinitions returns all loaded task definitions.
func (m *Manager) GetDefinitions() []TaskDefinition {
	m.mu.RLock()
	defer m.mu.RUnlock()
	defs := make([]TaskDefinition, 0, len(m.definitions))
	for _, d := range m.definitions {
		defs = append(defs, d)
	}
	return defs
}

// GetWorkerConfig resolves effective configuration for a given worker ID.
// Merges: Code Defaults -> Global Defaults -> Per-Worker Override.
func (m *Manager) GetWorkerConfig(workerID string) WorkerConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	res := WorkerConfig{
		WorkerID:           workerID,
		MaxConcurrentTasks: m.global.Defaults.MaxConcurrentTasks,
		PathMappings:       make(map[string]string),
		PluginConfigs:      make(map[string]interface{}),
	}
	if res.MaxConcurrentTasks <= 0 {
		res.MaxConcurrentTasks = 1
	}

	// Copy global defaults
	for k, v := range m.global.Defaults.PathMappings {
		res.PathMappings[k] = v
	}
	for k, v := range m.global.Defaults.PluginConfigs {
		res.PluginConfigs[k] = v
	}

	// Merge worker overrides if present
	if w, ok := m.workers[workerID]; ok {
		if w.MaxConcurrentTasks > 0 {
			res.MaxConcurrentTasks = w.MaxConcurrentTasks
		}
		if len(w.EnabledPlugins) > 0 {
			res.EnabledPlugins = w.EnabledPlugins
		}
		if len(w.PluginLimits) > 0 {
			res.PluginLimits = w.PluginLimits
		}
		if w.ScratchDir != "" {
			res.ScratchDir = w.ScratchDir
		}
		for k, v := range w.PathMappings {
			res.PathMappings[k] = v
		}
		for k, v := range w.PluginConfigs {
			res.PluginConfigs[k] = v
		}
	}

	return res
}

// TranslatePath translates a server-side path to a worker-local path.
// Matches the longest matching server path prefix and replaces it.
func (m *Manager) TranslatePath(workerID, serverPath string) string {
	if serverPath == "" {
		return ""
	}

	wCfg := m.GetWorkerConfig(workerID)
	var bestMatchKey string
	var bestMatchVal string

	for serverPrefix, workerPrefix := range wCfg.PathMappings {
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
		return filepath.Join(bestMatchVal, relPath)
	}

	return serverPath
}

// GetRepoRoot returns the repository root directory.
func (m *Manager) GetRepoRoot() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.repoRoot
}

// GetTaskDefinition finds a task definition by name.
func (m *Manager) GetTaskDefinition(name string) (TaskDefinition, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	def, ok := m.definitions[name]
	return def, ok
}

// GetTaskAssets resolves all assets for a given task definition, computing hashes and git commit version.
func (m *Manager) GetTaskAssets(taskName string) (*models.TaskAssets, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	def, ok := m.definitions[taskName]
	if !ok {
		return nil, fmt.Errorf("task definition %s not found", taskName)
	}

	result := &models.TaskAssets{
		TaskName: taskName,
		Files:    make([]models.TaskAssetItem, 0),
	}

	hasher := sha256.New()

	// Helper to hash and record a file
	processFile := func(relPath string) error {
		absPath := filepath.Join(m.repoRoot, relPath)
		info, err := os.Stat(absPath)
		if err != nil {
			absPath = filepath.Join(m.tasksDir, relPath)
			info, err = os.Stat(absPath)
			if err != nil {
				return err
			}
		}
		if info.IsDir() {
			return nil
		}

		f, err := os.Open(absPath)
		if err != nil {
			return err
		}
		defer f.Close()

		fHasher := sha256.New()
		if _, err := io.Copy(fHasher, f); err != nil {
			return err
		}
		fSum := hex.EncodeToString(fHasher.Sum(nil))

		hasher.Write([]byte(relPath + ":" + fSum))
		result.Files = append(result.Files, models.TaskAssetItem{
			Path:     relPath,
			Checksum: fSum,
			Size:     info.Size(),
		})
		return nil
	}

	// 1. Files listed explicitly
	for _, fPath := range def.Assets.Files {
		_ = processFile(fPath)
	}

	// 2. Directory scan
	if def.Assets.Directory != "" {
		absDir := filepath.Join(m.repoRoot, def.Assets.Directory)
		_ = filepath.Walk(absDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(m.repoRoot, path)
			if err == nil {
				_ = processFile(rel)
			}
			return nil
		})
	}

	// Version: check git commit hash in repoRoot first
	gitHash := getGitCommitHash(m.repoRoot)
	if gitHash != "" {
		result.Version = gitHash
	} else {
		result.Version = hex.EncodeToString(hasher.Sum(nil))
	}

	return result, nil
}

// GetAssetFilePath safely resolves a relative asset path to an absolute path.
// Only files inside the tasks directory are served (e.g. "tasks/assets/x.py"
// relative to the repo root, or "assets/x.py" relative to tasks/), so the API
// can never expose the database, binaries, or other files next to tasks/.
func (m *Manager) GetAssetFilePath(relPath string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cleaned := filepath.Clean(relPath)
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("invalid path traversal")
	}

	absTasksDir, err := filepath.Abs(m.tasksDir)
	if err != nil {
		return "", err
	}

	for _, candidate := range []string{filepath.Join(m.repoRoot, cleaned), filepath.Join(absTasksDir, cleaned)} {
		rel, err := filepath.Rel(absTasksDir, candidate)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("file not found: %s", relPath)
}

func getGitCommitHash(dir string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

// Durations parses the storage TTLs, applying defaults
// (upload 10m, client download 1h, worker download 6h).
func (c StorageConfig) Durations() (put, get, worker time.Duration) {
	parse := func(s string, def time.Duration) time.Duration {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
		return def
	}
	return parse(c.PresignPutTTL, 10*time.Minute), parse(c.PresignGetTTL, time.Hour), parse(c.WorkerGetTTL, 6*time.Hour)
}

// Namespace returns the storage policy for plugin, with defaults applied.
func (c StorageConfig) Namespace(plugin string) (StorageNamespace, bool) {
	ns, ok := c.Namespaces[plugin]
	if !ok {
		return ns, false
	}
	if ns.RetentionDays <= 0 {
		ns.RetentionDays = 10
	}
	if ns.MaxUploadMB <= 0 {
		ns.MaxUploadMB = 25
	}
	if ns.MaxOutputMB <= 0 {
		ns.MaxOutputMB = 512
	}
	return ns, true
}

// AllowsMIME reports whether mime is allowed for uploads ("*" or "image/*" style wildcards supported; empty list allows all).
func (ns StorageNamespace) AllowsMIME(mime string) bool {
	if len(ns.AllowedMIME) == 0 {
		return true
	}
	for _, a := range ns.AllowedMIME {
		if a == "*" || a == mime || (strings.HasSuffix(a, "/*") && strings.HasPrefix(mime, strings.TrimSuffix(a, "*"))) {
			return true
		}
	}
	return false
}
