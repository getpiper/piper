// Package store persists Piper apps and deployments in SQLite (pure-Go driver).
package store

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

var ErrNotFound = errors.New("not found")

type App struct {
	Name      string
	Port      int
	Repo      string
	Branch    string
	CreatedAt time.Time
}

type Deployment struct {
	ID          string
	App         string
	PR          int
	ImageID     string
	ContainerID string
	HostPort    int
	Status      string
	CreatedAt   time.Time
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate applies additive column changes idempotently (pre-1.0, no migration
// framework). ALTER ... ADD COLUMN errors if the column exists; we ignore that.
func migrate(db *sql.DB) error {
	for _, stmt := range []string{
		`ALTER TABLE apps ADD COLUMN repo TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE apps ADD COLUMN branch TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE deployments ADD COLUMN pr INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.Exec(stmt); err != nil &&
			!strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateApp(name string, port int) (App, error) {
	now := time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO apps(name, port, repo, branch, created_at) VALUES(?,?,?,?,?)`,
		name, port, "", "", now.Format(time.RFC3339Nano))
	if err != nil {
		return App{}, err
	}
	return App{Name: name, Port: port, CreatedAt: now}, nil
}

func (s *Store) GetApp(name string) (App, error) {
	return s.scanApp(s.db.QueryRow(
		`SELECT name, port, repo, branch, created_at FROM apps WHERE name=?`, name))
}

func (s *Store) AppByRepo(repo string) (App, error) {
	return s.scanApp(s.db.QueryRow(
		`SELECT name, port, repo, branch, created_at FROM apps WHERE repo=?`, repo))
}

func (s *Store) UpdateAppRepo(name, repo, branch string) error {
	res, err := s.db.Exec(`UPDATE apps SET repo=?, branch=? WHERE name=?`, repo, branch, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type rowScanner interface{ Scan(dest ...any) error }

func (s *Store) scanApp(row rowScanner) (App, error) {
	var a App
	var ts string
	err := row.Scan(&a.Name, &a.Port, &a.Repo, &a.Branch, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, err
	}
	a.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
	return a, nil
}

func (s *Store) ListApps() ([]App, error) {
	rows, err := s.db.Query(`SELECT name, port, repo, branch, created_at FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		a, err := s.scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) CreateDeployment(app, imageID, containerID string, hostPort int, status string) (Deployment, error) {
	d := Deployment{
		ID: uuid.NewString(), App: app, ImageID: imageID,
		ContainerID: containerID, HostPort: hostPort, Status: status,
		CreatedAt: time.Now().UTC(),
	}
	_, err := s.db.Exec(
		`INSERT INTO deployments(id, app, image_id, container_id, host_port, status, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		d.ID, d.App, d.ImageID, d.ContainerID, d.HostPort, d.Status,
		d.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return Deployment{}, err
	}
	return d, nil
}

func (s *Store) UpdateDeploymentStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE deployments SET status=? WHERE id=?`, status, id)
	return err
}

type GitHubApp struct {
	AppID         int64
	PrivateKey    string
	WebhookSecret string
}

func (s *Store) SaveGitHubApp(a GitHubApp) error {
	_, err := s.db.Exec(
		`INSERT INTO github_app(id, app_id, private_key, webhook_secret) VALUES(1,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET app_id=excluded.app_id,
		   private_key=excluded.private_key, webhook_secret=excluded.webhook_secret`,
		a.AppID, a.PrivateKey, a.WebhookSecret)
	return err
}

func (s *Store) GetGitHubApp() (GitHubApp, error) {
	var a GitHubApp
	err := s.db.QueryRow(`SELECT app_id, private_key, webhook_secret FROM github_app WHERE id=1`).
		Scan(&a.AppID, &a.PrivateKey, &a.WebhookSecret)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubApp{}, ErrNotFound
	}
	return a, err
}

func (s *Store) LatestRunning(app string) (Deployment, error) {
	var d Deployment
	var ts string
	err := s.db.QueryRow(
		`SELECT id, app, image_id, container_id, host_port, status, created_at
		 FROM deployments WHERE app=? AND status='running' AND pr=0
		 ORDER BY created_at DESC LIMIT 1`, app).
		Scan(&d.ID, &d.App, &d.ImageID, &d.ContainerID, &d.HostPort, &d.Status, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
	return d, nil
}

func (s *Store) CreatePreviewDeployment(app string, pr int, imageID, containerID string, hostPort int, status string) (Deployment, error) {
	d := Deployment{
		ID: uuid.NewString(), App: app, PR: pr, ImageID: imageID,
		ContainerID: containerID, HostPort: hostPort, Status: status,
		CreatedAt: time.Now().UTC(),
	}
	_, err := s.db.Exec(
		`INSERT INTO deployments(id, app, image_id, container_id, host_port, status, created_at, pr)
		 VALUES(?,?,?,?,?,?,?,?)`,
		d.ID, d.App, d.ImageID, d.ContainerID, d.HostPort, d.Status,
		d.CreatedAt.Format(time.RFC3339Nano), d.PR)
	if err != nil {
		return Deployment{}, err
	}
	return d, nil
}

func (s *Store) PreviewRunning(app string, pr int) (Deployment, error) {
	var d Deployment
	var ts string
	err := s.db.QueryRow(
		`SELECT id, app, image_id, container_id, host_port, status, created_at, pr
		 FROM deployments WHERE app=? AND pr=? AND status='running'
		 ORDER BY created_at DESC LIMIT 1`, app, pr).
		Scan(&d.ID, &d.App, &d.ImageID, &d.ContainerID, &d.HostPort, &d.Status, &ts, &d.PR)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
	return d, nil
}
