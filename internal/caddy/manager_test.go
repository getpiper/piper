package caddy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
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

// Caddy is a process-global singleton (caddy.Load/caddy.Stop act on it), so a
// second StartManager while one is live would silently clobber the first's
// config. StartManager must refuse it, and a fresh Start must succeed once the
// first is Stopped.
func TestStartManagerRefusesSecondWhileActive(t *testing.T) {
	t.Setenv("PATH", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)

	m, err := StartManager("http://"+freeAddr(t), freeAddr(t))
	if err != nil {
		t.Fatalf("first StartManager: %v", err)
	}

	if _, err := StartManager("http://"+freeAddr(t), freeAddr(t)); err == nil {
		m.Stop()
		t.Fatal("second StartManager while one is active should error, got nil")
	}

	m.Stop()

	// After Stop the invariant is cleared: a fresh Manager starts fine.
	m2, err := StartManager("http://"+freeAddr(t), freeAddr(t))
	if err != nil {
		t.Fatalf("StartManager after Stop: %v", err)
	}
	m2.Stop()
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

// EnsureHTTPS at runtime must leave :80-style plaintext serving intact while
// the new piper-tls server terminates TLS with a load_pem cert (coexistence).
func TestEnsureHTTPSServesTLSAlongsidePlaintext(t *testing.T) {
	t.Setenv("PATH", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)

	admin := "http://" + freeAddr(t)
	httpListen := freeAddr(t)
	httpsListen := freeAddr(t)

	m, err := StartManager(admin, httpListen)
	if err != nil {
		t.Fatalf("StartManager: %v", err)
	}
	defer m.Stop()

	c := NewClient(admin)
	if err := c.EnsureHTTPS(httpsListen); err != nil {
		t.Fatalf("EnsureHTTPS: %v", err)
	}
	// Idempotent second call.
	if err := c.EnsureHTTPS(httpsListen); err != nil {
		t.Fatalf("EnsureHTTPS twice: %v", err)
	}

	certPEM, keyPEM := selfSignedPEM(t, "shop.example.com")
	if err := c.ReplaceCert(string(certPEM), string(keyPEM)); err != nil {
		t.Fatalf("ReplaceCert: %v", err)
	}

	// Plaintext HTTP on the original listener still answers.
	resp, err := http.Get("http://" + httpListen + "/")
	if err != nil {
		t.Fatalf("plaintext GET after EnsureHTTPS: %v", err)
	}
	resp.Body.Close()

	// TLS handshake on the new listener serves the loaded cert.
	conn, err := tls.Dial("tcp", httpsListen, &tls.Config{
		ServerName: "blog.shop.example.com", InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("TLS dial piper-tls: %v", err)
	}
	conn.Close()
}

func selfSignedPEM(t *testing.T, base string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "*." + base},
		DNSNames:     []string{"*." + base, base},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kb, _ := x509.MarshalECPrivateKey(key)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return certPEM, keyPEM
}
