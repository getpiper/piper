package relay

import (
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Org is one org membership from the caller's point of view.
type Org struct {
	ID   string
	Slug string
	Role string // the caller's role: "owner" | "member"
}

// ErrNoOrg is returned when an org doesn't exist or the caller is not a
// member — deliberately indistinguishable, so org existence never leaks.
var ErrNoOrg = errors.New("no such org")

// CreateOrg creates an org account (type='org', no GitHub identity, no
// credentials) with a slug derived from name — unique in the same username
// namespace user slugs live in, since both become DNS-label components — and
// makes the creator its sole owner.
func (s *Store) CreateOrg(creatorID, name string) (Org, error) {
	var ctype string
	err := s.db.QueryRow(`SELECT type FROM accounts WHERE id=?`, creatorID).Scan(&ctype)
	if errors.Is(err, sql.ErrNoRows) {
		return Org{}, ErrBadCredential
	}
	if err != nil {
		return Org{}, err
	}
	if ctype != "user" {
		return Org{}, errors.New("only user accounts create orgs")
	}

	base := deriveUsername(name)
	id := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.Begin()
	if err != nil {
		return Org{}, err
	}
	defer tx.Rollback()
	for i := 1; ; i++ {
		slug := base
		if i > 1 {
			slug = base + "-" + strconv.Itoa(i)
		}
		_, err := tx.Exec(
			`INSERT INTO accounts(id, github_id, github_login, username, type, disabled, created_at)
			 VALUES(?,NULL,NULL,?,'org',0,?)`, id, slug, now)
		if err == nil {
			if _, err := tx.Exec(
				`INSERT INTO org_members(org_id, account_id, role, created_at) VALUES(?,?,'owner',?)`,
				id, creatorID, now); err != nil {
				return Org{}, err
			}
			if err := tx.Commit(); err != nil {
				return Org{}, err
			}
			return Org{ID: id, Slug: slug, Role: "owner"}, nil
		}
		if isUniqueViolation(err) {
			continue // slug taken (user or org); try the next suffix
		}
		return Org{}, err
	}
}

// OrgsForAccount lists the orgs accountID belongs to, oldest membership first.
func (s *Store) OrgsForAccount(accountID string) ([]Org, error) {
	rows, err := s.db.Query(
		`SELECT o.id, o.username, m.role
		   FROM org_members m JOIN accounts o ON o.id = m.org_id
		  WHERE m.account_id = ? ORDER BY m.rowid`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orgs []Org
	for rows.Next() {
		var o Org
		if err := rows.Scan(&o.ID, &o.Slug, &o.Role); err != nil {
			return nil, err
		}
		orgs = append(orgs, o)
	}
	return orgs, rows.Err()
}

// OrgRole resolves slug to the org's account id and accountID's role in it.
// ErrNoOrg both when no such org exists and when the caller is not a member,
// so a non-member can't probe org existence.
func (s *Store) OrgRole(slug, accountID string) (orgID, role string, err error) {
	err = s.db.QueryRow(
		`SELECT o.id, m.role
		   FROM accounts o JOIN org_members m ON m.org_id = o.id AND m.account_id = ?
		  WHERE o.username = ? AND o.type = 'org'`, accountID, slug).
		Scan(&orgID, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNoOrg
	}
	if err != nil {
		return "", "", err
	}
	return orgID, role, nil
}

// Member is one row of an org's member list.
type Member struct {
	Username string
	Role     string
}

// ErrNotMember is returned when the target username has no membership row.
var ErrNotMember = errors.New("not a member")

// ErrLastOwner is returned when a role change or removal would leave the org
// with no owner.
var ErrLastOwner = errors.New("an org must keep at least one owner")

// Members lists an org's members, oldest first.
func (s *Store) Members(orgID string) ([]Member, error) {
	rows, err := s.db.Query(
		`SELECT a.username, m.role
		   FROM org_members m JOIN accounts a ON a.id = m.account_id
		  WHERE m.org_id = ? ORDER BY m.rowid`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.Username, &m.Role); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// memberForUpdate resolves username's membership row inside tx and, when the
// change would drop an owner, enforces the last-owner rule.
func memberForUpdate(tx *sql.Tx, orgID, username string, dropsOwner func(cur string) bool) (targetID string, err error) {
	var cur string
	err = tx.QueryRow(
		`SELECT a.id, m.role
		   FROM org_members m JOIN accounts a ON a.id = m.account_id
		  WHERE m.org_id = ? AND a.username = ?`, orgID, username).
		Scan(&targetID, &cur)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotMember
	}
	if err != nil {
		return "", err
	}
	if dropsOwner(cur) {
		var owners int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM org_members WHERE org_id = ? AND role = 'owner'`, orgID).
			Scan(&owners); err != nil {
			return "", err
		}
		if owners <= 1 {
			return "", ErrLastOwner
		}
	}
	return targetID, nil
}

// SetMemberRole changes username's role in the org. Demoting the last owner is
// ErrLastOwner; an unknown target is ErrNotMember.
func (s *Store) SetMemberRole(orgID, username, role string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	targetID, err := memberForUpdate(tx, orgID, username,
		func(cur string) bool { return cur == "owner" && role != "owner" })
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE org_members SET role=? WHERE org_id=? AND account_id=?`, role, orgID, targetID); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveMember deletes username's membership. Removing the last owner is
// ErrLastOwner; an unknown target is ErrNotMember. The member's personal
// account and boxes are untouched.
func (s *Store) RemoveMember(orgID, username string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	targetID, err := memberForUpdate(tx, orgID, username,
		func(cur string) bool { return cur == "owner" })
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM org_members WHERE org_id=? AND account_id=?`, orgID, targetID); err != nil {
		return err
	}
	return tx.Commit()
}
