package apiclient

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTokenOnlySentToServerHost(t *testing.T) {
	var gotServer, gotOther string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotServer = r.Header.Get("Authorization") }))
	defer server.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotOther = r.Header.Get("Authorization") }))
	defer other.Close()

	c := &http.Client{Transport: Transport(server.URL, "s3cret", nil)}
	for _, u := range []string{server.URL + "/api/v1/stats", other.URL + "/presigned?X-Amz-Signature=x"} {
		resp, err := c.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if gotServer != "Bearer s3cret" {
		t.Errorf("server got %q", gotServer)
	}
	if gotOther != "" {
		t.Errorf("token leaked to third party: %q", gotOther)
	}
}
