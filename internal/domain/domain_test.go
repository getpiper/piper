package domain

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/store"
)

// selfSignedPEM issues a throwaway wildcard cert so the fake issuer's output
// parses (activation reads NotAfter and hostname coverage from it).
func selfSignedPEM(t *testing.T, notAfter time.Time, domains ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domains[0]},
		DNSNames:     domains,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kb, _ := x509.MarshalECPrivateKey(key)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return certPEM, keyPEM
}

type fakeIssuer struct {
	mu       sync.Mutex
	calls    int
	failures int // fail the first N Obtain calls
	notAfter time.Time
}

func (f *fakeIssuer) Obtain(domains []string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failures {
		return nil, nil, errors.New("acme: boom")
	}
	na := f.notAfter
	if na.IsZero() {
		na = time.Now().Add(90 * 24 * time.Hour)
	}
	c, k := selfSignedPEMForObtain(na, domains)
	return c, k, nil
}

// selfSignedPEMForObtain is selfSignedPEM without *testing.T (Obtain has none).
func selfSignedPEMForObtain(notAfter time.Time, domains []string) ([]byte, []byte) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domains[0]},
		DNSNames:     domains,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kb, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	return certPEM, keyPEM
}

func (f *fakeIssuer) obtainCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeProxy struct {
	mu       sync.Mutex
	ensured  []string
	replaced int
	routes   map[string]int
	removed  []string
}

func (f *fakeProxy) EnsureHTTPS(listen string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = append(f.ensured, listen)
	return nil
}
func (f *fakeProxy) ReplaceCert(cert, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replaced++
	return nil
}
func (f *fakeProxy) UpsertRouteTLS(host string, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.routes == nil {
		f.routes = map[string]int{}
	}
	f.routes[host] = port
	return nil
}
func (f *fakeProxy) RemoveRoute(host string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, host)
	return nil
}
func (f *fakeProxy) route(host string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.routes[host]
	return p, ok
}

type fakeNotifier struct {
	mu   sync.Mutex
	got  []string
	fail error
}

func (f *fakeNotifier) SetCustomDomain(d string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.got = append(f.got, d)
	return nil
}
func (f *fakeNotifier) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) == 0 {
		return ""
	}
	return f.got[len(f.got)-1]
}

func newTestManager(t *testing.T, iss *fakeIssuer) (*Manager, *store.Store, *fakeProxy, *fakeNotifier, string) {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	proxy := &fakeProxy{}
	relay := &fakeNotifier{}
	m := New(Options{
		Store:  st,
		Issuer: func(provider, token string) (Issuer, error) { return iss, nil },
		Proxy:  proxy, DataDir: dataDir,
		RelayHost: "relay.example.net", HTTPSListen: ":8443",
	})
	m.SetRelay(relay)
	m.retryDelay = func(int) time.Duration { return time.Millisecond }
	return m, st, proxy, relay, dataDir
}

// waitStatus polls the store until the domain config reaches status (2s cap).
func waitStatus(t *testing.T, st *store.Store, status string) store.DomainConfig {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var dc store.DomainConfig
	var err error
	for time.Now().Before(deadline) {
		dc, err = st.GetDomainConfig()
		if err == nil && dc.Status == status {
			return dc
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("status never reached %q (last: %+v, err %v)", status, dc, err)
	return dc
}

func TestSetIssuesAndActivates(t *testing.T) {
	m, st, proxy, relay, dataDir := newTestManager(t, &fakeIssuer{})
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment("blog", "img", "ctr", 40001, "running", ""); err != nil {
		t.Fatal(err)
	}

	status, err := m.Set("Example.COM", "cloudflare", "cf-token")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if status.Domain != "example.com" || status.Status != StatusIssuing {
		t.Fatalf("Set returned %+v", status)
	}

	dc := waitStatus(t, st, StatusActive)
	if dc.CertNotAfter.IsZero() {
		t.Fatal("active config has zero cert_not_after")
	}
	if got := relay.last(); got != "example.com" {
		t.Fatalf("relay notified with %q", got)
	}
	if p, ok := proxy.route("blog.example.com"); !ok || p != 40001 {
		t.Fatalf("blog route = %d,%v; want 40001 on blog.example.com", p, ok)
	}
	proxy.mu.Lock()
	ensured, replaced := proxy.ensured, proxy.replaced
	proxy.mu.Unlock()
	if len(ensured) == 0 || ensured[0] != ":8443" || replaced == 0 {
		t.Fatalf("proxy calls: ensured=%v replaced=%d", ensured, replaced)
	}
	for _, f := range []string{"cert.pem", "key.pem"} {
		fi, err := os.Stat(filepath.Join(dataDir, "domain", f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s perms = %v, want 0600", f, fi.Mode().Perm())
		}
	}
}

func TestSetValidation(t *testing.T) {
	m, _, _, _, _ := newTestManager(t, &fakeIssuer{})
	if _, err := m.Set("not a domain", "cloudflare", "tok"); !errors.Is(err, ErrInvalidDomain) {
		t.Fatalf("bad domain: %v", err)
	}
	if _, err := m.Set("ok.example.com", "route53", "tok"); !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("bad provider: %v", err)
	}
	if _, err := m.Set("ok.example.com", "cloudflare", ""); !errors.Is(err, ErrTokenRequired) {
		t.Fatalf("empty token: %v", err)
	}
}

func TestIssueFailureRecordsFailedThenRetriesToActive(t *testing.T) {
	iss := &fakeIssuer{failures: 2}
	m, st, _, _, _ := newTestManager(t, iss)
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	dc := waitStatus(t, st, StatusActive)
	if dc.Error != "" {
		t.Fatalf("active config still carries error %q", dc.Error)
	}
	if iss.obtainCalls() < 3 {
		t.Fatalf("obtain calls = %d, want ≥3 (2 failures + success)", iss.obtainCalls())
	}
}

func TestRelayRejectionSurfacesAsFailed(t *testing.T) {
	iss := &fakeIssuer{}
	m, st, _, relay, _ := newTestManager(t, iss)
	relay.fail = errors.New("domain already in use")
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	dc := waitStatus(t, st, StatusFailed)
	if dc.Error != "domain already in use" {
		t.Fatalf("error = %q", dc.Error)
	}
	// Retries must reuse the disk cert instead of re-obtaining (rate limits).
	before := iss.obtainCalls()
	time.Sleep(50 * time.Millisecond)
	if after := iss.obtainCalls(); after != before {
		t.Fatalf("obtain calls grew %d→%d during relay-only retries", before, after)
	}
	// Unblock the relay; the loop must converge to active.
	relay.mu.Lock()
	relay.fail = nil
	relay.mu.Unlock()
	waitStatus(t, st, StatusActive)
}

func TestSetEnvManaged(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(Options{Store: st, Proxy: &fakeProxy{}, EnvDomain: "env.example.com",
		Issuer: func(string, string) (Issuer, error) { return nil, errors.New("unused") }})
	if _, err := m.Set("x.dev", "cloudflare", "tok"); !errors.Is(err, ErrEnvManaged) {
		t.Fatalf("Set on env-managed: %v", err)
	}
	if err := m.Remove(); !errors.Is(err, ErrEnvManaged) {
		t.Fatalf("Remove on env-managed: %v", err)
	}
}

func TestStatusDNSOK(t *testing.T) {
	m, st, _, _, _ := newTestManager(t, &fakeIssuer{})
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, StatusActive)

	// Probe and relay host resolve to the same address → dns_ok.
	m.resolve = func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "piper-probe.example.com", "relay.example.net":
			return []net.IP{net.ParseIP("203.0.113.7")}, nil
		}
		return nil, errors.New("nxdomain")
	}
	s, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !s.DNSOK {
		t.Fatal("want dns_ok=true when probe matches relay")
	}
	if s.DNSTokenSet != true || s.Domain != "example.com" || s.Source != "api" {
		t.Fatalf("status = %+v", s)
	}
	want := []DNSRecord{
		{Type: "CNAME", Name: "*.example.com", Value: "relay.example.net"},
		{Type: "CNAME", Name: "example.com", Value: "relay.example.net"},
	}
	if len(s.DNSRecords) != 2 || s.DNSRecords[0] != want[0] || s.DNSRecords[1] != want[1] {
		t.Fatalf("dns_records = %+v", s.DNSRecords)
	}

	// Probe resolving elsewhere → not ok.
	m.resolve = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "piper-probe.example.com" {
			return []net.IP{net.ParseIP("198.51.100.9")}, nil
		}
		return []net.IP{net.ParseIP("203.0.113.7")}, nil
	}
	s, _ = m.Status()
	if s.DNSOK {
		t.Fatal("want dns_ok=false on mismatch")
	}
}

func TestStatusUnconfigured(t *testing.T) {
	m, _, _, _, _ := newTestManager(t, &fakeIssuer{})
	s, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "" || s.Domain != "" || s.Source != "api" {
		t.Fatalf("unconfigured status = %+v", s)
	}
}

func TestRemoveTearsDown(t *testing.T) {
	m, st, proxy, relay, dataDir := newTestManager(t, &fakeIssuer{})
	if _, err := st.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment("blog", "img", "ctr", 40001, "running", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Set("example.com", "cloudflare", "tok"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, StatusActive)

	if err := m.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := relay.last(); got != "" {
		t.Fatalf("relay last notify = %q, want cleared", got)
	}
	proxy.mu.Lock()
	removed := append([]string(nil), proxy.removed...)
	proxy.mu.Unlock()
	found := false
	for _, h := range removed {
		found = found || h == "blog.example.com"
	}
	if !found {
		t.Fatalf("removed routes = %v, want blog.example.com", removed)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "domain")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cert dir survives Remove: %v", err)
	}
	if _, err := st.GetDomainConfig(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("config survives Remove: %v", err)
	}
	// Removing again is a no-op.
	if err := m.Remove(); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}
