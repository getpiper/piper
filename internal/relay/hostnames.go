package relay

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// appHostname derives the single-label public hostname for (account, app):
// "<hex>-<username>.<apex>", where <hex> is the first 8 hex chars of
// sha256(accountID + "/" + app). It truncates <username> so the whole first
// label stays within DNS's 63-char limit (the 8-char hash preserves
// uniqueness). Deterministic — the same (account, app) always maps to the same
// hostname.
func appHostname(accountID, app, username, apex string) string {
	sum := sha256.Sum256([]byte(accountID + "/" + app))
	h := hex.EncodeToString(sum[:])[:8]
	// first label is "<h>-<username>": budget 63 - len(h) - 1 for the username.
	if max := 63 - len(h) - 1; len(username) > max {
		username = username[:max]
	}
	return h + "-" + username + "." + apex
}

// AgentAccount resolves the account owning the agent whose base_domain is
// baseDomain. ErrBadToken if there is no such agent; ErrBadCredential if the
// owning account is disabled.
func (s *Store) AgentAccount(baseDomain string) (accountID, username string, err error) {
	var disabled sql.NullInt64
	err = s.db.QueryRow(
		`SELECT acc.id, acc.username, acc.disabled
		   FROM agents ag JOIN accounts acc ON acc.id = ag.account_id
		  WHERE ag.base_domain = ?`, baseDomain).
		Scan(&accountID, &username, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrBadToken
	}
	if err != nil {
		return "", "", err
	}
	if disabled.Valid && disabled.Int64 != 0 {
		return "", "", ErrBadCredential
	}
	return accountID, username, nil
}

// AgentDisabled reports whether the account owning the agent whose base_domain
// is baseDomain has been disabled by the operator kill-switch. It is the
// narrowest read the relay's per-session watchdog needs: a store failure or an
// unknown base domain comes back as an error (not a false), so the watchdog can
// evict only on an affirmative disabled=true and leave healthy sessions running
// on a transient read error. The LEFT JOIN mirrors Authenticate: an account-less
// legacy agent has nothing to disable and reads as not-disabled, not missing.
func (s *Store) AgentDisabled(baseDomain string) (bool, error) {
	var disabled sql.NullInt64
	err := s.db.QueryRow(
		`SELECT acc.disabled
		   FROM agents ag LEFT JOIN accounts acc ON acc.id = ag.account_id
		  WHERE ag.base_domain = ?`, baseDomain).Scan(&disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrBadToken
	}
	if err != nil {
		return false, err
	}
	return disabled.Valid && disabled.Int64 != 0, nil
}

// RegisterHostname assigns (idempotently) the public hostname for app on the
// account owning baseDomain, enforcing the per-account app cap. Returns the
// assigned "<app-hash>-<username>.<apex>".
func (s *Store) RegisterHostname(baseDomain, app string) (string, error) {
	accountID, username, err := s.AgentAccount(baseDomain)
	if err != nil {
		return "", err
	}

	var existing string
	err = s.db.QueryRow(`SELECT hostname FROM hostnames WHERE account_id=? AND app=?`, accountID, app).Scan(&existing)
	if err == nil {
		return existing, nil // idempotent
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM hostnames WHERE account_id=?`, accountID).Scan(&count); err != nil {
		return "", err
	}
	if count >= s.maxAppsOrDefault() {
		return "", ErrQuotaExceeded
	}

	hostname := appHostname(accountID, app, username, s.apexOrDefault())
	_, err = s.db.Exec(
		`INSERT INTO hostnames(hostname, account_id, app, created_at) VALUES(?,?,?,?)`,
		hostname, accountID, app, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	return hostname, nil
}

// DeregisterHostname removes the hostname row for the account owning baseDomain.
// A missing row is not an error.
func (s *Store) DeregisterHostname(baseDomain, hostname string) error {
	accountID, _, err := s.AgentAccount(baseDomain)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM hostnames WHERE account_id=? AND hostname=?`, accountID, hostname)
	return err
}
