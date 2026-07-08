// Package relay is the cloud-side SNI-passthrough tunnel server. It authenticates
// agents by per-agent token and routes public :443 traffic down the matching
// tunnel by SNI. It never decrypts traffic.
package relay

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

var ErrBadToken = errors.New("bad token")

type Agent struct {
	Name       string
	BaseDomain string
}

type Store struct {
	db        *sql.DB
	apex      string
	maxAgents int
	maxApps   int
}

// Configure sets the free-tier apex, the per-account agent cap (EnrollForAccount)
// and the per-account app cap (RegisterHostname). Safe to call once after Open.
func (s *Store) Configure(apex string, maxAgents, maxApps int) {
	s.apex = apex
	s.maxAgents = maxAgents
	s.maxApps = maxApps
}

func (s *Store) maxAppsOrDefault() int {
	if s.maxApps <= 0 {
		return 10
	}
	return s.maxApps
}

func (s *Store) apexOrDefault() string {
	if s.apex == "" {
		return "public.getpiper.co"
	}
	return s.apex
}

func (s *Store) maxAgentsOrDefault() int {
	if s.maxAgents <= 0 {
		return 3
	}
	return s.maxAgents
}

func Open(path string) (*Store, error) {
	// busy_timeout makes a second writer (e.g. an overlapping control API
	// request) wait for the lock instead of failing immediately with
	// SQLITE_BUSY.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := ensureAgentAccountColumn(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate agents: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// Enroll mints a random token for a new agent bound to baseDomain and stores
// only its hash. The plaintext token is returned once, to the operator.
func (s *Store) Enroll(name, baseDomain string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	_, err := s.db.Exec(
		`INSERT INTO agents(name, token_hash, base_domain, created_at) VALUES(?,?,?,?)`,
		name, hashToken(tok), baseDomain, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	return tok, nil
}

// Authenticate resolves a plaintext token to its Agent, or ErrBadToken. An agent
// whose owning account has been disabled is rejected as ErrBadToken.
func (s *Store) Authenticate(token string) (Agent, error) {
	var ag Agent
	var disabled sql.NullInt64
	err := s.db.QueryRow(
		`SELECT ag.name, ag.base_domain, acc.disabled
		   FROM agents ag LEFT JOIN accounts acc ON acc.id = ag.account_id
		  WHERE ag.token_hash = ?`, hashToken(token)).
		Scan(&ag.Name, &ag.BaseDomain, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrBadToken
	}
	if err != nil {
		return Agent{}, err
	}
	if disabled.Valid && disabled.Int64 != 0 {
		return Agent{}, ErrBadToken
	}
	return ag, nil
}

// ensureAgentAccountColumn adds agents.account_id if an older DB predates it.
// CREATE TABLE IF NOT EXISTS can't alter an existing table, so we add the column
// idempotently and tolerate the "duplicate column" error on already-migrated DBs.
func ensureAgentAccountColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(agents)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "account_id" {
			return nil // already migrated
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE agents ADD COLUMN account_id TEXT`)
	return err
}
