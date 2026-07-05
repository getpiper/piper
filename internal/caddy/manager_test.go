package caddy

import (
	"context"
	"io"
	"net"
	"net/http"
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
// `caddy` binary is reachable) it still brings up a live admin API that the
// Client can drive.
func TestStartManagerRunsCaddyWithoutExternalBinary(t *testing.T) {
	// No external caddy anywhere on PATH; embedded Caddy must not care.
	t.Setenv("PATH", "")
	// Keep Caddy's autosave/storage out of the real home dir.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)

	admin := "http://" + freeAddr(t)
	m, err := StartManager(context.Background(), admin, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("StartManager: %v", err)
	}
	defer m.Stop()

	// Admin API is live.
	resp, err := http.Get(admin + "/config/")
	if err != nil {
		t.Fatalf("GET admin /config/: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin /config/ status = %d", resp.StatusCode)
	}

	// The Client can push a route, and Caddy accepts it (addressable by id).
	c := NewClient(admin)
	if err := c.UpsertRoute("blog.piper.localhost", 40001); err != nil {
		t.Fatalf("UpsertRoute: %v", err)
	}
	got, err := http.Get(admin + "/id/piper-blog.piper.localhost")
	if err != nil {
		t.Fatalf("GET route by id: %v", err)
	}
	io.Copy(io.Discard, got.Body)
	got.Body.Close()
	if got.StatusCode != http.StatusOK {
		t.Fatalf("route not found by id: status %d", got.StatusCode)
	}
}
