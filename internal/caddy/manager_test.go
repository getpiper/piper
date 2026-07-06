package caddy

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// freeAddr returns a currently-free 127.0.0.1:port.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	defer l.Close()
	return l.Addr().String()
}

// The base (LAN, non-WithHTTPS) config is plain HTTP: automatic HTTPS must be
// disabled so adding a host-matched route provisions no internal cert and
// stands up no :80 redirect server. Plan 1 is HTTP-only on :80.
func TestBaseConfigDisablesAutomaticHTTPSOnLAN(t *testing.T) {
	o := &managerOpts{httpListen: ":80"}
	base := o.baseConfig()

	srv := base["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["piper"].(map[string]any)
	ah, ok := srv["automatic_https"].(map[string]any)
	if !ok {
		t.Fatalf("base config should set automatic_https on the piper server, got %v", srv["automatic_https"])
	}
	if ah["disable"] != true {
		t.Fatalf("automatic_https.disable should be true on the LAN path, got %v", ah["disable"])
	}
}

// A host-matched route must apply on a non-:80 http port without Caddy trying
// to bind :80 for an auto-HTTPS redirect server — the exact case #40's test had
// to sidestep. With automatic HTTPS disabled the reload succeeds regardless of
// whether :80 is bindable.
func TestUpsertRouteOnNon80PortApplies(t *testing.T) {
	t.Setenv("PATH", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)

	admin := "http://" + freeAddr(t)
	httpListen := freeAddr(t) // non-:80
	m, err := StartManager(admin, httpListen)
	if err != nil {
		t.Fatalf("StartManager: %v", err)
	}
	defer m.Stop()

	c := NewClient(admin)
	if err := c.UpsertRoute("app.piper.localhost", 8080); err != nil {
		t.Fatalf("UpsertRoute on non-:80 port: %v", err)
	}
}

// StartManager must run Caddy in-process: with PATH emptied (so no external
// `caddy` binary is reachable) it still brings up a live admin API serving the
// base config we built.
func TestStartManagerRunsCaddyWithoutExternalBinary(t *testing.T) {
	// No external caddy anywhere on PATH; embedded Caddy must not care.
	t.Setenv("PATH", "")
	// Keep Caddy's autosave/storage out of the real home dir.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)

	admin := "http://" + freeAddr(t)
	httpListen := freeAddr(t)
	m, err := StartManager(admin, httpListen)
	if err != nil {
		t.Fatalf("StartManager: %v", err)
	}
	defer m.Stop()

	// The admin API is live and serving the config we built (piper server on
	// httpListen). This can only be true if Caddy is running in-process.
	resp, err := http.Get(admin + "/config/")
	if err != nil {
		t.Fatalf("GET admin /config/: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin /config/ status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), httpListen) {
		t.Fatalf("running config missing our http listener %q: %s", httpListen, body)
	}
}
