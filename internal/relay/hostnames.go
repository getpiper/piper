package relay

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// appHostname derives the single-label public hostname for (account, app, pr):
// "<hex>-<username>.<apex>" for production (pr 0), "pr<N>-<hex>-<username>.<apex>"
// for a PR preview, where <hex> is the first 8 hex chars of sha256 over the
// same triple. It truncates <username> so the whole first label stays within
// DNS's 63-char limit (the 8-char hash preserves uniqueness). Deterministic —
// the same (account, app, pr) always maps to the same hostname.
//
// Everything stays in ONE label: the relay serves a "*.<apex>" wildcard, which
// matches exactly one label, so a preview at "pr-<N>-<app>.<agent>.<apex>"
// would fail TLS outright (#302).
func appHostname(accountID, app, username, apex string, pr int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%d", accountID, app, pr)))
	h := hex.EncodeToString(sum[:])[:8]
	prefix := ""
	if pr > 0 {
		prefix = fmt.Sprintf("pr%d-", pr)
	}
	// first label is "<prefix><h>-<username>": the rest is the username budget.
	if max := 63 - len(prefix) - len(h) - 1; len(username) > max {
		username = username[:max]
	}
	return prefix + h + "-" + username + "." + apex
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
// narrowest read the relay's per-session watchdog needs, and it distinguishes
// three outcomes so the watchdog can tell transient from permanent:
//   - (false, nil)              the agent is live and enabled;
//   - (true, nil)               the owning account is disabled;
//   - (false, ErrUnknownAccount) there is no such agent row — the base is gone.
//
// A store failure comes back as its raw (transient) error, so the watchdog
// leaves healthy sessions running on a blip and evicts only on the two
// affirmative reads (disabled=true or ErrUnknownAccount). The LEFT JOIN mirrors
// Authenticate: an account-less agent (operator-enrolled via `piper-relay
// enroll`) still has an agents row, so its NULL acc.disabled reads as
// not-disabled — only a *missing agent row* is unknown.
func (s *Store) AgentDisabled(baseDomain string) (bool, error) {
	var disabled sql.NullInt64
	err := s.db.QueryRow(
		`SELECT acc.disabled
		   FROM agents ag LEFT JOIN accounts acc ON acc.id = ag.account_id
		  WHERE ag.base_domain = ?`, baseDomain).Scan(&disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrUnknownAccount
	}
	if err != nil {
		return false, err
	}
	return disabled.Valid && disabled.Int64 != 0, nil
}

// RegisterHostname assigns (idempotently) the public hostname for app on the
// account owning baseDomain, enforcing the per-account app cap. pr is 0 for the
// production host and the PR number for a preview, which gets a hostname of its
// own so it can never overwrite production's. Returns the assigned
// "<app-hash>-<username>.<apex>".
//
// Only production hosts count against the app cap: the cap bounds apps, and an
// open PR must not lock an account out of creating one.
func (s *Store) RegisterHostname(baseDomain, app string, pr int) (string, error) {
	accountID, username, err := s.AgentAccount(baseDomain)
	if err != nil {
		return "", err
	}

	var existing string
	err = s.db.QueryRow(`SELECT hostname FROM hostnames WHERE account_id=? AND app=? AND pr=?`, accountID, app, pr).Scan(&existing)
	if err == nil {
		return existing, nil // idempotent
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	if pr == 0 {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM hostnames WHERE account_id=? AND pr=0`, accountID).Scan(&count); err != nil {
			return "", err
		}
		if count >= s.maxAppsOrDefault() {
			return "", ErrQuotaExceeded
		}
	}

	hostname := appHostname(accountID, app, username, s.apexOrDefault(), pr)
	_, err = s.db.Exec(
		`INSERT INTO hostnames(hostname, account_id, app, pr, created_at) VALUES(?,?,?,?,?)`,
		hostname, accountID, app, pr, time.Now().UTC().Format(time.RFC3339Nano))
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
