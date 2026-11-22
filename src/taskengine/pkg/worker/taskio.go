package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"taskengine/pkg/models"
	"taskengine/pkg/plugin"
)

var _ plugin.TaskIO = (*httpProgressReporter)(nil)

func (r *httpProgressReporter) results() ([]string, map[string]interface{}) {
	r.ioMu.Lock()
	defer r.ioMu.Unlock()
	return append([]string(nil), r.fileIDs...), r.output
}

// SetOutput implements plugin.TaskIO.
func (r *httpProgressReporter) SetOutput(details map[string]interface{}) {
	r.ioMu.Lock()
	defer r.ioMu.Unlock()
	r.output = details
}

// UploadOutput implements plugin.TaskIO: presign via the server, then PUT the
// file straight to object storage.
func (r *httpProgressReporter) UploadOutput(ctx context.Context, localPath, filename, mime string, meta map[string]string) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if fi.Size() == 0 {
		return "", fmt.Errorf("output %s is empty", filename)
	}

	body, _ := json.Marshal(models.PresignOutputRequest{Filename: filename, MIME: mime, Size: fi.Size(), Meta: meta})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/tasks/%s/outputs", r.serverURL, r.taskID), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("presign output: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("presign output: %d %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var p models.PresignResponse
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return "", err
	}

	put, err := http.NewRequestWithContext(ctx, p.Method, p.URL, f)
	if err != nil {
		return "", err
	}
	put.ContentLength = fi.Size()
	for k, v := range p.Headers {
		put.Header.Set(k, v)
	}
	putResp, err := r.transferClient.Do(put)
	if err != nil {
		return "", fmt.Errorf("upload output: %w", err)
	}
	defer putResp.Body.Close()
	if putResp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(putResp.Body, 512))
		return "", fmt.Errorf("upload output: %d %s", putResp.StatusCode, strings.TrimSpace(string(msg)))
	}

	r.ioMu.Lock()
	r.fileIDs = append(r.fileIDs, p.FileID)
	r.ioMu.Unlock()
	return p.FileID, nil
}

// FetchAsset implements plugin.TaskIO.
func (r *httpProgressReporter) FetchAsset(ctx context.Context, relPath string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/files/%s", r.serverURL, strings.TrimPrefix(relPath, "/")), nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch asset %s: status %d", relPath, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}
