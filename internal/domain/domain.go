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
	"fmt"
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

// errStaleConfig aborts an issuance/renewal run whose config snapshot no
// longer matches the store (replaced or removed while the run was pending).
// The run must exit without side effects; the teardown/replacement wins.
var errStaleConfig = errors.New("domain config changed; aborting stale run")

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

	loopMu  sync.Mutex // guards loopGen
	loopGen int        // bumped per Set/Resume; a loop exits when superseded

	dnsMu    sync.Mutex // guards dnsCache
	dnsCache map[string]dnsCacheEntry

	envMu       sync.Mutex // guards the env-managed status fields below
	envStatus   string     // "" | issuing | active | failed (env mode only)
	envError    string
	envNotAfter time.Time

	// test seams
	retryDelay func(attempt int) time.Duration
	resolve    func(ctx context.Context, host string) ([]net.IP, error)
	now        func() time.Time
}

// dnsCacheEntry memoizes one domain's dnsOK result so a polling dashboard
// doesn't issue live DNS lookups on every GET/PUT /v1/domain (#114).
type dnsCacheEntry struct {
	ok bool
	at time.Time
}

// dnsOKTTL bounds how long a cached dnsOK result is served before a re-lookup.
const dnsOKTTL = 5 * time.Second

func New(o Options) *Manager {
	envStatus := ""
	if o.EnvDomain != "" {
		envStatus = StatusIssuing // pending until RunEnv reports its outcome
	}
	return &Manager{
		st: o.Store, newIssuer: o.Issuer, proxy: o.Proxy,
		dataDir: o.DataDir, relayHost: o.RelayHost,
		httpsListen: o.HTTPSListen, envDomain: o.EnvDomain,
		dnsCache:   map[string]dnsCacheEntry{},
		envStatus:  envStatus,
		retryDelay: defaultRetryDelay,
		resolve: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
		now: time.Now,
	}
}

// nextGen bumps the loop generation and returns it; the goroutine started with
// this value drives issuance until a later Set/Resume supersedes it.
func (m *Manager) nextGen() int {
	m.loopMu.Lock()
	defer m.loopMu.Unlock()
	m.loopGen++
	return m.loopGen
}

func (m *Manager) currentGen() int {
	m.loopMu.Lock()
	defer m.loopMu.Unlock()
	return m.loopGen
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
	// issueMu makes the replace atomic w.r.t. issuance: an in-flight run
	// either completes for the still-current old config (and is then torn
	// down here) or sees the new config and aborts. Acquiring it can block
	// for the duration of an in-flight ACME Obtain (minutes, bounded) — a
	// deliberate trade: correctness of the state machine over PUT latency.
	m.issueMu.Lock()
	// Replacing a different domain tears the old one down first.
	if prev, err := m.st.GetDomainConfig(); err == nil && prev.Domain != d {
		m.teardown(prev)
	}
	if err := m.st.SetDomainConfig(d, provider, token); err != nil {
		m.issueMu.Unlock()
		return Status{}, err
	}
	m.issueMu.Unlock()
	go m.issueLoop(d, m.nextGen())
	return m.Status()
}

// issueLoop drives one config to activation with capped-backoff retries. It
// exits when the stored config no longer matches domain (replaced or deleted),
// activation succeeds, or a newer Set/Resume supersedes this generation — the
// last guard stops a same-domain re-PUT from leaving a second loop retrying
// alongside this one (#112).
func (m *Manager) issueLoop(domain string, gen int) {
	for attempt := 0; ; attempt++ {
		if m.currentGen() != gen {
			return
		}
		dc, err := m.st.GetDomainConfig()
		if err != nil || dc.Domain != domain || dc.Status == StatusActive {
			return
		}
		if err := m.issueOnce(dc); err == nil {
			return
		} else if errors.Is(err, errStaleConfig) {
			return // replaced or removed; the successor owns the state now
		} else {
			_ = m.st.UpdateDomainStatus(dc.Domain, StatusFailed, err.Error(), time.Time{})
		}
		time.Sleep(m.retryDelay(attempt))
	}
}

// defaultRetryDelay backs off 5s, then 1m, 2m, 4m, … capped at ~1h. The short
// first retry keeps a restart-mid-issuance from flapping to "failed" for a full
// minute: the common cause is the tunnel not being connected yet at Resume, and
// the immediate re-attempt (cert already on disk) succeeds once it is (#113).
func defaultRetryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return 5 * time.Second
	}
	shift := attempt - 1
	if shift > 6 {
		shift = 6
	}
	return time.Minute << uint(shift)
}

// issueOnce obtains (or reuses) the cert, arms Caddy, then tells the relay.
// The disk-cert reuse keeps retries and restarts inside LE rate limits: a
// relay hiccup must not burn a fresh certificate. snap is the caller's config
// snapshot; the run re-reads under issueMu and aborts with errStaleConfig if
// the stored domain has moved on (Set-replace/Remove won the race).
func (m *Manager) issueOnce(snap store.DomainConfig) error {
	m.issueMu.Lock()
	defer m.issueMu.Unlock()
	dc, err := m.st.GetDomainConfig()
	if err != nil || dc.Domain != snap.Domain {
		return errStaleConfig
	}
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
		// Defense in depth: teardown paths all hold issueMu today, so the
		// config cannot have changed during Obtain — but re-check before any
		// side effect in case a future path mutates it without the lock.
		if cur, err := m.st.GetDomainConfig(); err != nil || cur.Domain != dc.Domain {
			return errStaleConfig
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
	return m.st.UpdateDomainStatus(dc.Domain, StatusActive, "", notAfter)
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
// The relay clear is best-effort here (Set-replace only): the subsequent
// set-domain push for the new domain makes the relay unregister the old one,
// so a missed clear self-heals. Caller must hold issueMu.
func (m *Manager) teardown(dc store.DomainConfig) {
	if r := m.notifier(); r != nil {
		_ = r.SetCustomDomain("")
	}
	m.removeRoutesAndCert(dc)
}

// removeRoutesAndCert drops the domain's Caddy routes and the on-disk cert.
func (m *Manager) removeRoutesAndCert(dc store.DomainConfig) {
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
		m.envMu.Lock()
		st := Status{
			Domain: m.envDomain, Source: "env",
			Status: m.envStatus, Error: m.envError,
			DNSRecords: m.dnsRecords(m.envDomain),
		}
		if !m.envNotAfter.IsZero() {
			t := m.envNotAfter
			st.CertNotAfter = &t
		}
		m.envMu.Unlock()
		return st, nil
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
	st.DNSOK = m.cachedDNSOK(dc.Domain)
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

// cachedDNSOK serves dnsOK from a short TTL cache so a dashboard polling
// GET /v1/domain (and PUT, which returns Status) doesn't trigger a pair of
// blocking DNS lookups every call. The lookup runs outside the lock (#114).
func (m *Manager) cachedDNSOK(domain string) bool {
	m.dnsMu.Lock()
	if e, ok := m.dnsCache[domain]; ok && m.now().Sub(e.at) < dnsOKTTL {
		m.dnsMu.Unlock()
		return e.ok
	}
	m.dnsMu.Unlock()

	ok := m.dnsOK(domain)

	m.dnsMu.Lock()
	m.dnsCache[domain] = dnsCacheEntry{ok: ok, at: m.now()}
	m.dnsMu.Unlock()
	return ok
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
// The relay clear must succeed before the row is deleted: once the config is
// gone nothing on the box would ever retry the clear, and the relay would
// splice the domain forever. A failed clear fails Remove with the config
// intact so the user can retry DELETE. Holding issueMu can block for an
// in-flight ACME Obtain (minutes, bounded) — see Set for the rationale.
func (m *Manager) Remove() error {
	if m.envDomain != "" {
		return ErrEnvManaged
	}
	m.issueMu.Lock()
	defer m.issueMu.Unlock()
	dc, err := m.st.GetDomainConfig()
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if r := m.notifier(); r != nil {
		if err := r.SetCustomDomain(""); err != nil {
			return fmt.Errorf("clear relay domain mapping: %w", err)
		}
	}
	m.removeRoutesAndCert(dc)
	return m.st.DeleteDomainConfig()
}
