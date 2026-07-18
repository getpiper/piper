package caddy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithHTTPSBaseConfig(t *testing.T) {
	o := &managerOpts{httpListen: ":80"}
	WithHTTPS(":443")(o)
	base := o.baseConfig()

	srv := base["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["piper"].(map[string]any)
	listens := srv["listen"].([]string)
	found := false
	for _, l := range listens {
		if l == ":443" {
			found = true
		}
	}
	if !found {
		t.Fatalf("piper server should listen on :443, got %v", listens)
	}
	if srv["automatic_https"] == nil {
		t.Fatal("automatic_https should be set (disabled) when TLS is enabled")
	}
	if _, ok := base["apps"].(map[string]any)["tls"]; !ok {
		t.Fatal("tls app should be present when TLS is enabled")
	}
	if srv["tls_connection_policies"] == nil {
		t.Fatal("tls_connection_policies should be set when TLS is enabled")
	}
}

func TestLoadCert(t *testing.T) {
	var gotPath, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	if err := c.LoadCert("CERTPEM", "KEYPEM"); err != nil {
		t.Fatalf("LoadCert: %v", err)
	}
	if gotPath != "/config/apps/tls/certificates/load_pem" {
		t.Fatalf("path = %q", gotPath)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(gotBody), &got); err != nil {
		t.Fatalf("body not a JSON object: %v (%s)", err, gotBody)
	}
	if got["certificate"] != "CERTPEM" || got["key"] != "KEYPEM" {
		t.Fatalf("bad load_pem body: %s", gotBody)
	}
}

func TestReplaceCerts(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := NewClient(ts.URL)
	pairs := []CertPair{
		{CertPEM: "CERT1", KeyPEM: "KEY1"},
		{CertPEM: "CERT2", KeyPEM: "KEY2"},
	}
	if err := c.ReplaceCerts(pairs); err != nil {
		t.Fatalf("ReplaceCerts: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Fatalf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/config/apps/tls/certificates/load_pem" {
		t.Fatalf("path = %q", gotPath)
	}
	var got []map[string]string
	if err := json.Unmarshal([]byte(gotBody), &got); err != nil {
		t.Fatalf("body not a JSON array: %v (%s)", err, gotBody)
	}
	if len(got) != 2 || got[0]["certificate"] != "CERT1" || got[0]["key"] != "KEY1" ||
		got[1]["certificate"] != "CERT2" || got[1]["key"] != "KEY2" {
		t.Fatalf("bad replacement body: %s", gotBody)
	}

	// The empty set is a valid replacement (last cert unloaded), sent as [].
	if err := c.ReplaceCerts(nil); err != nil {
		t.Fatalf("ReplaceCerts(nil): %v", err)
	}
	if gotBody != "[]" {
		t.Fatalf("empty replacement body = %s, want []", gotBody)
	}
}
