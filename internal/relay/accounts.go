package relay

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Account is a relay tenant. One account owns many agents.
type Account struct {
	ID       string
	Username string
	Disabled bool
}

// deriveUsername turns an email into a DNS-safe label component: the local part,
// lowercased, with every rune outside [a-z0-9-] replaced by '-', trimmed of
// leading/trailing '-', and capped at 30 chars so the eventual
// "<hash>-<username>.<apex>" label stays under DNS's 63-char limit.
func deriveUsername(email string) string {
	local := email
	if i := strings.IndexByte(email, '@'); i >= 0 {
		local = email[:i]
	}
	local = strings.ToLower(local)
	var b strings.Builder
	for _, r := range local {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	u := strings.Trim(b.String(), "-")
	if u == "" {
		u = "user"
	}
	if len(u) > 30 {
		u = strings.Trim(u[:30], "-")
	}
	return u
}

// UpsertAccount returns the account for googleSub, creating it (with a unique
// derived username) on first sight. Idempotent by googleSub.
func (s *Store) UpsertAccount(googleSub, email string) (Account, error) {
	var acc Account
	var disabled int
	err := s.db.QueryRow(`SELECT id, username, disabled FROM accounts WHERE google_sub=?`, googleSub).
		Scan(&acc.ID, &acc.Username, &disabled)
	if err == nil {
		acc.Disabled = disabled != 0
		return acc, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Account{}, err
	}

	base := deriveUsername(email)
	id := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i := 1; ; i++ {
		username := base
		if i > 1 {
			username = base + "-" + strconv.Itoa(i)
		}
		_, err := s.db.Exec(
			`INSERT INTO accounts(id, google_sub, username, disabled, created_at) VALUES(?,?,?,0,?)`,
			id, googleSub, username, now)
		if err == nil {
			return Account{ID: id, Username: username}, nil
		}
		if isUniqueViolation(err) {
			// Another account already holds this username; try the next suffix.
			// (A racing insert of the same google_sub is vanishingly unlikely on a
			// single relay; the SELECT above handles the common re-login path.)
			continue
		}
		return Account{}, err
	}
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint failure.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ErrBadCredential is returned for an unknown account credential or one whose
// account has been disabled by the operator kill-switch.
var ErrBadCredential = errors.New("bad credential")

// MintAccountCredential issues a fresh random credential for accountID and stores
// only its hash. The plaintext is returned once, to the caller.
func (s *Store) MintAccountCredential(accountID string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	cred := hex.EncodeToString(raw)
	_, err := s.db.Exec(
		`INSERT INTO account_creds(token_hash, account_id, created_at) VALUES(?,?,?)`,
		hashToken(cred), accountID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	return cred, nil
}

// AuthenticateAccount resolves a plaintext credential to its Account. A disabled
// account is treated as unauthenticated (ErrBadCredential).
func (s *Store) AuthenticateAccount(cred string) (Account, error) {
	var acc Account
	var disabled int
	err := s.db.QueryRow(
		`SELECT a.id, a.username, a.disabled
		   FROM account_creds c JOIN accounts a ON a.id = c.account_id
		  WHERE c.token_hash = ?`, hashToken(cred)).
		Scan(&acc.ID, &acc.Username, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrBadCredential
	}
	if err != nil {
		return Account{}, err
	}
	if disabled != 0 {
		return Account{}, ErrBadCredential
	}
	acc.Disabled = false
	return acc, nil
}

// DisableAccount flips the kill-switch for an account by username. Its
// credentials stop authenticating and its agents stop connecting.
func (s *Store) DisableAccount(username string) error {
	res, err := s.db.Exec(`UPDATE accounts SET disabled=1 WHERE username=?`, username)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("no such account")
	}
	return nil
}
