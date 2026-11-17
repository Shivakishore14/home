package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestJellyfinNotifierRefresh(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/Library/Refresh" || r.Header.Get("X-Emby-Token") != "k" {
			t.Errorf("unexpected request %s %s token=%q", r.Method, r.URL.Path, r.Header.Get("X-Emby-Token"))
		}
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	t.Setenv("JELLYFIN_URL", srv.URL+"/")
	t.Setenv("JELLYFIN_API_KEY", "k")
	j := newJellyfinNotifierFromEnv()
	if j == nil {
		t.Fatal("notifier not enabled")
	}
	j.refresh()
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("expected 1 refresh, got %d", hits)
	}

	t.Setenv("JELLYFIN_URL", "")
	if newJellyfinNotifierFromEnv() != nil {
		t.Fatal("notifier should be disabled without URL")
	}
	var nilJ *jellyfinNotifier
	nilJ.Schedule() // must not panic
}
