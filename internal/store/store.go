// Package store persists Piper apps and deployments in SQLite (pure-Go driver).
package store

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
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
	CreatedAt time.Time
}

type Deployment struct {
	ID          string
	App         string
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
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateApp(name string, port int) (App, error) {
	now := time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO apps(name, port, created_at) VALUES(?,?,?)`,
		name, port, now.Format(time.RFC3339Nano))
	if err != nil {
		return App{}, err
	}
	return App{Name: name, Port: port, CreatedAt: now}, nil
}

func (s *Store) GetApp(name string) (App, error) {
	var a App
	var ts string
	err := s.db.QueryRow(`SELECT name, port, created_at FROM apps WHERE name=?`, name).
		Scan(&a.Name, &a.Port, &ts)
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
	rows, err := s.db.Query(`SELECT name, port, created_at FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		var ts string
		if err := rows.Scan(&a.Name, &a.Port, &ts); err != nil {
			return nil, err
		}
		a.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
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

func (s *Store) LatestRunning(app string) (Deployment, error) {
	var d Deployment
	var ts string
	err := s.db.QueryRow(
		`SELECT id, app, image_id, container_id, host_port, status, created_at
		 FROM deployments WHERE app=? AND status='running'
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
