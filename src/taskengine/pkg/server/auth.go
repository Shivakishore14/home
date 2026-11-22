package server

import (
	"crypto/subtle"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// Access levels for routes.
type access int

const (
	accessPublic access = iota // no token needed
	accessWorker               // worker or client token
	accessClient               // client token only
)

type role int

const (
	roleNone role = iota
	roleWorker
	roleClient
)

const authCookieName = "te_token"

// Auth holds bearer tokens. With no tokens configured, auth is disabled and
// every request is treated as a client (preserves LAN-only deployments).
type Auth struct {
	clientTokens []string
	workerTokens []string
}

// AuthFromEnv reads tokens from the environment:
//   - TASKENGINE_API_KEY, TASKENGINE_CLIENT_TOKENS (comma separated): full access
//   - TASKENGINE_WORKER_TOKENS (comma separated): worker endpoints only
func AuthFromEnv() *Auth {
	a := &Auth{}
	a.clientTokens = append(a.clientTokens, splitTokens(os.Getenv("TASKENGINE_API_KEY"))...)
	a.clientTokens = append(a.clientTokens, splitTokens(os.Getenv("TASKENGINE_CLIENT_TOKENS"))...)
	a.workerTokens = splitTokens(os.Getenv("TASKENGINE_WORKER_TOKENS"))
	return a
}

// NewAuth builds an Auth from explicit token lists.
func NewAuth(clientTokens, workerTokens []string) *Auth {
	return &Auth{clientTokens: clientTokens, workerTokens: workerTokens}
}

func splitTokens(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Enabled reports whether any token is configured.
func (a *Auth) Enabled() bool {
	return a != nil && (len(a.clientTokens) > 0 || len(a.workerTokens) > 0)
}

func matchAny(token string, list []string) bool {
	ok := false
	for _, t := range list {
		if subtle.ConstantTimeCompare([]byte(token), []byte(t)) == 1 {
			ok = true
		}
	}
	return ok
}

func requestToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, found := strings.CutPrefix(h, "Bearer "); found {
			return strings.TrimSpace(after)
		}
	}
	if c, err := r.Cookie(authCookieName); err == nil {
		return c.Value
	}
	return ""
}

func (a *Auth) roleOf(r *http.Request) role {
	if !a.Enabled() {
		return roleClient
	}
	tok := requestToken(r)
	if tok == "" {
		return roleNone
	}
	if matchAny(tok, a.clientTokens) {
		return roleClient
	}
	if matchAny(tok, a.workerTokens) {
		return roleWorker
	}
	return roleNone
}

func (a *Auth) allowed(r *http.Request, level access) bool {
	switch level {
	case accessPublic:
		return true
	case accessWorker:
		return a.roleOf(r) >= roleWorker
	default:
		return a.roleOf(r) == roleClient
	}
}

// route registers a handler guarded by the given access level.
func (s *Server) route(pattern string, level access, h http.HandlerFunc) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.allowed(r, level) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="taskengine"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	})
}

var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>TaskEngine – Sign in</title>
<style>body{font-family:system-ui,sans-serif;background:#0f1115;color:#e6e6e6;display:grid;place-items:center;min-height:100vh;margin:0}
form{background:#181b22;padding:2rem;border-radius:12px;width:min(90vw,360px)}input,button{width:100%;box-sizing:border-box;padding:.7rem;margin-top:.8rem;border-radius:8px;border:1px solid #333;background:#0f1115;color:inherit;font-size:1rem}
button{background:#3b82f6;border:none;cursor:pointer}.err{color:#f87171}</style></head>
<body><form method="post" action="/login"><h2>TaskEngine</h2>
{{if .}}<p class="err">{{.}}</p>{{end}}
<input type="password" name="token" placeholder="Access token" autocomplete="current-password" autofocus required>
<button type="submit">Sign in</button></form></body></html>`))

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = loginTmpl.Execute(w, "")
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.FormValue("token"))
	if !s.auth.Enabled() || !matchAny(token, s.auth.clientTokens) {
		log.Printf("[Auth] failed login from %s", r.RemoteAddr)
		time.Sleep(500 * time.Millisecond) // slow down guessing
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = loginTmpl.Execute(w, "Invalid token")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((90 * 24 * time.Hour).Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: authCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
