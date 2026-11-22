package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// MemoryStore is an in-memory Store served over a local HTTP server. It is
// meant for tests and local development; presigned URLs are single-use-free
// signed tokens that enforce method, content type, size and expiry like R2.
type MemoryStore struct {
	srv *httptest.Server

	mu      sync.Mutex
	objects map[string]memObject
	grants  map[string]memGrant
}

type memObject struct {
	data        []byte
	contentType string
}

type memGrant struct {
	method      string
	key         string
	contentType string
	size        int64
	expires     time.Time
	disposition string
}

// NewMemory starts an in-memory store. Call Close when done.
func NewMemory() *MemoryStore {
	m := &MemoryStore{objects: map[string]memObject{}, grants: map[string]memGrant{}}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	return m
}

// Close stops the HTTP server.
func (m *MemoryStore) Close() { m.srv.Close() }

// Put stores an object directly (test setup).
func (m *MemoryStore) Put(key, contentType string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memObject{data: data, contentType: contentType}
}

// Get returns an object's bytes (test assertions).
func (m *MemoryStore) Get(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	return o.data, ok
}

// Keys lists stored object keys.
func (m *MemoryStore) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.objects {
		keys = append(keys, k)
	}
	return keys
}

func (m *MemoryStore) grant(g memGrant) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	m.mu.Lock()
	m.grants[tok] = g
	m.mu.Unlock()
	return fmt.Sprintf("%s/o/%s?sig=%s", m.srv.URL, url.PathEscape(g.key), tok)
}

func (m *MemoryStore) PresignPut(_ context.Context, key, contentType string, size int64, ttl time.Duration) (*PresignedRequest, error) {
	u := m.grant(memGrant{method: http.MethodPut, key: key, contentType: contentType, size: size, expires: time.Now().Add(ttl)})
	return &PresignedRequest{Method: http.MethodPut, URL: u, Headers: map[string]string{"Content-Type": contentType}, ExpiresAt: time.Now().Add(ttl).UTC()}, nil
}

func (m *MemoryStore) PresignGet(_ context.Context, key, filename string, attachment bool, ttl time.Duration) (string, error) {
	disp := ""
	if filename != "" {
		disp = "inline"
		if attachment {
			disp = "attachment"
		}
		disp += fmt.Sprintf(`; filename="%s"`, filename)
	}
	return m.grant(memGrant{method: http.MethodGet, key: key, expires: time.Now().Add(ttl), disposition: disp}), nil
}

func (m *MemoryStore) Head(_ context.Context, key string) (*ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	return &ObjectInfo{Size: int64(len(o.data)), ContentType: o.contentType}, nil
}

func (m *MemoryStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *MemoryStore) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	g, ok := m.grants[r.URL.Query().Get("sig")]
	m.mu.Unlock()
	key, _ := url.PathUnescape(r.URL.Path[len("/o/"):])
	if !ok || g.key != key || g.method != r.Method || time.Now().After(g.expires) {
		http.Error(w, "SignatureDoesNotMatch", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodPut:
		if r.Header.Get("Content-Type") != g.contentType {
			http.Error(w, "SignatureDoesNotMatch: content-type", http.StatusForbidden)
			return
		}
		if g.size > 0 && r.ContentLength != g.size {
			http.Error(w, "SignatureDoesNotMatch: content-length", http.StatusForbidden)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.Put(key, g.contentType, data)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		m.mu.Lock()
		o, ok := m.objects[key]
		m.mu.Unlock()
		if !ok {
			http.Error(w, "NoSuchKey", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", o.contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
		if g.disposition != "" {
			w.Header().Set("Content-Disposition", g.disposition)
		}
		_, _ = w.Write(o.data)
	}
}
