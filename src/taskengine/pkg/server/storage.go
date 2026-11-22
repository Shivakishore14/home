package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"taskengine/pkg/config"
	"taskengine/pkg/models"
	"taskengine/pkg/storage"

	"github.com/google/uuid"
)

// File references inside task params look like {"upload": "<file id>"}.
// At claim time the server adds "url", "filename", "mime" and "size" so the
// worker can download the input directly from object storage.
const uploadRefKey = "upload"

var (
	safePluginName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	unsafeFileChar = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

// SetStore enables presigned object storage (nil disables it).
func (s *Server) SetStore(st storage.Store) { s.store = st }

// StorageEnabled reports whether object storage is configured.
func (s *Server) StorageEnabled() bool { return s.store != nil }

func (s *Server) storageRoutes() {
	s.route("POST /api/v1/uploads", accessClient, s.handlePresignUpload)
	s.route("POST /api/v1/tasks/{id}/outputs", accessWorker, s.handlePresignOutput)
	s.route("GET /api/v1/blobs/{id}", accessClient, s.handleGetBlob)
}

func sanitizeFilename(name string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	name = unsafeFileChar.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._")
	if name == "" {
		name = "file"
	}
	if len(name) > 100 {
		ext := path.Ext(name)
		if len(ext) > 10 {
			ext = ""
		}
		name = name[:100-len(ext)] + ext
	}
	return name
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) storagePolicy(plugin string) (config.StorageConfig, config.StorageNamespace, error) {
	if s.store == nil {
		return config.StorageConfig{}, config.StorageNamespace{}, errStorageDisabled
	}
	sc := s.cfg.GetGlobal().Storage
	ns, ok := sc.Namespace(plugin)
	if !ok || !safePluginName.MatchString(plugin) {
		return sc, ns, fmt.Errorf("storage is not enabled for plugin %q (add storage.namespaces.%s to config.yaml)", plugin, plugin)
	}
	return sc, ns, nil
}

var errStorageDisabled = errors.New("object storage is not configured on this server")

func storageErrStatus(err error) int {
	if errors.Is(err, errStorageDisabled) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadRequest
}

// handlePresignUpload: client asks for a URL to upload an input file.
func (s *Server) handlePresignUpload(w http.ResponseWriter, r *http.Request) {
	var req models.PresignUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid upload body", http.StatusBadRequest)
		return
	}
	sc, ns, err := s.storagePolicy(req.Plugin)
	if err != nil {
		http.Error(w, err.Error(), storageErrStatus(err))
		return
	}
	req.MIME = strings.ToLower(strings.TrimSpace(req.MIME))
	if req.MIME == "" || !ns.AllowsMIME(req.MIME) {
		http.Error(w, fmt.Sprintf("mime type %q not allowed for %s (allowed: %v)", req.MIME, req.Plugin, ns.AllowedMIME), http.StatusBadRequest)
		return
	}
	maxBytes := int64(ns.MaxUploadMB) << 20
	if req.Size <= 0 || req.Size > maxBytes {
		http.Error(w, fmt.Sprintf("size must be between 1 and %d bytes", maxBytes), http.StatusBadRequest)
		return
	}

	putTTL, _, _ := sc.Durations()
	now := time.Now().UTC()
	id := uuid.NewString()
	f := &models.File{
		ID:        id,
		Plugin:    req.Plugin,
		Kind:      models.FileKindUpload,
		ObjectKey: fmt.Sprintf("%s/in/%s/%s/%s", req.Plugin, now.Format("2006-01-02"), id, sanitizeFilename(req.Filename)),
		Filename:  sanitizeFilename(req.Filename),
		MIME:      req.MIME,
		Size:      req.Size,
		Status:    models.FileStatusPending,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Duration(ns.RetentionDays) * 24 * time.Hour),
	}
	s.presignAndRespond(w, r, f, putTTL)
}

// handlePresignOutput: worker asks for a URL to upload a task output.
func (s *Server) handlePresignOutput(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	var req models.PresignOutputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid output body", http.StatusBadRequest)
		return
	}
	task, err := s.db.GetTask(r.Context(), taskID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if task == nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	if task.Status != models.StatusRunning {
		http.Error(w, "task is not running", http.StatusConflict)
		return
	}
	sc, ns, err := s.storagePolicy(task.PluginName)
	if err != nil {
		http.Error(w, err.Error(), storageErrStatus(err))
		return
	}
	req.MIME = strings.ToLower(strings.TrimSpace(req.MIME))
	if req.MIME == "" {
		req.MIME = "application/octet-stream"
	}
	maxBytes := int64(ns.MaxOutputMB) << 20
	if req.Size <= 0 || req.Size > maxBytes {
		http.Error(w, fmt.Sprintf("size must be between 1 and %d bytes", maxBytes), http.StatusBadRequest)
		return
	}

	putTTL, _, _ := sc.Durations()
	now := time.Now().UTC()
	id := uuid.NewString()
	name := sanitizeFilename(req.Filename)
	f := &models.File{
		ID:        id,
		Plugin:    task.PluginName,
		TaskID:    task.ID,
		Kind:      models.FileKindOutput,
		ObjectKey: fmt.Sprintf("%s/out/%s/%s-%s", task.PluginName, task.ID, id[:8], name),
		Filename:  name,
		MIME:      req.MIME,
		Size:      req.Size,
		Status:    models.FileStatusPending,
		Meta:      req.Meta,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Duration(ns.RetentionDays) * 24 * time.Hour),
	}
	s.presignAndRespond(w, r, f, putTTL)
}

func (s *Server) presignAndRespond(w http.ResponseWriter, r *http.Request, f *models.File, ttl time.Duration) {
	p, err := s.store.PresignPut(r.Context(), f.ObjectKey, f.MIME, f.Size, ttl)
	if err != nil {
		log.Printf("[Storage] presign put failed: %v", err)
		http.Error(w, "failed to presign upload", http.StatusBadGateway)
		return
	}
	if err := s.db.CreateFile(r.Context(), f); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, models.PresignResponse{
		FileID: f.ID, Method: p.Method, URL: p.URL, Headers: p.Headers, ExpiresAt: p.ExpiresAt,
	})
}

// handleGetBlob redirects to a short-lived presigned download URL.
func (s *Server) handleGetBlob(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, errStorageDisabled.Error(), http.StatusServiceUnavailable)
		return
	}
	f, err := s.db.GetFile(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if f == nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	if f.Status == models.FileStatusExpired || time.Now().After(f.ExpiresAt) {
		http.Error(w, "file expired", http.StatusGone)
		return
	}
	_, getTTL, _ := s.cfg.GetGlobal().Storage.Durations()
	u, err := s.store.PresignGet(r.Context(), f.ObjectKey, f.Filename, r.URL.Query().Get("download") == "1", getTTL)
	if err != nil {
		http.Error(w, "failed to presign download", http.StatusBadGateway)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	http.Redirect(w, r, u, http.StatusFound)
}

// decodeParams decodes JSON preserving numbers exactly (seeds can exceed 2^53).
func decodeParams(raw json.RawMessage) (interface{}, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// walkUploadRefs calls fn for every {"upload": "<id>"} object in v.
func walkUploadRefs(v interface{}, fn func(ref map[string]interface{}, id string) error) error {
	switch t := v.(type) {
	case map[string]interface{}:
		if id, ok := t[uploadRefKey].(string); ok {
			return fn(t, id)
		}
		for _, child := range t {
			if err := walkUploadRefs(child, fn); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, child := range t {
			if err := walkUploadRefs(child, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateUploadRefs checks every upload referenced by a new task exists in
// storage, belongs to the task's plugin and matches what was presigned.
func (s *Server) validateUploadRefs(ctx context.Context, pluginName string, params json.RawMessage) error {
	v, err := decodeParams(params)
	if err != nil {
		return fmt.Errorf("invalid params: %w", err)
	}
	return walkUploadRefs(v, func(_ map[string]interface{}, id string) error {
		if s.store == nil {
			return errStorageDisabled
		}
		f, err := s.db.GetFile(ctx, id)
		if err != nil {
			return err
		}
		if f == nil || f.Kind != models.FileKindUpload || f.Plugin != pluginName {
			return fmt.Errorf("upload %s not found for plugin %s", id, pluginName)
		}
		if f.Status == models.FileStatusExpired || time.Now().After(f.ExpiresAt) {
			return fmt.Errorf("upload %s has expired", id)
		}
		if f.Status == models.FileStatusReady {
			return nil
		}
		info, err := s.store.Head(ctx, f.ObjectKey)
		if errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("upload %s has not been uploaded yet", id)
		}
		if err != nil {
			return fmt.Errorf("checking upload %s: %w", id, err)
		}
		if info.Size != f.Size {
			_ = s.store.Delete(ctx, f.ObjectKey)
			return fmt.Errorf("upload %s size mismatch (%d != %d)", id, info.Size, f.Size)
		}
		return s.db.MarkFileReady(ctx, id, info.Size)
	})
}

// injectInputURLs adds presigned download URLs to upload refs in a claimed
// task's payload. URLs are minted at claim time so queued tasks never carry
// stale links.
func (s *Server) injectInputURLs(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	if s.store == nil {
		return payload, nil
	}
	v, err := decodeParams(payload)
	if err != nil || v == nil {
		return payload, nil
	}
	_, _, workerTTL := s.cfg.GetGlobal().Storage.Durations()
	found := false
	err = walkUploadRefs(v, func(ref map[string]interface{}, id string) error {
		f, err := s.db.GetFile(ctx, id)
		if err != nil {
			return err
		}
		if f == nil || f.Status == models.FileStatusExpired {
			return fmt.Errorf("input %s is missing or expired", id)
		}
		u, err := s.store.PresignGet(ctx, f.ObjectKey, "", false, workerTTL)
		if err != nil {
			return err
		}
		ref["url"], ref["filename"], ref["mime"], ref["size"] = u, f.Filename, f.MIME, f.Size
		found = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return payload, nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// confirmOutputs verifies uploaded outputs exist and marks them READY.
func (s *Server) confirmOutputs(ctx context.Context, taskID string, ids []string) {
	if s.store == nil {
		return
	}
	for _, id := range ids {
		f, err := s.db.GetFile(ctx, id)
		if err != nil || f == nil || f.TaskID != taskID || f.Kind != models.FileKindOutput {
			log.Printf("[Storage] task %s reported unknown output %s", taskID, id)
			continue
		}
		info, err := s.store.Head(ctx, f.ObjectKey)
		if err != nil {
			log.Printf("[Storage] task %s output %s not found in storage: %v", taskID, id, err)
			continue
		}
		_ = s.db.MarkFileReady(ctx, id, info.Size)
	}
}

// taskFiles lists a task's outputs with presigned GET URLs for ready files.
func (s *Server) taskFiles(ctx context.Context, taskID string) ([]models.File, error) {
	files, err := s.db.ListTaskFiles(ctx, taskID)
	if err != nil || s.store == nil {
		return files, err
	}
	_, getTTL, _ := s.cfg.GetGlobal().Storage.Durations()
	now := time.Now()
	for i := range files {
		f := &files[i]
		if f.Status == models.FileStatusReady && now.After(f.ExpiresAt) {
			f.Status = models.FileStatusExpired
		}
		if f.Status != models.FileStatusReady {
			continue
		}
		if u, err := s.store.PresignGet(ctx, f.ObjectKey, f.Filename, false, getTTL); err == nil {
			f.URL = u
		}
	}
	return files, nil
}

// expireFiles marks files past retention (and abandoned uploads) as expired.
// The objects themselves are removed by the bucket's lifecycle rules.
func (s *Server) expireFiles(ctx context.Context) {
	n, err := s.db.ExpireFiles(ctx, time.Now(), 24*time.Hour)
	if err != nil {
		log.Printf("[Storage] expiry sweep failed: %v", err)
	} else if n > 0 {
		log.Printf("[Storage] marked %d files expired", n)
	}
}
