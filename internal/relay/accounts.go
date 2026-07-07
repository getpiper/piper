package relay

import (
	"database/sql"
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
