package server

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// jellyfinNotifier asks Jellyfin to rescan its libraries after media changes.
// Jellyfin's realtime monitor misses changes made over SMB / Docker Desktop
// bind mounts, so without this, replaced files keep pointing at deleted paths.
//
// Enabled when JELLYFIN_URL and JELLYFIN_API_KEY are set. Rescans are debounced
// so a burst of completions triggers a single scan.
type jellyfinNotifier struct {
	url, apiKey string
	client      *http.Client

	mu      sync.Mutex
	pending *time.Timer
}

const jellyfinDebounce = 30 * time.Second

func newJellyfinNotifierFromEnv() *jellyfinNotifier {
	url := strings.TrimRight(os.Getenv("JELLYFIN_URL"), "/")
	key := os.Getenv("JELLYFIN_API_KEY")
	if url == "" || key == "" {
		return nil
	}
	log.Printf("[TaskEngine Server] Jellyfin library refresh enabled (%s)", url)
	return &jellyfinNotifier{url: url, apiKey: key, client: &http.Client{Timeout: 15 * time.Second}}
}

// Schedule queues a library refresh, coalescing calls within the debounce window.
func (j *jellyfinNotifier) Schedule() {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.pending != nil {
		return
	}
	j.pending = time.AfterFunc(jellyfinDebounce, func() {
		j.mu.Lock()
		j.pending = nil
		j.mu.Unlock()
		j.refresh()
	})
}

func (j *jellyfinNotifier) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.url+"/Library/Refresh", nil)
	if err != nil {
		return
	}
	req.Header.Set("X-Emby-Token", j.apiKey)
	resp, err := j.client.Do(req)
	if err != nil {
		log.Printf("[Jellyfin] library refresh failed: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("[Jellyfin] library refresh returned %s", resp.Status)
		return
	}
	log.Printf("[Jellyfin] library refresh triggered")
}
