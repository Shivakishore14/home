package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"taskengine/pkg/models"
)

// CallbackSecretEnv holds the HMAC key used to sign callback requests.
// Without it, tasks with a callback are refused.
const CallbackSecretEnv = "TASKENGINE_CALLBACK_SECRET"

// Signature headers sent with every callback. The signature is
// hex(HMAC-SHA256(secret, timestamp + "." + body)), prefixed with "sha256=".
const (
	HeaderCallbackEvent     = "X-TaskEngine-Event"
	HeaderCallbackDelivery  = "X-TaskEngine-Delivery"
	HeaderCallbackTimestamp = "X-TaskEngine-Timestamp"
	HeaderCallbackSignature = "X-TaskEngine-Signature"
)

const (
	callbackPollInterval = 5 * time.Second
	callbackBatchSize    = 20
	maxCallbackURLLen    = 2048
	maxCallbackRefLen    = 256
)

// defaultCallbackBackoff is the wait before each retry; after the last one
// the callback is marked FAILED (~22h after the first attempt).
var defaultCallbackBackoff = []time.Duration{
	30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute,
	time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour,
}

var errCallbacksDisabled = errors.New("callbacks are not configured on this server (set " + CallbackSecretEnv + ")")

// callbackSender delivers queued task callbacks (the `callbacks` outbox table).
type callbackSender struct {
	secret  []byte
	client  *http.Client
	wake    chan struct{}
	backoff []time.Duration
}

func newCallbackSenderFromEnv() *callbackSender {
	secret := strings.TrimSpace(os.Getenv(CallbackSecretEnv))
	if secret != "" {
		log.Printf("[TaskEngine Server] Task callbacks enabled")
	}
	return &callbackSender{
		secret: []byte(secret),
		client: &http.Client{
			Timeout: 15 * time.Second,
			// Never follow redirects: they could lead off the allowlist.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		wake:    make(chan struct{}, 1),
		backoff: defaultCallbackBackoff,
	}
}

// SetCallbackSecret replaces the callback signing secret. Intended for tests and embedding.
func (s *Server) SetCallbackSecret(secret string) {
	s.callbacks.secret = []byte(secret)
}

// SignCallback returns the signature header value for a callback body.
func SignCallback(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// validateCallback checks a task's callback against the server policy.
func (s *Server) validateCallback(cb *models.TaskCallback) error {
	if cb == nil {
		return nil
	}
	if cb.URL == "" {
		return errors.New("callback.url is required")
	}
	if len(s.callbacks.secret) == 0 {
		return errCallbacksDisabled
	}
	if len(cb.URL) > maxCallbackURLLen {
		return errors.New("callback.url is too long")
	}
	if len(cb.Ref) > maxCallbackRefLen {
		return fmt.Errorf("callback.ref is longer than %d characters", maxCallbackRefLen)
	}
	return s.callbackURLAllowed(cb.URL)
}

// callbackURLAllowed requires https (http only for loopback) and a host on
// the callbacks.allowed_hosts list.
func (s *Server) callbackURLAllowed(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("callback.url %q is not a valid absolute URL", raw)
	}
	if u.User != nil {
		return errors.New("callback.url must not contain credentials")
	}
	host := strings.ToLower(u.Hostname())
	switch u.Scheme {
	case "https":
	case "http":
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("callback.url must use https")
		}
	default:
		return errors.New("callback.url must use https")
	}
	hostPort := strings.ToLower(u.Host)
	for _, allowed := range s.cfg.GetGlobal().Callbacks.AllowedHosts {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed != "" && (allowed == host || allowed == hostPort) {
			return nil
		}
	}
	return fmt.Errorf("callback host %q is not allowed (add it to callbacks.allowed_hosts in config.yaml)", host)
}

// kickCallbacks wakes the dispatcher so a fresh event goes out immediately.
func (s *Server) kickCallbacks() {
	select {
	case s.callbacks.wake <- struct{}{}:
	default:
	}
}

// startCallbackDispatcher sends due callbacks when woken and every few seconds
// (retries, and events queued by the stale-task sweeper).
func (s *Server) startCallbackDispatcher() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(callbackPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopChan:
				return
			case <-ticker.C:
			case <-s.callbacks.wake:
			}
			s.deliverDueCallbacks(context.Background())
		}
	}()
}

// deliverDueCallbacks sends every callback that is due, one at a time.
func (s *Server) deliverDueCallbacks(ctx context.Context) {
	for {
		due, err := s.db.DueCallbacks(ctx, time.Now(), callbackBatchSize)
		if err != nil {
			log.Printf("[Callbacks] could not load due callbacks: %v", err)
			return
		}
		for _, c := range due {
			select {
			case <-s.stopChan:
				return
			default:
			}
			s.deliverCallback(ctx, c)
		}
		if len(due) < callbackBatchSize {
			return
		}
	}
}

// deliverCallback makes one attempt and records the result.
func (s *Server) deliverCallback(ctx context.Context, c models.CallbackDelivery) {
	status, err := s.sendCallback(ctx, c)
	if err == nil {
		if dbErr := s.db.MarkCallbackDelivered(ctx, c.ID, status); dbErr != nil {
			log.Printf("[Callbacks] %s delivered but not recorded: %v", c.ID, dbErr)
		}
		s.Broadcast("callback_delivered", map[string]interface{}{"id": c.ID, "task_id": c.TaskID, "event": c.Event})
		return
	}

	var next *time.Time
	var perm *permanentCallbackError
	if !errors.As(err, &perm) && c.Attempts < len(s.callbacks.backoff) {
		t := time.Now().Add(s.callbacks.backoff[c.Attempts])
		next = &t
	}
	if dbErr := s.db.MarkCallbackAttemptFailed(ctx, c.ID, status, err.Error(), next); dbErr != nil {
		log.Printf("[Callbacks] could not record failed attempt for %s: %v", c.ID, dbErr)
	}
	if next == nil {
		log.Printf("[Callbacks] giving up on %s (%s for task %s): %v", c.ID, c.Event, c.TaskID, err)
		s.Broadcast("callback_failed", map[string]interface{}{"id": c.ID, "task_id": c.TaskID, "event": c.Event, "error": err.Error()})
	} else {
		log.Printf("[Callbacks] %s attempt %d failed, retrying at %s: %v", c.ID, c.Attempts+1, next.UTC().Format(time.RFC3339), err)
	}
}

// permanentCallbackError stops retries (receiver said 410 Gone, URL no longer allowed, task gone).
type permanentCallbackError struct{ msg string }

func (e *permanentCallbackError) Error() string { return e.msg }

// sendCallback POSTs the signed payload. It returns the HTTP status (0 if none).
func (s *Server) sendCallback(ctx context.Context, c models.CallbackDelivery) (int, error) {
	if len(s.callbacks.secret) == 0 {
		return 0, errCallbacksDisabled
	}
	if err := s.callbackURLAllowed(c.URL); err != nil {
		return 0, &permanentCallbackError{err.Error()}
	}
	body, err := s.callbackBody(ctx, c)
	if err != nil {
		return 0, err
	}

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return 0, &permanentCallbackError{err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "TaskEngine-Callback/1")
	req.Header.Set(HeaderCallbackEvent, c.Event)
	req.Header.Set(HeaderCallbackDelivery, c.ID)
	req.Header.Set(HeaderCallbackTimestamp, ts)
	req.Header.Set(HeaderCallbackSignature, SignCallback(s.callbacks.secret, ts, body))

	resp, err := s.callbacks.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resp.StatusCode, nil
	case resp.StatusCode == http.StatusGone:
		return resp.StatusCode, &permanentCallbackError{"receiver returned 410 Gone"}
	default:
		return resp.StatusCode, fmt.Errorf("receiver returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
}

// callbackBody builds the payload at send time, so file URLs are freshly presigned on every attempt.
func (s *Server) callbackBody(ctx context.Context, c models.CallbackDelivery) ([]byte, error) {
	task, err := s.db.GetTask(ctx, c.TaskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, &permanentCallbackError{"task no longer exists"}
	}
	files, err := s.taskFiles(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	if files == nil {
		files = []models.File{}
	}
	return json.Marshal(models.CallbackPayload{
		DeliveryID: c.ID,
		Event:      c.Event,
		TaskID:     task.ID,
		Ref:        c.Ref,
		PluginName: task.PluginName,
		Status:     task.Status,
		Message:    task.StatusMessage,
		Output:     task.Output,
		Files:      files,
		OccurredAt: c.CreatedAt,
	})
}

// handleRedeliverCallback re-queues a callback (e.g. after fixing the receiver).
func (s *Server) handleRedeliverCallback(w http.ResponseWriter, r *http.Request) {
	ok, err := s.db.RedeliverCallback(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "callback not found", http.StatusNotFound)
		return
	}
	s.kickCallbacks()
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}
