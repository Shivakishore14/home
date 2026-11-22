// Package apiclient provides HTTP plumbing for talking to a TaskEngine server.
package apiclient

import (
	"net/http"
	"net/url"
	"os"
	"strings"
)

// TokenEnv is the environment variable holding the bearer token used by
// workers, producers, and CLI commands.
const TokenEnv = "TASKENGINE_TOKEN"

// TokenFromEnv returns the token from TASKENGINE_TOKEN (may be empty).
func TokenFromEnv() string { return strings.TrimSpace(os.Getenv(TokenEnv)) }

type authTransport struct {
	base  http.RoundTripper
	host  string
	token string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Only attach the token to the TaskEngine server itself, never to
	// third-party URLs (presigned storage URLs, ComfyUI, downloads...).
	if t.token != "" && req.URL.Host == t.host && req.Header.Get("Authorization") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(req)
}

// Transport wraps base (nil = http.DefaultTransport) so requests to serverURL
// carry the bearer token.
func Transport(serverURL, token string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	u, err := url.Parse(serverURL)
	if err != nil || token == "" {
		return base
	}
	return &authTransport{base: base, host: u.Host, token: token}
}
