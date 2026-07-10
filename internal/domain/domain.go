// Package domain manages the box's BYO custom domain: cert issuance via ACME
// DNS-01, live activation in Caddy, relay routing, renewal, and teardown. It
// orchestrates certs/caddy/tunnel through interfaces (the deploy pattern) so
// it unit-tests with fakes. See docs/superpowers/specs/2026-07-10-domain-config-api-design.md.
package domain

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/getpiper/piper/internal/certs"
	"github.com/getpiper/piper/internal/store"
)

const (
	StatusIssuing = "issuing"
	StatusActive  = "active"
	StatusFailed  = "failed"
)

var (
	ErrEnvManaged          = errors.New("domain config is env-managed (PIPER_BASE_DOMAIN); unset it to manage via the API")
	ErrInvalidDomain       = errors.New("invalid domain")
	ErrUnsupportedProvider = errors.New("unsupported dns provider")
	ErrTokenRequired       = errors.New("dns_token required")
)

// Issuer obtains one PEM cert/key covering domains. *certs.Manager satisfies it.
type Issuer interface {
	Obtain(domains []string) (certPEM, keyPEM []byte, err error)
}

// IssuerFactory builds an Issuer for one issuance run from the stored DNS
// creds. Construction registers the ACME account, so it is deferred to
// issuance time rather than Manager construction.
type IssuerFactory func(provider, token string) (Issuer, error)

// Proxy is the caddy slice the Manager drives. *caddy.Client satisfies it.
type Proxy interface {
	EnsureHTTPS(listen string) error
	ReplaceCert(certPEM, keyPEM string) error
	UpsertRouteTLS(host string, upstreamHostPort int) error
	RemoveRoute(host string) error
}

// RelayNotifier pushes the custom domain to the relay over the tunnel.
// *agent.TunnelClient satisfies it.
type RelayNotifier interface {
	SetCustomDomain(domain string) error
}

// Options wires a Manager. EnvDomain non-empty means domain config is
// env-managed (the pre-#102 PIPER_BASE_DOMAIN BYO path): API writes are
// rejected and Status reports source "env".
type Options struct {
	Store       *store.Store
	Issuer      IssuerFactory
	Proxy       Proxy
	DataDir     string
	RelayHost   string // host part of the relay address; the DNS-record target
	HTTPSListen string // e.g. ":443"
	EnvDomain   string
}

// Manager owns the custom-domain lifecycle: issuing → active/failed,
// persisted in the store so the dashboard can poll it and restarts resume it.
type Manager struct {
	st          *store.Store
	newIssuer   IssuerFactory
	proxy       Proxy
	dataDir     string
	relayHost   string
	httpsListen string
	envDomain   string

	relayMu sync.Mutex
	relay   RelayNotifier

	issueMu sync.Mutex // serializes issuance/renewal runs

	// test seams
	retryDelay func(attempt int) time.Duration
	resolve    func(ctx context.Context, host string) ([]net.IP, error)
}

func New(o Options) *Manager {
	return &Manager{
		st: o.Store, newIssuer: o.Issuer, proxy: o.Proxy,
		dataDir: o.DataDir, relayHost: o.RelayHost,
		httpsListen: o.HTTPSListen, envDomain: o.EnvDomain,
		retryDelay: defaultRetryDelay,
		resolve: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
	}
}

// SetRelay injects the tunnel client once relay mode is up (piperd creates the
// tunnel client after the Manager). Nil is tolerated: activation then skips
// the relay push (LAN/tests).
func (m *Manager) SetRelay(r RelayNotifier) {
	m.relayMu.Lock()
	m.relay = r
	m.relayMu.Unlock()
}

func (m *Manager) notifier() RelayNotifier {
	m.relayMu.Lock()
	defer m.relayMu.Unlock()
	return m.relay
}

// domainRE accepts lowercase dotted DNS names ("shop.example.com").
var domainRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z][a-z0-9-]*[a-z0-9]$`)

// Set validates and persists a new custom-domain config, then starts issuance
// asynchronously. The returned Status is the freshly-kicked "issuing" state.
func (m *Manager) Set(domainName, provider, token string) (Status, error) {
	if m.envDomain != "" {
		return Status{}, ErrEnvManaged
	}
	d := strings.ToLower(strings.TrimSpace(domainName))
	if !domainRE.MatchString(d) {
		return Status{}, ErrInvalidDomain
	}
	if provider != "cloudflare" {
		return Status{}, ErrUnsupportedProvider
	}
	if token == "" {
		return Status{}, ErrTokenRequired
	}
	// Replacing a different domain tears the old one down first.
	if prev, err := m.st.GetDomainConfig(); err == nil && prev.Domain != d {
		m.teardown(prev)
	}
	if err := m.st.SetDomainConfig(d, provider, token); err != nil {
		return Status{}, err
	}
	go m.issueLoop(d)
	return m.Status()
}

// issueLoop drives one config to activation with capped-backoff retries. It
// exits when the stored config no longer matches domain (replaced or deleted)
// or activation succeeds.
func (m *Manager) issueLoop(domain string) {
	for attempt := 0; ; attempt++ {
		dc, err := m.st.GetDomainConfig()
		if err != nil || dc.Domain != domain || dc.Status == StatusActive {
			return
		}
		if err := m.issueOnce(dc); err == nil {
			return
		} else {
			_ = m.st.UpdateDomainStatus(StatusFailed, err.Error(), time.Time{})
		}
		time.Sleep(m.retryDelay(attempt))
	}
}

// defaultRetryDelay backs off 1m, 2m, 4m, … capped at 1h.
func defaultRetryDelay(attempt int) time.Duration {
	if attempt > 6 {
		attempt = 6
	}
	return time.Minute << uint(attempt)
}

// issueOnce obtains (or reuses) the cert, arms Caddy, then tells the relay.
// The disk-cert reuse keeps retries and restarts inside LE rate limits: a
// relay hiccup must not burn a fresh certificate.
func (m *Manager) issueOnce(dc store.DomainConfig) error {
	m.issueMu.Lock()
	defer m.issueMu.Unlock()
	certPEM, keyPEM, err := m.readCert()
	if err != nil || !certCovers(certPEM, dc.Domain, time.Now()) {
		iss, err := m.newIssuer(dc.DNSProvider, dc.DNSToken)
		if err != nil {
			return err
		}
		certPEM, keyPEM, err = iss.Obtain([]string{"*." + dc.Domain, dc.Domain})
		if err != nil {
			return err
		}
		if err := m.writeCert(certPEM, keyPEM); err != nil {
			return err
		}
	}
	if err := m.arm(dc, certPEM, keyPEM); err != nil {
		return err
	}
	if r := m.notifier(); r != nil {
		if err := r.SetCustomDomain(dc.Domain); err != nil {
			return err
		}
	}
	notAfter, err := certs.NotAfter(certPEM)
	if err != nil {
		return err
	}
	return m.st.UpdateDomainStatus(StatusActive, "", notAfter)
}

// arm loads the cert and app routes into Caddy — the box must answer before
// the relay routes to it. Shared by first activation and restart resume.
func (m *Manager) arm(dc store.DomainConfig, certPEM, keyPEM []byte) error {
	if err := m.proxy.EnsureHTTPS(m.httpsListen); err != nil {
		return err
	}
	if err := m.proxy.ReplaceCert(string(certPEM), string(keyPEM)); err != nil {
		return err
	}
	apps, err := m.st.ListApps()
	if err != nil {
		return err
	}
	for _, a := range apps {
		dep, err := m.st.LatestRunning(a.Name)
		if err != nil {
			continue // never deployed or not running: nothing to route
		}
		if err := m.proxy.UpsertRouteTLS(a.Name+"."+dc.Domain, dep.HostPort); err != nil {
			return err
		}
	}
	return nil
}

// teardown reverses activation: relay mapping first, then routes, then files.
func (m *Manager) teardown(dc store.DomainConfig) {
	if r := m.notifier(); r != nil {
		_ = r.SetCustomDomain("")
	}
	if apps, err := m.st.ListApps(); err == nil {
		for _, a := range apps {
			_ = m.proxy.RemoveRoute(a.Name + "." + dc.Domain)
		}
	}
	_ = os.RemoveAll(filepath.Join(m.dataDir, "domain"))
}

func (m *Manager) certDir() string  { return filepath.Join(m.dataDir, "domain") }
func (m *Manager) certPath() string { return filepath.Join(m.certDir(), "cert.pem") }
func (m *Manager) keyPath() string  { return filepath.Join(m.certDir(), "key.pem") }

func (m *Manager) writeCert(certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(m.certDir(), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(m.certPath(), certPEM, 0o600); err != nil {
		return err
	}
	return os.WriteFile(m.keyPath(), keyPEM, 0o600)
}

func (m *Manager) readCert() (certPEM, keyPEM []byte, err error) {
	certPEM, err = os.ReadFile(m.certPath())
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = os.ReadFile(m.keyPath())
	if err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

// certCovers reports whether the leaf in certPEM is valid for hosts under
// domain and not expiring within 24h — the disk-cert reuse test.
func certCovers(certPEM []byte, domain string, now time.Time) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	if crt.VerifyHostname("piper-probe."+domain) != nil {
		return false
	}
	return now.Add(24 * time.Hour).Before(crt.NotAfter)
}

// Status assembles the wire state (GET /v1/domain). DNSOK is computed in
// dnsOK (added in Task 7).
func (m *Manager) Status() (Status, error) {
	if m.envDomain != "" {
		return Status{Domain: m.envDomain, Source: "env", Status: StatusActive, DNSRecords: m.dnsRecords(m.envDomain)}, nil
	}
	dc, err := m.st.GetDomainConfig()
	if errors.Is(err, store.ErrNotFound) {
		return Status{Source: "api", DNSRecords: []DNSRecord{}}, nil
	}
	if err != nil {
		return Status{}, err
	}
	st := Status{
		Domain: dc.Domain, DNSProvider: dc.DNSProvider,
		DNSTokenSet: dc.DNSToken != "", Source: "api",
		Status: dc.Status, Error: dc.Error,
		DNSRecords: m.dnsRecords(dc.Domain),
	}
	st.DNSOK = m.dnsOK(dc.Domain)
	if !dc.CertNotAfter.IsZero() {
		t := dc.CertNotAfter
		st.CertNotAfter = &t
	}
	return st, nil
}

// DNSRecord is one record the user must create at their DNS host.
type DNSRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Status is the wire state of the box's custom-domain config. The DNS token
// never appears; DNSTokenSet signals presence.
type Status struct {
	Domain       string      `json:"domain"`
	DNSProvider  string      `json:"dns_provider"`
	DNSTokenSet  bool        `json:"dns_token_set"`
	Source       string      `json:"source"` // "api" | "env"
	Status       string      `json:"status"` // "" | "issuing" | "active" | "failed"
	Error        string      `json:"error"`
	CertNotAfter *time.Time  `json:"cert_not_after,omitempty"`
	DNSRecords   []DNSRecord `json:"dns_records"`
	DNSOK        bool        `json:"dns_ok"`
}

func (m *Manager) dnsRecords(domain string) []DNSRecord {
	return []DNSRecord{
		{Type: "CNAME", Name: "*." + domain, Value: m.relayHost},
		{Type: "CNAME", Name: domain, Value: m.relayHost},
	}
}

// dnsOK reports whether a wildcard lookup under domain reaches the relay:
// piper-probe.<domain> (any label matches the user's wildcard record) must
// resolve to an address the relay host also resolves to. Traffic readiness —
// independent of issuance, which needs only the DNS API token.
func (m *Manager) dnsOK(domain string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	probe, err := m.resolve(ctx, "piper-probe."+domain)
	if err != nil {
		return false
	}
	relay, err := m.resolve(ctx, m.relayHost)
	if err != nil {
		return false
	}
	for _, p := range probe {
		for _, r := range relay {
			if p.Equal(r) {
				return true
			}
		}
	}
	return false
}

// Remove tears down the custom domain. Shared-domain URLs are untouched.
func (m *Manager) Remove() error {
	if m.envDomain != "" {
		return ErrEnvManaged
	}
	dc, err := m.st.GetDomainConfig()
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	m.teardown(dc)
	return m.st.DeleteDomainConfig()
}
