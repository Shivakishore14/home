package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskengine/pkg/config"
	"taskengine/pkg/db"
	"taskengine/pkg/models"
	"taskengine/pkg/storage"
)

func setupStorageServer(t *testing.T) (*Server, *db.DB, *storage.MemoryStore) {
	t.Helper()
	dir := t.TempDir()
	cfgYAML := `
server:
  heartbeat_timeout_seconds: 30
storage:
  namespaces:
    comfyui:
      retention_days: 10
      max_upload_mb: 1
      allowed_mime: ["image/*"]
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfgYAML), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	mem := storage.NewMemory()
	t.Cleanup(mem.Close)

	srv := NewServer(cfg, database, nil)
	srv.SetAuth(NewAuth([]string{"client"}, []string{"worker"}))
	srv.SetStore(mem)
	return srv, database, mem
}

func call(t *testing.T, h http.Handler, method, path, token string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	default:
		data, _ := json.Marshal(b)
		r = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func doPresigned(t *testing.T, p models.PresignResponse, data []byte) int {
	t.Helper()
	req, _ := http.NewRequest(p.Method, p.URL, bytes.NewReader(data))
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func decode[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %T: %v (%s)", v, err, rr.Body.String())
	}
	return v
}

func TestUploadPresignValidation(t *testing.T) {
	srv, _, _ := setupStorageServer(t)
	h := srv.Handler()

	cases := []struct {
		name string
		body models.PresignUploadRequest
		want int
	}{
		{"unknown namespace", models.PresignUploadRequest{Plugin: "video-transcoder", Filename: "a.png", MIME: "image/png", Size: 10}, 400},
		{"bad mime", models.PresignUploadRequest{Plugin: "comfyui", Filename: "a.exe", MIME: "application/x-msdownload", Size: 10}, 400},
		{"too big", models.PresignUploadRequest{Plugin: "comfyui", Filename: "a.png", MIME: "image/png", Size: 2 << 20}, 400},
		{"zero size", models.PresignUploadRequest{Plugin: "comfyui", Filename: "a.png", MIME: "image/png"}, 400},
		{"ok", models.PresignUploadRequest{Plugin: "comfyui", Filename: "../../etc/a b.png", MIME: "image/png", Size: 10}, 201},
	}
	for _, c := range cases {
		if rr := call(t, h, "POST", "/api/v1/uploads", "client", c.body); rr.Code != c.want {
			t.Errorf("%s: got %d want %d (%s)", c.name, rr.Code, c.want, rr.Body.String())
		}
	}
	if rr := call(t, h, "POST", "/api/v1/uploads", "worker", cases[4].body); rr.Code != 401 {
		t.Errorf("worker token must not presign uploads: %d", rr.Code)
	}

	srv.SetStore(nil)
	if rr := call(t, h, "POST", "/api/v1/uploads", "client", cases[4].body); rr.Code != http.StatusServiceUnavailable {
		t.Errorf("storage disabled: got %d", rr.Code)
	}
}

func TestStorageEndToEnd(t *testing.T) {
	srv, database, mem := setupStorageServer(t)
	h := srv.Handler()
	img := []byte("\x89PNG fake image bytes")

	// 1. Client presigns and uploads an input.
	rr := call(t, h, "POST", "/api/v1/uploads", "client", models.PresignUploadRequest{Plugin: "comfyui", Filename: "fox.png", MIME: "image/png", Size: int64(len(img))})
	if rr.Code != 201 {
		t.Fatalf("presign upload: %d %s", rr.Code, rr.Body.String())
	}
	up := decode[models.PresignResponse](t, rr)

	// Referencing it before the bytes are uploaded fails.
	params := fmt.Sprintf(`{"workflow":"x","inputs":{"image":{"upload":%q},"seed":18446744073709551615}}`, up.FileID)
	if rr := call(t, h, "POST", "/api/v1/tasks", "client", fmt.Sprintf(`{"plugin_name":"comfyui","params":%s}`, params)); rr.Code != 400 {
		t.Fatalf("task with missing upload: got %d", rr.Code)
	}
	// Wrong size is rejected by the store.
	if code := doPresigned(t, up, append(img, 'x')); code != 403 {
		t.Fatalf("oversize PUT: got %d", code)
	}
	if code := doPresigned(t, up, img); code != 200 {
		t.Fatalf("PUT: got %d", code)
	}

	// Upload belongs to comfyui; other plugins cannot reference it.
	if rr := call(t, h, "POST", "/api/v1/tasks", "client", fmt.Sprintf(`{"plugin_name":"command-runner","params":%s}`, params)); rr.Code != 400 {
		t.Fatalf("cross-plugin ref: got %d", rr.Code)
	}

	// 2. Create the task.
	rr = call(t, h, "POST", "/api/v1/tasks", "client", fmt.Sprintf(`{"plugin_name":"comfyui","params":%s}`, params))
	if rr.Code != 201 {
		t.Fatalf("create task: %d %s", rr.Code, rr.Body.String())
	}
	created := decode[models.Task](t, rr)

	// 3. Worker claims; payload carries a working download URL and exact seed.
	rr = call(t, h, "POST", "/api/v1/tasks/claim", "worker", models.ClaimTaskRequest{WorkerID: "w1", Plugins: []string{"comfyui"}})
	if rr.Code != 200 {
		t.Fatalf("claim: %d %s", rr.Code, rr.Body.String())
	}
	claimed := decode[models.Task](t, rr)
	if !strings.Contains(string(claimed.Payload), "18446744073709551615") {
		t.Errorf("seed precision lost: %s", claimed.Payload)
	}
	var payload struct {
		Inputs struct {
			Image struct {
				Upload, URL, Filename, MIME string
				Size                        int64
			}
		}
	}
	if err := json.Unmarshal(claimed.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	ref := payload.Inputs.Image
	if ref.URL == "" || ref.Filename != "fox.png" || ref.MIME != "image/png" || ref.Size != int64(len(img)) {
		t.Fatalf("ref not enriched: %+v", ref)
	}
	resp, err := http.Get(ref.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, img) {
		t.Fatalf("download mismatch")
	}
	// The stored payload itself never contains URLs.
	if stored, _ := database.GetTask(context.Background(), created.ID); strings.Contains(string(stored.Payload), "url") {
		t.Errorf("URL persisted in DB payload: %s", stored.Payload)
	}

	// 4. Worker uploads an output.
	out := []byte("generated image")
	if rr := call(t, h, "POST", "/api/v1/tasks/"+created.ID+"/outputs", "client", nil); rr.Code == 201 {
		t.Fatal("bad body accepted")
	}
	rr = call(t, h, "POST", "/api/v1/tasks/"+created.ID+"/outputs", "worker", models.PresignOutputRequest{Filename: "jeveration_00001_.png", MIME: "image/png", Size: int64(len(out)), Meta: map[string]string{"node": "5"}})
	if rr.Code != 201 {
		t.Fatalf("presign output: %d %s", rr.Code, rr.Body.String())
	}
	op := decode[models.PresignResponse](t, rr)
	if code := doPresigned(t, op, out); code != 200 {
		t.Fatalf("output PUT: %d", code)
	}
	keys := strings.Join(mem.Keys(), ",")
	if !strings.Contains(keys, "comfyui/out/"+created.ID+"/") || !strings.Contains(keys, "comfyui/in/") {
		t.Errorf("unexpected keys: %s", keys)
	}

	// 5. Complete with output details.
	rr = call(t, h, "POST", "/api/v1/tasks/"+created.ID+"/complete", "worker", models.CompleteTaskRequest{
		Message: "done", Files: []string{op.FileID}, OutputDetails: map[string]interface{}{"prompt_id": "p1"},
	})
	if rr.Code != 200 {
		t.Fatalf("complete: %d", rr.Code)
	}

	// 6. Client reads results.
	rr = call(t, h, "GET", "/api/v1/tasks/"+created.ID, "client", nil)
	detail := decode[models.TaskDetail](t, rr)
	if detail.Status != models.StatusCompleted || !strings.Contains(string(detail.Output), "p1") {
		t.Fatalf("task detail: %+v", detail.Task)
	}
	if len(detail.Files) != 1 || detail.Files[0].Status != models.FileStatusReady || detail.Files[0].URL == "" || detail.Files[0].Meta["node"] != "5" {
		t.Fatalf("files: %+v", detail.Files)
	}
	resp, err = http.Get(detail.Files[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, out) {
		t.Fatal("output download mismatch")
	}

	// Blob redirect.
	rr = call(t, h, "GET", "/api/v1/blobs/"+op.FileID+"?download=1", "client", nil)
	if rr.Code != http.StatusFound || rr.Header().Get("Location") == "" {
		t.Fatalf("blob redirect: %d", rr.Code)
	}

	// 7. Expiry sweep marks everything expired after retention.
	if n, err := database.ExpireFiles(context.Background(), time.Now().Add(11*24*time.Hour), 24*time.Hour); err != nil || n != 2 {
		t.Fatalf("expire: n=%d err=%v", n, err)
	}
	if rr := call(t, h, "GET", "/api/v1/blobs/"+op.FileID, "client", nil); rr.Code != http.StatusGone {
		t.Errorf("expired blob: got %d", rr.Code)
	}
}

func TestClaimFailsTaskWhenInputExpired(t *testing.T) {
	srv, database, _ := setupStorageServer(t)
	h := srv.Handler()
	img := []byte("img")

	up := decode[models.PresignResponse](t, call(t, h, "POST", "/api/v1/uploads", "client", models.PresignUploadRequest{Plugin: "comfyui", Filename: "a.png", MIME: "image/png", Size: 3}))
	doPresigned(t, up, img)
	rr := call(t, h, "POST", "/api/v1/tasks", "client", fmt.Sprintf(`{"plugin_name":"comfyui","params":{"image":{"upload":%q}}}`, up.FileID))
	task := decode[models.Task](t, rr)

	_, _ = database.ExpireFiles(context.Background(), time.Now().Add(30*24*time.Hour), time.Hour)

	if rr := call(t, h, "POST", "/api/v1/tasks/claim", "worker", models.ClaimTaskRequest{WorkerID: "w", Plugins: []string{"comfyui"}}); rr.Code != 204 {
		t.Fatalf("claim: %d", rr.Code)
	}
	got, _ := database.GetTask(context.Background(), task.ID)
	if got.Status != models.StatusFailed {
		t.Errorf("status %s", got.Status)
	}
}

func TestSanitizeFilename(t *testing.T) {
	for in, want := range map[string]string{
		"../../etc/passwd":   "passwd",
		`C:\Users\x\a b.png`: "a_b.png",
		"":                   "file",
		"..":                 "file",
		"ok-1_2.PNG":         "ok-1_2.PNG",
	} {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitize(%q) = %q want %q", in, got, want)
		}
	}
}
