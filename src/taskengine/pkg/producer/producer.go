package producer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"taskengine/pkg/config"
	"taskengine/pkg/models"
	"taskengine/pkg/plugin"
)

// Config configures the TaskEngine Producer.
type Config struct {
	ServerURL string
	TasksDir  string
	Interval  time.Duration
	Once      bool
}

// Producer periodically scans local storage and dispatches discovered tasks to a remote server.
type Producer struct {
	cfg      Config
	cfgMgr   *config.Manager
	registry *plugin.Registry
	client   *http.Client
}

// New creates a new Producer instance.
func New(cfg Config, registry *plugin.Registry) (*Producer, error) {
	if cfg.ServerURL == "" {
		cfg.ServerURL = "http://localhost:8080"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if registry == nil {
		registry = plugin.DefaultRegistry
	}

	cfgMgr, err := config.NewManager(cfg.TasksDir)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration from %s: %w", cfg.TasksDir, err)
	}

	return &Producer{
		cfg:      cfg,
		cfgMgr:   cfgMgr,
		registry: registry,
		client:   &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Run begins the periodic scan and task production loop.
func (p *Producer) Run(ctx context.Context) error {
	log.Printf("[TaskEngine Producer] Starting producer targeting server: %s", p.cfg.ServerURL)
	log.Printf("[TaskEngine Producer] Scanning task definitions from: %s (Interval: %v)", p.cfg.TasksDir, p.cfg.Interval)

	if err := p.scanAndProduce(ctx); err != nil {
		log.Printf("[TaskEngine Producer] Scan error: %v", err)
	}

	if p.cfg.Once {
		log.Println("[TaskEngine Producer] Single scan complete. Exiting.")
		return nil
	}

	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[TaskEngine Producer] Shutting down producer...")
			return nil
		case <-ticker.C:
			// Dynamically reload task definitions from disk on each tick
			_ = p.cfgMgr.Reload()
			if err := p.scanAndProduce(ctx); err != nil {
				log.Printf("[TaskEngine Producer] Scan error: %v", err)
			}
		}
	}
}

func (p *Producer) scanAndProduce(ctx context.Context) error {
	defs := p.cfgMgr.GetDefinitions()
	var totalEnqueued, totalSkipped int

	for _, def := range defs {
		if !def.Enabled {
			continue
		}

		serverPlugin, ok := p.registry.GetServerPlugin(def.PluginName)
		if ok {
			paramsJSON, _ := json.Marshal(def.Params)
			_ = serverPlugin.Init(ctx, paramsJSON)
			generated, err := serverPlugin.GenerateTasks(ctx)
			if err != nil {
				log.Printf("[Producer Error %s] Failed to generate tasks: %v", def.Name, err)
				continue
			}

			for _, item := range generated {
				req := models.CreateTaskRequest{
					PluginName: item.PluginName,
					TargetFile: item.TargetFile,
					Priority:   def.Priority,
					Params:     item.Params,
				}

				created, err := p.sendTask(ctx, req)
				if err != nil {
					log.Printf("[Producer Warning] Failed to send task for %s: %v", item.TargetFile, err)
				} else if created {
					totalEnqueued++
				} else {
					totalSkipped++
				}
			}
		}
	}

	if totalEnqueued > 0 {
		log.Printf("[TaskEngine Producer] Dispatched %d new task(s) to server (%d active/skipped)", totalEnqueued, totalSkipped)
	}
	return nil
}

func (p *Producer) sendTask(ctx context.Context, req models.CreateTaskRequest) (bool, error) {
	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return false, err
	}

	url := fmt.Sprintf("%s/api/v1/tasks", p.cfg.ServerURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusCreated {
		return true, nil
	}
	if resp.StatusCode == http.StatusOK {
		return false, nil // Skipped as duplicate
	}
	return false, fmt.Errorf("server returned status: %d", resp.StatusCode)
}
