package relay

import (
	"database/sql"
	"errors"
	"time"
)

// ErrNoInstallation is returned when no GitHub App installation is on record
// for the requested installation id or account.
var ErrNoInstallation = errors.New("no github installation")

// LinkInstallation records a GitHub App installation against the account of the
// user who installed it (the webhook's sender). Target type and login are
// display metadata: an org-target install still links to the installing user.
//
// Idempotent by installation_id, because the OAuth redirect and the
// installation webhook race and either may land first.
func (s *Store) LinkInstallation(installationID, senderGithubID, targetType, targetLogin string) error {
	var accountID string
	err := s.db.QueryRow(`SELECT id FROM accounts WHERE github_id=?`, senderGithubID).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnknownAccount
	}
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO github_installations(installation_id, account_id, target_type, target_login, created_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(installation_id) DO UPDATE SET
		     account_id   = excluded.account_id,
		     target_type  = excluded.target_type,
		     target_login = excluded.target_login`,
		installationID, accountID, targetType, targetLogin,
		time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// UnlinkInstallation drops an installation, e.g. on installation.deleted.
func (s *Store) UnlinkInstallation(installationID string) error {
	_, err := s.db.Exec(`DELETE FROM github_installations WHERE installation_id=?`, installationID)
	return err
}

// AccountForInstallation resolves an installation to its owning account id.
func (s *Store) AccountForInstallation(installationID string) (string, error) {
	var id string
	err := s.db.QueryRow(
		`SELECT account_id FROM github_installations WHERE installation_id=?`,
		installationID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoInstallation
	}
	return id, err
}

// InstallationForAccount returns the installation an account's agents mint
// tokens through. The most recent one wins if an account somehow holds several.
func (s *Store) InstallationForAccount(accountID string) (string, error) {
	var id string
	err := s.db.QueryRow(
		`SELECT installation_id FROM github_installations
		  WHERE account_id=? ORDER BY created_at DESC LIMIT 1`, accountID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoInstallation
	}
	return id, err
}
