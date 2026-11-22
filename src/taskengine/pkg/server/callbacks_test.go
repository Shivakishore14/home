package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"taskengine/pkg/config"
	"taskengine/pkg/db"
	"taskengine/pkg/models"
)

const testCallbackSecret = "test-secret"

// receiver records callback requests and answers with the queued status codes (then 200).
type receiver struct {
	mu       sync.Mutex
	statuses []int
	got      []receivedCallback
}

type receivedCallback struct {
	header  http.Header
	body    []byte
	payload models.CallbackPayload
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var p models.CallbackPayload
	_ = json.Unmarshal(body, &p)
	rc.mu.Lock()
	rc.got = append(rc.got, receivedCallback{header: r.Header.Clone(), body: body, payload: p})
	code := http.StatusOK
	if len(rc.statuses) > 0 {
		code, rc.statuses = rc.statuses[0], rc.statuses[1:]
	}
	rc.mu.Unlock()
	w.WriteHeader(code)
}

func (rc *receiver) calls() []receivedCallback {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]receivedCallback(nil), rc.got...)
}

func setupCallbackServer(t *testing.T, secret string) (*Server, *db.DB, *receiver, string) {
	t.Helper()
	dir := t.TempDir()
	cfgYAML := `
server:
  heartbeat_timeout_seconds: 30
callbacks:
  allowed_hosts: ["127.0.0.1", "hooks.example.com"]
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

	srv := NewServer(cfg, database, nil)
	srv.SetCallbackSecret(secret)
	srv.callbacks.backoff = []time.Duration{0, 0} // retries due immediately, 3 attempts total

	rc := &receiver{}
	hook := httptest.NewServer(rc)
	t.Cleanup(hook.Close)
	return srv, database, rc, hook.URL + "/hooks/taskengine"
}

func createTaskWithCallback(t *testing.T, h http.Handler, callbackURL, ref string) (*httptest.ResponseRecorder, models.Task) {
	t.Helper()
	body := map[string]interface{}{"plugin_name": "comfyui", "params": map[string]string{"workflow": "x"}}
	if callbackURL != "" {
		body["callback"] = map[string]string{"url": callbackURL, "ref": ref}
	}
	b, _ := json.Marshal(body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/tasks", bytes.NewReader(b)))
	var task models.Task
	_ = json.Unmarshal(rr.Body.Bytes(), &task)
	return rr, task
}

func post(t *testing.T, h http.Handler, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", path, bytes.NewReader(b)))
	if rr.Code >= 300 {
		t.Fatalf("POST %s: %d %s", path, rr.Code, rr.Body.String())
	}
	return rr
}

func claim(t *testing.T, h http.Handler) {
	t.Helper()
	rr := post(t, h, "/api/v1/tasks/claim", map[string]interface{}{"worker_id": "w1", "plugins": []string{"comfyui"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("claim: expected a task, got %d", rr.Code)
	}
}

func getDetail(t *testing.T, h http.Handler, id string) models.TaskDetail {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/tasks/"+id, nil))
	var d models.TaskDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode task: %v (%s)", err, rr.Body.String())
	}
	return d
}

func TestCallbackOnCompletion(t *testing.T) {
	srv, _, rc, hookURL := setupCallbackServer(t, testCallbackSecret)
	h := srv.Handler()

	rr, task := createTaskWithCallback(t, h, hookURL, "job_42")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	claim(t, h)
	post(t, h, "/api/v1/tasks/"+task.ID+"/complete", map[string]interface{}{
		"output_details": map[string]interface{}{"workflow": "x", "outputs": []string{"a.png"}},
	})

	srv.deliverDueCallbacks(context.Background())

	calls := rc.calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 callback, got %d", len(calls))
	}
	c := calls[0]
	ts := c.header.Get(HeaderCallbackTimestamp)
	if want := SignCallback([]byte(testCallbackSecret), ts, c.body); c.header.Get(HeaderCallbackSignature) != want {
		t.Errorf("bad signature %q, want %q", c.header.Get(HeaderCallbackSignature), want)
	}
	p := c.payload
	if p.Event != models.EventTaskCompleted || p.Ref != "job_42" || p.TaskID != task.ID || p.Status != models.StatusCompleted {
		t.Errorf("unexpected payload: %+v", p)
	}
	if !strings.Contains(string(p.Output), "a.png") {
		t.Errorf("output missing from payload: %s", p.Output)
	}
	if c.header.Get(HeaderCallbackDelivery) != p.DeliveryID || p.DeliveryID == "" {
		t.Errorf("delivery id header %q vs body %q", c.header.Get(HeaderCallbackDelivery), p.DeliveryID)
	}

	d := getDetail(t, h, task.ID)
	if d.Callback == nil || d.Callback.Ref != "job_42" {
		t.Errorf("task callback not stored: %+v", d.Callback)
	}
	if len(d.Callbacks) != 1 || d.Callbacks[0].Status != models.CallbackDelivered || d.Callbacks[0].LastStatus != 200 {
		t.Errorf("delivery not recorded: %+v", d.Callbacks)
	}

	// Nothing left to send.
	srv.deliverDueCallbacks(context.Background())
	if n := len(rc.calls()); n != 1 {
		t.Errorf("expected no redelivery, got %d calls", n)
	}
}

func TestCallbackRetriesThenGivesUp(t *testing.T) {
	srv, _, rc, hookURL := setupCallbackServer(t, testCallbackSecret)
	h := srv.Handler()
	rc.statuses = []int{500, 503, 500}

	_, task := createTaskWithCallback(t, h, hookURL, "r")
	post(t, h, "/api/v1/tasks/"+task.ID+"/cancel", nil)

	for i := 0; i < 5; i++ {
		srv.deliverDueCallbacks(context.Background())
	}
	if n := len(rc.calls()); n != 3 {
		t.Fatalf("expected 3 attempts (1 + 2 retries), got %d", n)
	}
	cb := getDetail(t, h, task.ID).Callbacks
	if len(cb) != 1 || cb[0].Status != models.CallbackFailed || cb[0].Attempts != 3 || cb[0].LastStatus != 500 {
		t.Fatalf("expected FAILED after 3 attempts: %+v", cb)
	}
	if rc.calls()[0].payload.Event != models.EventTaskCancelled {
		t.Errorf("expected cancel event, got %s", rc.calls()[0].payload.Event)
	}

	// Redeliver puts it back in the queue.
	post(t, h, "/api/v1/callbacks/"+cb[0].ID+"/redeliver", nil)
	srv.deliverDueCallbacks(context.Background())
	if cb := getDetail(t, h, task.ID).Callbacks; cb[0].Status != models.CallbackDelivered {
		t.Errorf("expected DELIVERED after redeliver: %+v", cb)
	}
}

func TestCallbackRetryThenSuccess(t *testing.T) {
	srv, _, rc, hookURL := setupCallbackServer(t, testCallbackSecret)
	h := srv.Handler()
	rc.statuses = []int{502}

	_, task := createTaskWithCallback(t, h, hookURL, "r")
	post(t, h, "/api/v1/tasks/"+task.ID+"/cancel", nil)
	srv.deliverDueCallbacks(context.Background())
	srv.deliverDueCallbacks(context.Background())

	cb := getDetail(t, h, task.ID).Callbacks
	if len(rc.calls()) != 2 || cb[0].Status != models.CallbackDelivered || cb[0].Attempts != 2 {
		t.Fatalf("expected delivery on 2nd attempt: calls=%d %+v", len(rc.calls()), cb)
	}
}

func TestCallbackGoneStopsRetries(t *testing.T) {
	srv, _, rc, hookURL := setupCallbackServer(t, testCallbackSecret)
	h := srv.Handler()
	rc.statuses = []int{410}

	_, task := createTaskWithCallback(t, h, hookURL, "r")
	post(t, h, "/api/v1/tasks/"+task.ID+"/cancel", nil)
	srv.deliverDueCallbacks(context.Background())
	srv.deliverDueCallbacks(context.Background())

	if cb := getDetail(t, h, task.ID).Callbacks; len(rc.calls()) != 1 || cb[0].Status != models.CallbackFailed {
		t.Fatalf("410 should stop retries: calls=%d %+v", len(rc.calls()), cb)
	}
}

func TestCallbackOnlyOnFinalFailure(t *testing.T) {
	srv, _, rc, hookURL := setupCallbackServer(t, testCallbackSecret)
	h := srv.Handler()

	_, task := createTaskWithCallback(t, h, hookURL, "r")
	claim(t, h)
	// A retryable failure requeues the task: no callback yet.
	post(t, h, "/api/v1/tasks/"+task.ID+"/fail", map[string]interface{}{"error_message": "comfy down", "can_retry": true})
	srv.deliverDueCallbacks(context.Background())
	if n := len(rc.calls()); n != 0 {
		t.Fatalf("retryable failure must not call back, got %d", n)
	}

	claim(t, h)
	post(t, h, "/api/v1/tasks/"+task.ID+"/fail", map[string]interface{}{"error_message": "bad prompt", "can_retry": false})
	srv.deliverDueCallbacks(context.Background())
	calls := rc.calls()
	if len(calls) != 1 || calls[0].payload.Event != models.EventTaskFailed || calls[0].payload.Message != "bad prompt" {
		t.Fatalf("expected one task.failed callback, got %+v", calls)
	}
}

func TestCallbackOnStaleTaskTimeout(t *testing.T) {
	srv, database, rc, hookURL := setupCallbackServer(t, testCallbackSecret)
	h := srv.Handler()

	_, task := createTaskWithCallback(t, h, hookURL, "r")
	// Use up the retries, so a lost worker fails the task for good.
	claim(t, h)
	for i := 0; i < 3; i++ {
		post(t, h, "/api/v1/tasks/"+task.ID+"/fail", map[string]interface{}{"error_message": "x", "can_retry": true})
		claim(t, h)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := database.RecoverStaleTasks(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	srv.deliverDueCallbacks(context.Background())
	calls := rc.calls()
	if len(calls) != 1 || calls[0].payload.Event != models.EventTaskFailed {
		t.Fatalf("expected task.failed after timeout, got %d calls", len(calls))
	}
}

func TestTaskWithoutCallbackQueuesNothing(t *testing.T) {
	srv, _, rc, _ := setupCallbackServer(t, testCallbackSecret)
	h := srv.Handler()

	_, task := createTaskWithCallback(t, h, "", "")
	post(t, h, "/api/v1/tasks/"+task.ID+"/cancel", nil)
	srv.deliverDueCallbacks(context.Background())
	if len(rc.calls()) != 0 || len(getDetail(t, h, task.ID).Callbacks) != 0 {
		t.Fatal("task without callback must not queue one")
	}
}

func TestCallbackValidation(t *testing.T) {
	srv, _, _, hookURL := setupCallbackServer(t, testCallbackSecret)
	h := srv.Handler()

	cases := []struct {
		url  string
		code int
	}{
		{"https://hooks.example.com/x", http.StatusCreated},
		{hookURL, http.StatusCreated},                         // http allowed on loopback
		{"http://hooks.example.com/x", http.StatusBadRequest}, // http elsewhere
		{"https://evil.example.com/x", http.StatusBadRequest}, // not allowlisted
		{"https://u:p@hooks.example.com/x", http.StatusBadRequest},
		{"ftp://hooks.example.com/x", http.StatusBadRequest},
		{"not a url", http.StatusBadRequest},
	}
	for _, tc := range cases {
		if rr, _ := createTaskWithCallback(t, h, tc.url, "r"); rr.Code != tc.code {
			t.Errorf("%s: got %d (%s), want %d", tc.url, rr.Code, strings.TrimSpace(rr.Body.String()), tc.code)
		}
	}
	if rr, _ := createTaskWithCallback(t, h, "https://hooks.example.com/x", strings.Repeat("r", 300)); rr.Code != http.StatusBadRequest {
		t.Errorf("long ref: got %d", rr.Code)
	}
}

func TestCallbackRefusedWithoutSecret(t *testing.T) {
	srv, _, _, hookURL := setupCallbackServer(t, "")
	if rr, _ := createTaskWithCallback(t, srv.Handler(), hookURL, "r"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without a secret, got %d", rr.Code)
	}
}
