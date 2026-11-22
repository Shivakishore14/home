// Package comfyui runs ComfyUI workflows as TaskEngine tasks.
//
// Task params:
//
//	{"workflow": "jeveration_t2i", "inputs": {"prompt": "a fox", "image": {"upload": "<file id>"}}}
//
// The workflow template (API-format JSON) and its manifest (YAML, declaring
// which node inputs clients may set) are fetched from the server's
// tasks/assets/comfyui/ directory. Outputs are uploaded to object storage
// and attached to the task.
package comfyui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // decoders for thumbnails
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
	"gopkg.in/yaml.v3"

	"taskengine/pkg/plugin"
)

const pluginName = "comfyui"

func init() {
	plugin.RegisterWorker(New())
}

// Config is the worker-side plugin config (tasks/workers/<id>.yaml plugin_configs.comfyui).
type Config struct {
	// URL of the ComfyUI server (default http://127.0.0.1:8188).
	URL string `json:"url"`
	// AssetsPath is where workflow templates live on the server (default tasks/assets/comfyui).
	AssetsPath string `json:"assets_path"`
	// TimeoutMinutes bounds one task (default 60).
	TimeoutMinutes int `json:"timeout_minutes"`
	// ThumbnailSize is the max edge of generated JPEG thumbnails (default 384, 0 = default, -1 = off).
	ThumbnailSize int `json:"thumbnail_size"`
}

// Plugin implements plugin.WorkerPlugin.
type Plugin struct {
	mu  sync.RWMutex
	cfg Config
}

// New returns a plugin with default config.
func New() *Plugin {
	p := &Plugin{}
	_ = p.Init(context.Background(), nil)
	return p
}

func (p *Plugin) Name() string { return pluginName }

func (p *Plugin) Init(_ context.Context, raw json.RawMessage) error {
	cfg := Config{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("comfyui config: %w", err)
		}
	}
	if cfg.URL == "" {
		cfg.URL = "http://127.0.0.1:8188"
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	if cfg.AssetsPath == "" {
		cfg.AssetsPath = "tasks/assets/comfyui"
	}
	if cfg.TimeoutMinutes <= 0 {
		cfg.TimeoutMinutes = 60
	}
	if cfg.ThumbnailSize == 0 {
		cfg.ThumbnailSize = 384
	}
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
	return nil
}

func (p *Plugin) config() Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

// Params is the task payload.
type Params struct {
	Workflow string                     `json:"workflow"`
	Inputs   map[string]json.RawMessage `json:"inputs"`
}

// Execute runs one workflow end to end.
func (p *Plugin) Execute(ctx context.Context, payload plugin.TaskPayload, reporter plugin.ProgressReporter) error {
	cfg := p.config()
	tio, ok := plugin.IOFrom(reporter)
	if !ok {
		return plugin.Permanent(errors.New("comfyui plugin requires a TaskEngine worker with storage support"))
	}
	logf := func(format string, args ...interface{}) {
		_ = reporter.Report(ctx, plugin.ProgressReport{LogChunk: fmt.Sprintf(format, args...) + "\n"})
	}
	status := func(progress float64, msg string) {
		_ = reporter.Report(ctx, plugin.ProgressReport{Progress: progress, Message: msg})
	}

	var params Params
	if err := json.Unmarshal(payload.Params, &params); err != nil {
		return plugin.Permanent(fmt.Errorf("invalid params: %w", err))
	}
	if !workflowName.MatchString(params.Workflow) {
		return plugin.Permanent(fmt.Errorf("invalid workflow name %q", params.Workflow))
	}

	// 1. Load template + manifest from the server.
	graph, manifest, err := p.loadWorkflow(ctx, tio, cfg, params.Workflow)
	if err != nil {
		return err
	}
	resolved, files, err := manifest.Apply(graph, params.Inputs)
	if err != nil {
		return plugin.Permanent(err)
	}
	logf("[comfyui] workflow %s, params %s", params.Workflow, compactJSON(resolved))

	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutMinutes)*time.Minute)
	defer cancel()
	c := &client{base: cfg.URL, http: &http.Client{}}

	pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
	err = c.ping(pingCtx)
	pingCancel()
	if err != nil {
		return fmt.Errorf("ComfyUI not reachable at %s: %w", cfg.URL, err) // retryable
	}

	work, err := os.MkdirTemp(scratchDir(), "comfyui-"+payload.ID+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	// 2. Stage input files into ComfyUI's input dir.
	subfolder := "taskengine/" + payload.ID
	for i, fi := range files {
		status(1, fmt.Sprintf("Uploading input %s", fi.Param))
		local := filepath.Join(work, fmt.Sprintf("in-%d%s", i, path.Ext(fi.Filename)))
		if err := downloadURL(ctx, fi.URL, local); err != nil {
			return fmt.Errorf("download input %s: %w", fi.Param, err)
		}
		name := fi.Filename
		if name == "" {
			name = fmt.Sprintf("input-%d%s", i, extFor(fi.MIME))
		}
		value, err := c.uploadInput(ctx, local, name, subfolder)
		if err != nil {
			return err
		}
		graph[fi.Node].Inputs[fi.Input] = value
		logf("[comfyui] staged %s -> %s", fi.Param, value)
	}

	// 3. Listen for progress before queueing so no events are missed.
	events, wsErr := c.listen(ctx, payload.ID)
	if wsErr != nil {
		logf("[comfyui] websocket unavailable (%v); falling back to polling", wsErr)
	}

	promptID, err := c.queuePrompt(ctx, graph, payload.ID)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusBadRequest {
			return plugin.Permanent(fmt.Errorf("ComfyUI rejected the workflow: %s", ae.Body))
		}
		return fmt.Errorf("queue prompt: %w", err)
	}
	logf("[comfyui] queued prompt %s", promptID)
	status(2, "Queued in ComfyUI")

	// 4. Wait for completion.
	entry, err := p.wait(ctx, c, promptID, len(graph), events, status, logf)
	if err != nil {
		// Leave nothing running on the GPU for a task we gave up on.
		cleanupCtx, cc := context.WithTimeout(context.Background(), 10*time.Second)
		c.cancel(cleanupCtx, promptID)
		cc()
		return err
	}

	// 5. Upload outputs.
	outputs, err := p.collectOutputs(ctx, c, tio, cfg, entry, work, status, logf)
	if err != nil {
		return err
	}
	if len(outputs) == 0 {
		return plugin.Permanent(errors.New("workflow produced no output files (does it end in a Save node?)"))
	}

	tio.SetOutput(map[string]interface{}{
		"workflow":  params.Workflow,
		"prompt_id": promptID,
		"params":    resolved,
		"outputs":   outputs,
	})
	status(100, fmt.Sprintf("Done: %d output(s)", len(outputs)))
	return nil
}

func (p *Plugin) loadWorkflow(ctx context.Context, tio plugin.TaskIO, cfg Config, name string) (Graph, *Manifest, error) {
	base := strings.TrimSuffix(cfg.AssetsPath, "/") + "/" + name
	manifestData, err := tio.FetchAsset(ctx, base+".yaml")
	if err != nil {
		return nil, nil, plugin.Permanent(fmt.Errorf("unknown workflow %q: %w", name, err))
	}
	var head struct {
		Workflow string `yaml:"workflow"`
	}
	if err := yaml.Unmarshal(manifestData, &head); err != nil {
		return nil, nil, plugin.Permanent(fmt.Errorf("manifest %s: %w", name, err))
	}
	wfFile := head.Workflow
	if wfFile == "" {
		wfFile = name + ".json"
	}
	if strings.Contains(wfFile, "/") || strings.Contains(wfFile, "..") {
		return nil, nil, plugin.Permanent(fmt.Errorf("manifest %s: workflow must be a file name next to the manifest", name))
	}
	graphData, err := tio.FetchAsset(ctx, strings.TrimSuffix(cfg.AssetsPath, "/")+"/"+wfFile)
	if err != nil {
		return nil, nil, plugin.Permanent(fmt.Errorf("workflow %q template: %w", name, err))
	}
	graph, err := ParseGraph(graphData)
	if err != nil {
		return nil, nil, plugin.Permanent(err)
	}
	manifest, err := ParseManifest(manifestData, graph)
	if err != nil {
		return nil, nil, plugin.Permanent(fmt.Errorf("manifest %s: %w", name, err))
	}
	return graph, manifest, nil
}

// wait blocks until the prompt finishes, reporting progress from websocket
// events. /history polling is the source of truth for completion.
func (p *Plugin) wait(ctx context.Context, c *client, promptID string, totalNodes int, events <-chan wsMessage,
	status func(float64, string), logf func(string, ...interface{})) (*historyEntry, error) {

	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()

	done := map[string]bool{}
	current := ""
	frac := 0.0
	lastPct := -1.0
	report := func(msg string) {
		pct := 5.0
		if totalNodes > 0 {
			pct = 5 + 85*(float64(len(done))+frac)/float64(totalNodes)
		}
		if pct > 90 {
			pct = 90
		}
		if pct-lastPct >= 0.5 || msg != "" {
			lastPct = pct
			status(pct, msg)
		}
	}

	check := func() (*historyEntry, error) {
		hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		entry, err := c.history(hctx, promptID)
		if err != nil || entry == nil {
			return nil, nil // transient or not finished yet
		}
		switch {
		case entry.Status.StatusStr == "error":
			return nil, plugin.Permanent(fmt.Errorf("ComfyUI execution failed: %s", historyError(entry)))
		case entry.Status.Completed:
			return entry, nil
		}
		return nil, nil
	}

	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, plugin.Permanent(errors.New("timed out waiting for ComfyUI"))
			}
			return nil, ctx.Err()

		case <-poll.C:
			if entry, err := check(); entry != nil || err != nil {
				return entry, err
			}

		case m, ok := <-events:
			if !ok {
				events = nil // socket closed; keep polling
				continue
			}
			var d struct {
				PromptID string   `json:"prompt_id"`
				Node     *string  `json:"node"`
				NodeID   string   `json:"node_id"`
				NodeType string   `json:"node_type"`
				Value    float64  `json:"value"`
				Max      float64  `json:"max"`
				Nodes    []string `json:"nodes"`
				Message  string   `json:"exception_message"`
			}
			if json.Unmarshal(m.Data, &d) != nil || (d.PromptID != "" && d.PromptID != promptID) {
				continue
			}
			switch m.Type {
			case "execution_start":
				report("Running")
			case "execution_cached":
				for _, n := range d.Nodes {
					done[n] = true
				}
				report("")
			case "executing":
				if current != "" {
					done[current] = true
				}
				frac = 0
				if d.Node == nil {
					if entry, err := check(); entry != nil || err != nil {
						return entry, err
					}
					continue
				}
				current = *d.Node
				report("Running node " + current)
			case "progress":
				if d.Max > 0 {
					frac = d.Value / d.Max
					report(fmt.Sprintf("Node %s: step %d/%d", current, int(d.Value), int(d.Max)))
				}
			case "execution_error":
				logf("[comfyui] error in node %s (%s): %s", d.NodeID, d.NodeType, d.Message)
				return nil, plugin.Permanent(fmt.Errorf("ComfyUI node %s (%s) failed: %s", d.NodeID, d.NodeType, d.Message))
			case "execution_interrupted":
				return nil, errors.New("ComfyUI execution was interrupted")
			}
		}
	}
}

func historyError(e *historyEntry) string {
	for _, msg := range e.Status.Messages {
		if len(msg) == 2 {
			var typ string
			_ = json.Unmarshal(msg[0], &typ)
			if typ == "execution_error" {
				var d struct {
					NodeType string `json:"node_type"`
					Message  string `json:"exception_message"`
				}
				_ = json.Unmarshal(msg[1], &d)
				return fmt.Sprintf("%s: %s", d.NodeType, strings.TrimSpace(d.Message))
			}
		}
	}
	return "unknown error"
}

// OutputInfo describes one uploaded output in the task's output details.
type OutputInfo struct {
	FileID      string `json:"file_id"`
	ThumbnailID string `json:"thumbnail_id,omitempty"`
	Filename    string `json:"filename"`
	Node        string `json:"node"`
	Kind        string `json:"kind"` // images, gifs, audio, video...
	MIME        string `json:"mime"`
}

func (p *Plugin) collectOutputs(ctx context.Context, c *client, tio plugin.TaskIO, cfg Config, entry *historyEntry, work string,
	status func(float64, string), logf func(string, ...interface{})) ([]OutputInfo, error) {

	type item struct {
		node, kind string
		file       OutputFile
	}
	var items []item
	nodeIDs := make([]string, 0, len(entry.Outputs))
	for id := range entry.Outputs {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	for _, id := range nodeIDs {
		kinds := make([]string, 0, len(entry.Outputs[id]))
		for k := range entry.Outputs[id] {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		for _, kind := range kinds {
			var list []OutputFile
			if json.Unmarshal(entry.Outputs[id][kind], &list) != nil {
				continue // not a file list (e.g. "text" outputs)
			}
			for _, f := range list {
				if f.Filename != "" && f.Type == "output" { // skip temp previews
					items = append(items, item{node: id, kind: kind, file: f})
				}
			}
		}
	}

	var outputs []OutputInfo
	for i, it := range items {
		status(90+9*float64(i)/float64(len(items)), fmt.Sprintf("Uploading output %d/%d", i+1, len(items)))
		local := filepath.Join(work, fmt.Sprintf("out-%d%s", i, path.Ext(it.file.Filename)))
		if err := saveTo(local, func(w *os.File) error { return c.download(ctx, it.file, w) }); err != nil {
			return nil, fmt.Errorf("download output %s: %w", it.file.Filename, err)
		}
		mt := mimeFor(it.file.Filename)
		meta := map[string]string{"node": it.node, "kind": it.kind}
		id, err := tio.UploadOutput(ctx, local, it.file.Filename, mt, meta)
		if err != nil {
			return nil, err
		}
		info := OutputInfo{FileID: id, Filename: it.file.Filename, Node: it.node, Kind: it.kind, MIME: mt}

		if cfg.ThumbnailSize > 0 && strings.HasPrefix(mt, "image/") {
			thumb := local + ".thumb.jpg"
			if err := makeThumbnail(local, thumb, cfg.ThumbnailSize); err == nil {
				name := strings.TrimSuffix(it.file.Filename, path.Ext(it.file.Filename)) + "_thumb.jpg"
				if tid, err := tio.UploadOutput(ctx, thumb, name, "image/jpeg", map[string]string{"role": "thumbnail", "of": id, "node": it.node}); err == nil {
					info.ThumbnailID = tid
				}
			} else {
				logf("[comfyui] thumbnail for %s failed: %v", it.file.Filename, err)
			}
		}
		outputs = append(outputs, info)
		logf("[comfyui] uploaded %s (%s)", it.file.Filename, id)
	}
	return outputs, nil
}

func makeThumbnail(src, dst string, maxEdge int) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > h && w > maxEdge {
		h, w = h*maxEdge/w, maxEdge
	} else if h >= w && h > maxEdge {
		w, h = w*maxEdge/h, maxEdge
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(out, out.Bounds(), img, b, draw.Over, nil)
	return saveTo(dst, func(fw *os.File) error { return jpeg.Encode(fw, out, &jpeg.Options{Quality: 82}) })
}

func saveTo(path string, write func(*os.File) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := write(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func downloadURL(ctx context.Context, u, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("HTTP %d", resp.StatusCode)
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			return plugin.Permanent(fmt.Errorf("input no longer available: %w", err))
		}
		return err
	}
	return saveTo(dst, func(f *os.File) error { _, err := f.ReadFrom(resp.Body); return err })
}

func scratchDir() string {
	d := os.Getenv("SCRATCH_DIR")
	if strings.HasPrefix(d, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			d = filepath.Join(home, d[2:])
		}
	}
	if d != "" {
		if err := os.MkdirAll(d, 0o755); err == nil {
			return d
		}
	}
	return os.TempDir()
}

func mimeFor(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".flac":
		return "audio/flac"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".glb":
		return "model/gltf-binary"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return strings.SplitN(t, ";", 2)[0]
	}
	return "application/octet-stream"
}

func extFor(mimeType string) string {
	switch mimeType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	}
	return ""
}

func compactJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	if len(b) > 500 {
		return string(b[:500]) + "…"
	}
	return string(b)
}
