package comfyui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"taskengine/pkg/plugin"
)

const testGraph = `{
  "1": {"class_type": "CheckpointLoader", "inputs": {"ckpt_name": "m.safetensors"}},
  "2": {"class_type": "CLIPTextEncode", "inputs": {"text": "default", "clip": ["1", 1]}},
  "3": {"class_type": "Sampler", "inputs": {"model": ["1", 0], "positive": ["2", 0], "seed": 0, "steps": 4}},
  "4": {"class_type": "LoadImage", "inputs": {"image": "placeholder.png"}},
  "5": {"class_type": "SaveImage", "inputs": {"images": ["3", 0], "filename_prefix": "t"}}
}`

const testManifest = `
title: Test
workflow: test.json
params:
  prompt: {node: "2", input: text, type: text, required: true, max_length: 50}
  seed:   {node: "3", input: seed, type: seed}
  steps:  {node: "3", input: steps, type: int, default: 4, min: 1, max: 8}
  image:  {node: "4", input: image, type: image}
`

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, x%h, color.RGBA{255, 0, 0, 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeIO implements plugin.ProgressReporter + plugin.TaskIO.
type fakeIO struct {
	assets  map[string][]byte
	mu      sync.Mutex
	uploads []struct {
		name, mime string
		data       []byte
		meta       map[string]string
	}
	output   map[string]interface{}
	progress []float64
	logs     strings.Builder
}

func (f *fakeIO) Report(_ context.Context, r plugin.ProgressReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Progress > 0 {
		f.progress = append(f.progress, r.Progress)
	}
	f.logs.WriteString(r.LogChunk)
	return nil
}
func (f *fakeIO) UploadOutput(_ context.Context, p, name, mime string, meta map[string]string) (string, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, struct {
		name, mime string
		data       []byte
		meta       map[string]string
	}{name, mime, data, meta})
	return fmt.Sprintf("file-%d", len(f.uploads)), nil
}
func (f *fakeIO) SetOutput(d map[string]interface{}) { f.output = d }
func (f *fakeIO) FetchAsset(_ context.Context, rel string) ([]byte, error) {
	if b, ok := f.assets[rel]; ok {
		return b, nil
	}
	return nil, errors.New("404")
}

func newIO() *fakeIO {
	return &fakeIO{assets: map[string][]byte{
		"tasks/assets/comfyui/test.yaml": []byte(testManifest),
		"tasks/assets/comfyui/test.json": []byte(testGraph),
	}}
}

// fakeComfy emulates the parts of ComfyUI's API the plugin uses.
type fakeComfy struct {
	t          *testing.T
	srv        *httptest.Server
	mu         sync.Mutex
	prompt     Graph
	uploaded   map[string][]byte
	interrupts int
	// behaviour
	rejectPrompt bool
	failExec     bool
	hang         bool
	output       []byte
}

func newFakeComfy(t *testing.T) *fakeComfy {
	f := &fakeComfy{t: t, uploaded: map[string][]byte{}, output: pngBytes(t, 800, 600)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /system_stats", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	mux.HandleFunc("POST /upload/image", func(w http.ResponseWriter, r *http.Request) {
		file, hdr, err := r.FormFile("image")
		if err != nil || r.FormValue("type") != "input" {
			http.Error(w, "bad upload", 400)
			return
		}
		data, _ := io.ReadAll(file)
		sub := r.FormValue("subfolder")
		f.mu.Lock()
		f.uploaded[sub+"/"+hdr.Filename] = data
		f.mu.Unlock()
		// ComfyUI may rename on collision; the plugin must use the returned name.
		json.NewEncoder(w).Encode(map[string]string{"name": "renamed_" + hdr.Filename, "subfolder": sub, "type": "input"})
	})
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		<-r.Context().Done()
	})
	mux.HandleFunc("POST /prompt", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prompt   Graph  `json:"prompt"`
			ClientID string `json:"client_id"`
		}
		dec := json.NewDecoder(r.Body)
		dec.UseNumber()
		_ = dec.Decode(&body)
		if f.rejectPrompt {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"message":"Prompt outputs failed validation"},"node_errors":{"3":{}}}`))
			return
		}
		f.mu.Lock()
		f.prompt = body.Prompt
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"prompt_id": "p-1", "number": 1, "node_errors": map[string]interface{}{}})
	})
	mux.HandleFunc("GET /history/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.hang {
			w.Write([]byte(`{}`))
			return
		}
		if f.failExec {
			w.Write([]byte(`{"p-1":{"outputs":{},"status":{"status_str":"error","completed":false,"messages":[["execution_error",{"node_type":"Sampler","exception_message":"CUDA out of memory"}]]}}}`))
			return
		}
		w.Write([]byte(`{"p-1":{"outputs":{
			"5":{"images":[{"filename":"t_00001_.png","subfolder":"","type":"output"}]},
			"9":{"images":[{"filename":"preview.png","subfolder":"","type":"temp"}]}
		},"status":{"status_str":"success","completed":true,"messages":[]}}}`))
	})
	mux.HandleFunc("GET /view", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") != "output" {
			http.Error(w, "nope", 404)
			return
		}
		w.Write(f.output)
	})
	mux.HandleFunc("POST /interrupt", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.interrupts++
		f.mu.Unlock()
	})
	mux.HandleFunc("POST /queue", func(w http.ResponseWriter, r *http.Request) {})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newPlugin(t *testing.T, url string) *Plugin {
	p := New()
	cfg, _ := json.Marshal(Config{URL: url, TimeoutMinutes: 1})
	if err := p.Init(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return p
}

func payload(params string) plugin.TaskPayload {
	return plugin.TaskPayload{ID: "task-1", PluginName: "comfyui", Params: json.RawMessage(params)}
}

func TestExecuteHappyPath(t *testing.T) {
	comfy := newFakeComfy(t)
	input := pngBytes(t, 10, 10)
	inputSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(input) }))
	defer inputSrv.Close()

	io := newIO()
	params := fmt.Sprintf(`{"workflow":"test","inputs":{"prompt":"a red fox","steps":6,
		"image":{"upload":"u1","url":%q,"filename":"fox.png","mime":"image/png","size":%d}}}`, inputSrv.URL+"/x", len(input))

	err := newPlugin(t, comfy.srv.URL).Execute(context.Background(), payload(params), io)
	if err != nil {
		t.Fatalf("execute: %v\nlogs:\n%s", err, io.logs.String())
	}

	// Graph was patched from the manifest.
	g := comfy.prompt
	if g["2"].Inputs["text"] != "a red fox" {
		t.Errorf("prompt not applied: %v", g["2"].Inputs["text"])
	}
	if fmt.Sprint(g["3"].Inputs["steps"]) != "6" {
		t.Errorf("steps not applied: %v", g["3"].Inputs["steps"])
	}
	if fmt.Sprint(g["3"].Inputs["seed"]) == "0" {
		t.Errorf("seed should be randomized")
	}
	if g["4"].Inputs["image"] != "taskengine/task-1/renamed_fox.png" {
		t.Errorf("image input = %v", g["4"].Inputs["image"])
	}
	if !bytes.Equal(comfy.uploaded["taskengine/task-1/fox.png"], input) {
		t.Errorf("input not staged into ComfyUI")
	}

	// Output + thumbnail uploaded; temp preview skipped.
	if len(io.uploads) != 2 {
		t.Fatalf("uploads = %d", len(io.uploads))
	}
	if io.uploads[0].name != "t_00001_.png" || !bytes.Equal(io.uploads[0].data, comfy.output) || io.uploads[0].meta["node"] != "5" {
		t.Errorf("output upload: %s %v", io.uploads[0].name, io.uploads[0].meta)
	}
	thumb, _, err := image.Decode(bytes.NewReader(io.uploads[1].data))
	if err != nil || thumb.Bounds().Dx() != 384 || thumb.Bounds().Dy() != 288 || io.uploads[1].meta["of"] != "file-1" {
		t.Errorf("thumbnail: err=%v meta=%v", err, io.uploads[1].meta)
	}

	outs, _ := io.output["outputs"].([]OutputInfo)
	if io.output["prompt_id"] != "p-1" || len(outs) != 1 || outs[0].ThumbnailID != "file-2" {
		t.Errorf("output details: %+v", io.output)
	}
	if last := io.progress[len(io.progress)-1]; last != 100 {
		t.Errorf("final progress %v", last)
	}
}

func TestExecuteErrors(t *testing.T) {
	cases := []struct {
		name      string
		params    string
		setup     func(*fakeComfy)
		permanent bool
		contains  string
	}{
		{"missing required", `{"workflow":"test","inputs":{}}`, nil, true, "required"},
		{"unknown param", `{"workflow":"test","inputs":{"prompt":"x","cfg":7}}`, nil, true, "unknown parameter"},
		{"out of range", `{"workflow":"test","inputs":{"prompt":"x","steps":99}}`, nil, true, "<= 8"},
		{"too long", `{"workflow":"test","inputs":{"prompt":"` + strings.Repeat("a", 51) + `"}}`, nil, true, "at most"},
		{"bad workflow name", `{"workflow":"../etc","inputs":{}}`, nil, true, "invalid workflow"},
		{"unknown workflow", `{"workflow":"nope","inputs":{}}`, nil, true, "unknown workflow"},
		{"upload without url", `{"workflow":"test","inputs":{"prompt":"x","image":{"upload":"u1"}}}`, nil, true, "no download URL"},
		{"prompt rejected", `{"workflow":"test","inputs":{"prompt":"x"}}`, func(f *fakeComfy) { f.rejectPrompt = true }, true, "rejected the workflow"},
		{"execution error", `{"workflow":"test","inputs":{"prompt":"x"}}`, func(f *fakeComfy) { f.failExec = true }, true, "CUDA out of memory"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			comfy := newFakeComfy(t)
			if c.setup != nil {
				c.setup(comfy)
			}
			err := newPlugin(t, comfy.srv.URL).Execute(context.Background(), payload(c.params), newIO())
			if err == nil || !strings.Contains(err.Error(), c.contains) {
				t.Fatalf("err = %v, want containing %q", err, c.contains)
			}
			var pe *plugin.PermanentError
			if errors.As(err, &pe) != c.permanent {
				t.Errorf("permanent = %v, want %v", !c.permanent, c.permanent)
			}
		})
	}
}

func TestComfyDownIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	err := newPlugin(t, url).Execute(context.Background(), payload(`{"workflow":"test","inputs":{"prompt":"x"}}`), newIO())
	var pe *plugin.PermanentError
	if err == nil || errors.As(err, &pe) {
		t.Fatalf("want retryable error, got %v", err)
	}
}

func TestCancelInterruptsPrompt(t *testing.T) {
	comfy := newFakeComfy(t)
	comfy.hang = true
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	err := newPlugin(t, comfy.srv.URL).Execute(ctx, payload(`{"workflow":"test","inputs":{"prompt":"x"}}`), newIO())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	comfy.mu.Lock()
	defer comfy.mu.Unlock()
	if comfy.interrupts != 1 {
		t.Errorf("interrupts = %d", comfy.interrupts)
	}
}

func TestParseGraphRejectsUIFormat(t *testing.T) {
	if _, err := ParseGraph([]byte(`{"last_node_id":5,"nodes":[]}`)); err == nil || !strings.Contains(err.Error(), "UI format") {
		t.Fatalf("err = %v", err)
	}
}

func TestManifestValidatesAgainstGraph(t *testing.T) {
	g, _ := ParseGraph([]byte(testGraph))
	if _, err := ParseManifest([]byte(`params: {x: {node: "99", input: text, type: text}}`), g); err == nil {
		t.Error("missing node accepted")
	}
	if _, err := ParseManifest([]byte(`params: {x: {node: "2", input: nope, type: text}}`), g); err == nil {
		t.Error("missing input accepted")
	}
	if _, err := ParseManifest([]byte(`params: {x: {node: "2", input: text, type: weird}}`), g); err == nil {
		t.Error("unknown type accepted")
	}
}

func TestSeedValidation(t *testing.T) {
	p := Param{Type: "seed"}
	if v, err := p.coerce(json.RawMessage(`18446744073709551615`)); err != nil || fmt.Sprint(v) != "18446744073709551615" {
		t.Errorf("max seed: %v %v", v, err)
	}
	for _, bad := range []string{`-1`, `18446744073709551616`, `1.5`, `"7"`} {
		if _, err := p.coerce(json.RawMessage(bad)); err == nil {
			t.Errorf("seed %s accepted", bad)
		}
	}
}
