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
