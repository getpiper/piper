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
	"regexp"
	"strings"
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
		return "public.getpiper.dev"
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
	for _, col := range []string{"account_id", "control_token", "custom_domain"} {
		if err := ensureAgentColumn(db, col); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate agents: %w", err)
		}
	}
	// One custom domain per relay, enforced structurally: closes the
	// SELECT-then-UPDATE FCFS race in SetCustomDomain. Partial so cleared
	// rows ('' or NULL) don't collide. Created here, not in schema.sql,
	// because the column itself is a migration.
	if _, err := db.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS agents_custom_domain_unique
		    ON agents(custom_domain) WHERE custom_domain != ''`); err != nil {
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

// SetControlToken stores the plaintext control-API bearer the box pushed for
// this enrollment. Plaintext by necessity: the relay must present it verbatim
// on forwarded control requests (see the control-stream routing design).
func (s *Store) SetControlToken(baseDomain, token string) error {
	res, err := s.db.Exec(`UPDATE agents SET control_token=? WHERE base_domain=?`, token, baseDomain)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrBadToken
	}
	return nil
}

// ControlToken returns the stored control bearer for baseDomain, "" if the box
// never provisioned one. Unknown agents are ErrBadToken.
func (s *Store) ControlToken(baseDomain string) (string, error) {
	var tok sql.NullString
	err := s.db.QueryRow(`SELECT control_token FROM agents WHERE base_domain=?`, baseDomain).Scan(&tok)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrBadToken
	}
	if err != nil {
		return "", err
	}
	return tok.String, nil
}

// ErrDomainTaken is returned when another agent already holds a custom domain.
var ErrDomainTaken = errors.New("domain already in use")

// ErrInvalidDomain is returned for a custom domain that is not a lowercase
// dotted DNS name.
var ErrInvalidDomain = errors.New("invalid domain")

// ErrDomainReserved is returned when a custom domain would collide with the
// relay apex or an enrolled agent's base domain.
var ErrDomainReserved = errors.New("domain conflicts with a relay-managed domain")

// customDomainRE accepts lowercase dotted DNS names, mirroring the agent-side
// check in internal/domain. Anything else — including uppercase, so
// case-games cannot dodge the overlap checks below — is rejected.
var customDomainRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z][a-z0-9-]*[a-z0-9]$`)

// dnsOverlap reports whether two DNS names are equal or one is a label-suffix
// of the other: "x.example.com" overlaps "example.com", "xexample.com" does not.
func dnsOverlap(a, b string) bool {
	return a == b || strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a)
}

// domainClaimable rejects a custom domain that would collide with the relay's
// own namespace: the apex (which also covers api.<apex> and every assigned
// hostname under it) or ANY enrolled agent's base domain, in either suffix
// direction. The set-domain op is authenticated but its Domain value is
// attacker-controlled on a compromised box; without this check an agent could
// splice another agent's SNI — or the relay control plane — to itself.
func (s *Store) domainClaimable(domain string) error {
	if dnsOverlap(domain, strings.ToLower(s.apexOrDefault())) {
		return ErrDomainReserved
	}
	rows, err := s.db.Query(`SELECT base_domain FROM agents`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var base string
		if err := rows.Scan(&base); err != nil {
			return err
		}
		if dnsOverlap(domain, strings.ToLower(base)) {
			return ErrDomainReserved
		}
	}
	return rows.Err()
}

// SetCustomDomain records domain as the BYO custom domain for the agent
// enrolled at baseDomain and returns the previous value. Empty domain clears.
// The domain must be a well-formed DNS name outside the relay's own namespace
// (ErrInvalidDomain / ErrDomainReserved) and is first-come-first-served
// across agents (ErrDomainTaken, backstopped by a unique index); the real
// ownership proof is the DNS-01 cert the box obtained before asking.
func (s *Store) SetCustomDomain(baseDomain, domain string) (string, error) {
	if domain != "" {
		if !customDomainRE.MatchString(domain) {
			return "", ErrInvalidDomain
		}
		if err := s.domainClaimable(domain); err != nil {
			return "", err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if domain != "" {
		var other string
		err := tx.QueryRow(
			`SELECT base_domain FROM agents WHERE custom_domain=? AND base_domain!=?`,
			domain, baseDomain).Scan(&other)
		if err == nil {
			return "", ErrDomainTaken
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	var prev sql.NullString
	err = tx.QueryRow(`SELECT custom_domain FROM agents WHERE base_domain=?`, baseDomain).Scan(&prev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrBadToken
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(`UPDATE agents SET custom_domain=? WHERE base_domain=?`, domain, baseDomain); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return "", ErrDomainTaken // index backstop: lost the FCFS race
		}
		return "", err
	}
	if err := tx.Commit(); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return "", ErrDomainTaken
		}
		return "", err
	}
	return prev.String, nil
}

// CustomDomain returns the agent's BYO custom domain, "" if none is set.
func (s *Store) CustomDomain(baseDomain string) (string, error) {
	var d sql.NullString
	err := s.db.QueryRow(`SELECT custom_domain FROM agents WHERE base_domain=?`, baseDomain).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrBadToken
	}
	if err != nil {
		return "", err
	}
	return d.String, nil
}

// ensureAgentColumn adds a column to agents if an older DB predates it.
// CREATE TABLE IF NOT EXISTS can't alter an existing table, so we add the
// column idempotently.
func ensureAgentColumn(db *sql.DB, column string) error {
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
		if name == column {
			return nil // already migrated
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE agents ADD COLUMN ` + column + ` TEXT`)
	return err
}
