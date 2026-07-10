package domain

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/getpiper/piper/internal/certs"
	"github.com/getpiper/piper/internal/store"
)

const (
	renewInterval = 12 * time.Hour
	renewWindow   = 30 * 24 * time.Hour
)

// Resume restores state after a restart: an active config re-arms from the
// disk cert (no re-issuance); issuing/failed configs — or an active one whose
// disk cert is damaged — re-enter the issue loop. The relay is not re-notified:
// it re-derives the mapping from its own store at session registration.
func (m *Manager) Resume() {
	if m.envDomain != "" {
		return
	}
	dc, err := m.st.GetDomainConfig()
	if err != nil {
		return
	}
	if dc.Status == StatusActive {
		certPEM, keyPEM, err := m.readCert()
		if err == nil && certCovers(certPEM, dc.Domain, time.Now()) {
			if err := m.arm(dc, certPEM, keyPEM); err == nil {
				return
			}
		}
		// Damaged or missing disk cert: degrade to re-issuance, not a crash.
		_ = m.st.UpdateDomainStatus(dc.Domain, StatusIssuing, "", time.Time{})
	}
	go m.issueLoop(dc.Domain)
}

// StartRenewals renews the API-managed cert: every renewInterval, when the
// disk cert is within renewWindow of expiry, re-obtain and hot-swap. Blocks
// until ctx ends; run it in a goroutine.
func (m *Manager) StartRenewals(ctx context.Context) {
	t := time.NewTicker(renewInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.renewCheck(time.Now())
		}
	}
}

func (m *Manager) renewCheck(now time.Time) {
	dc, err := m.st.GetDomainConfig()
	if err != nil || dc.Status != StatusActive {
		return
	}
	certPEM, _, err := m.readCert()
	if err != nil {
		return
	}
	due, err := certs.NeedsRenewal(certPEM, renewWindow, now)
	if err != nil || !due {
		return
	}
	if err := m.reissue(dc); err != nil && !errors.Is(err, errStaleConfig) {
		// Old cert keeps serving until expiry; surface the error, stay active.
		_ = m.st.UpdateDomainStatus(dc.Domain, StatusActive, "renew: "+err.Error(), dc.CertNotAfter)
	}
}

// reissue obtains a fresh cert for an already-active config and swaps it in.
// Like issueOnce, it re-validates the snapshot under issueMu and aborts with
// errStaleConfig when the config was replaced or removed meanwhile.
func (m *Manager) reissue(snap store.DomainConfig) error {
	m.issueMu.Lock()
	defer m.issueMu.Unlock()
	dc, err := m.st.GetDomainConfig()
	if err != nil || dc.Domain != snap.Domain {
		return errStaleConfig
	}
	iss, err := m.newIssuer(dc.DNSProvider, dc.DNSToken)
	if err != nil {
		return err
	}
	certPEM, keyPEM, err := iss.Obtain([]string{"*." + dc.Domain, dc.Domain})
	if err != nil {
		return err
	}
	// Defense in depth (see issueOnce): writing the cert for a torn-down
	// config would resurrect its deleted cert dir.
	if cur, err := m.st.GetDomainConfig(); err != nil || cur.Domain != dc.Domain {
		return errStaleConfig
	}
	if err := m.writeCert(certPEM, keyPEM); err != nil {
		return err
	}
	if err := m.proxy.ReplaceCert(string(certPEM), string(keyPEM)); err != nil {
		return err
	}
	notAfter, err := certs.NotAfter(certPEM)
	if err != nil {
		return err
	}
	return m.st.UpdateDomainStatus(dc.Domain, StatusActive, "", notAfter)
}

// RunEnv drives the env-managed (PIPER_BASE_DOMAIN) BYO path: issue the
// wildcard cert for the env domain now, then renew in the background — the
// fold-in of piperd's former setupRelayTLS/renewLoop pair. The Caddy manager
// was started WithHTTPS in this mode, so no EnsureHTTPS is needed.
func (m *Manager) RunEnv(ctx context.Context, iss Issuer) error {
	certPEM, keyPEM, err := iss.Obtain([]string{"*." + m.envDomain, m.envDomain})
	if err != nil {
		return err
	}
	if err := m.proxy.ReplaceCert(string(certPEM), string(keyPEM)); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(renewInterval)
		defer t.Stop()
		m.runEnvRenew(ctx, iss, certPEM, t.C, time.Now)
	}()
	return nil
}

func (m *Manager) runEnvRenew(ctx context.Context, iss Issuer, certPEM []byte, ticks <-chan time.Time, now func() time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			due, err := certs.NeedsRenewal(certPEM, renewWindow, now())
			if err != nil || !due {
				continue
			}
			newCert, newKey, err := iss.Obtain([]string{"*." + m.envDomain, m.envDomain})
			if err != nil {
				log.Printf("domain: env renew: %v", err)
				continue
			}
			if err := m.proxy.ReplaceCert(string(newCert), string(newKey)); err != nil {
				log.Printf("domain: env renew load: %v", err)
				continue
			}
			certPEM = newCert
		}
	}
}
