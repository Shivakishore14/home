package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func doReq(t *testing.T, h http.Handler, method, path, token string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestAuthDisabledIsOpen(t *testing.T) {
	srv, _, _, cleanup := setupTestServer(t)
	defer cleanup()
	srv.SetAuth(NewAuth(nil, nil))

	if rr := doReq(t, srv.Handler(), "GET", "/api/v1/stats", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("expected open API without tokens, got %d", rr.Code)
	}
}

func TestAuthRoles(t *testing.T) {
	srv, _, _, cleanup := setupTestServer(t)
	defer cleanup()
	srv.SetAuth(NewAuth([]string{"client-secret"}, []string{"worker-secret"}))
	h := srv.Handler()

	cases := []struct {
		name, method, path, token, body string
		want                            int
	}{
		{"healthz public", "GET", "/healthz", "", "", 200},
		{"login page public", "GET", "/login", "", "", 200},
		{"stats no token", "GET", "/api/v1/stats", "", "", 401},
		{"stats bad token", "GET", "/api/v1/stats", "nope", "", 401},
		{"stats worker token", "GET", "/api/v1/stats", "worker-secret", "", 401},
		{"stats client token", "GET", "/api/v1/stats", "client-secret", "", 200},
		{"create task worker", "POST", "/api/v1/tasks", "worker-secret", `{"plugin_name":"x"}`, 401},
		{"create task client", "POST", "/api/v1/tasks", "client-secret", `{"plugin_name":"x"}`, 201},
		{"claim no token", "POST", "/api/v1/tasks/claim", "", `{"worker_id":"w","plugins":["x"]}`, 401},
		{"claim worker", "POST", "/api/v1/tasks/claim", "worker-secret", `{"worker_id":"w","plugins":["none"]}`, 204},
		{"register worker", "POST", "/api/v1/workers/register", "worker-secret", `{"worker_id":"w"}`, 200},
		{"events no token", "GET", "/api/v1/events", "", "", 401},
	}
	for _, c := range cases {
		if rr := doReq(t, h, c.method, c.path, c.token, c.body); rr.Code != c.want {
			t.Errorf("%s: got %d want %d (%s)", c.name, rr.Code, c.want, rr.Body.String())
		}
	}
}

func TestDashboardRedirectAndCookieLogin(t *testing.T) {
	srv, _, _, cleanup := setupTestServer(t)
	defer cleanup()
	srv.SetAuth(NewAuth([]string{"client-secret"}, nil))
	h := srv.Handler()

	rr := doReq(t, h, "GET", "/", "", "")
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("expected redirect to /login, got %d %q", rr.Code, rr.Header().Get("Location"))
	}

	// Wrong token
	form := url.Values{"token": {"wrong"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad login: got %d", rr.Code)
	}

	// Correct token -> cookie
	form = url.Values{"token": {"client-secret"}}
	req = httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("login: got %d", rr.Code)
	}
	cookies := rr.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure {
		t.Fatalf("expected one HttpOnly+Secure cookie, got %+v", cookies)
	}

	req = httptest.NewRequest("GET", "/api/v1/stats", nil)
	req.AddCookie(cookies[0])
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("cookie auth: got %d", rr.Code)
	}
}
