package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// Runs only with R2_INTEGRATION=1 and R2_* credentials in the environment.
// Writes under tmp/ (bucket lifecycle deletes it after a day) and cleans up.
func TestR2Integration(t *testing.T) {
	if os.Getenv("R2_INTEGRATION") != "1" {
		t.Skip("set R2_INTEGRATION=1 with R2_* env to run")
	}
	cfg, ok := R2ConfigFromEnv()
	if !ok {
		t.Fatal("R2 env not set")
	}
	st, err := NewR2(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := fmt.Sprintf("tmp/integration/%d.txt", time.Now().UnixNano())
	body := []byte("hello taskengine")
	defer st.Delete(ctx, key)

	put := func(p *PresignedRequest, ct string, data []byte) int {
		req, _ := http.NewRequest(p.Method, p.URL, bytes.NewReader(data))
		for k, v := range p.Headers {
			req.Header.Set(k, v)
		}
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	p, err := st.PresignPut(ctx, key, "text/plain", int64(len(body)), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if code := put(p, "", append(body, 'X')); code < 400 {
		t.Errorf("oversized body accepted (%d): content-length not enforced", code)
	}
	if code := put(p, "image/png", body); code < 400 {
		t.Errorf("wrong content-type accepted (%d)", code)
	}
	if code := put(p, "", body); code != 200 {
		t.Fatalf("valid PUT failed: %d", code)
	}

	info, err := st.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(body)) || info.ContentType != "text/plain" {
		t.Errorf("head = %+v", info)
	}

	u, err := st.PresignGet(ctx, key, "x.txt", true, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(got, body) {
		t.Fatalf("GET %d %q", resp.StatusCode, got)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="x.txt"` {
		t.Errorf("content-disposition %q", cd)
	}

	if err := st.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Head(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
}
