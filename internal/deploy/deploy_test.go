package deploy

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/getpiper/piper/internal/runtime"
	"github.com/getpiper/piper/internal/store"
)

type fakeCaddy struct {
	upserts map[string]int
}

func newFakeCaddy() *fakeCaddy {
	return &fakeCaddy{upserts: make(map[string]int)}
}

func (f *fakeCaddy) UpsertRoute(host string, port int) error {
	f.upserts[host] = port
	return nil
}

func (f *fakeCaddy) RemoveRoute(string) error { return nil }

func newStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deploy.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return s, path
}

func deploymentCountWithStatus(t *testing.T, path, status string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("Open database: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM deployments WHERE status=?`, status).Scan(&count); err != nil {
		t.Fatalf("Count deployments: %v", err)
	}
	return count
}

func TestDeploySuccessRoutesAndRecords(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	routes := newFakeCaddy()
	d := New(s, rt, routes, "piper.localhost")

	dep, err := d.Deploy(context.Background(), "blog", t.TempDir())
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if dep.Status != "running" {
		t.Errorf("status = %q, want running", dep.Status)
	}
	if routes.upserts["blog.piper.localhost"] != 40001 {
		t.Errorf("routes = %+v, want blog.piper.localhost -> 40001", routes.upserts)
	}
	got, err := s.LatestRunning("blog")
	if err != nil {
		t.Fatalf("LatestRunning: %v", err)
	}
	if got.ContainerID != "c1" {
		t.Errorf("container ID = %q, want c1", got.ContainerID)
	}
}

func TestDeploySecondStopsPrevious(t *testing.T) {
	s, _ := newStore(t)
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("first Deploy: %v", err)
	}

	rt.BuildResultVal = runtime.BuildResult{ImageID: "img2"}
	rt.RunResultVal = runtime.RunResult{ContainerID: "c2", HostPort: 40002}
	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); err != nil {
		t.Fatalf("second Deploy: %v", err)
	}

	if len(rt.Stopped) != 1 || rt.Stopped[0] != "c1" {
		t.Errorf("stopped = %v, want [c1]", rt.Stopped)
	}
	got, err := s.LatestRunning("blog")
	if err != nil {
		t.Fatalf("LatestRunning: %v", err)
	}
	if got.ContainerID != "c2" {
		t.Errorf("container ID = %q, want c2", got.ContainerID)
	}
}

func TestDeployHealthFailureStopsContainerAndRecordsFailed(t *testing.T) {
	s, path := newStore(t)
	healthErr := errors.New("unhealthy")
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunResultVal:   runtime.RunResult{ContainerID: "c1", HostPort: 40001},
		HealthErr:      healthErr,
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); !errors.Is(err, healthErr) {
		t.Fatalf("Deploy error = %v, want health error", err)
	}
	if len(rt.Stopped) != 1 || rt.Stopped[0] != "c1" {
		t.Errorf("stopped = %v, want [c1]", rt.Stopped)
	}
	if got := deploymentCountWithStatus(t, path, "failed"); got != 1 {
		t.Errorf("failed deployment count = %d, want 1", got)
	}
	if _, err := s.LatestRunning("blog"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LatestRunning error = %v, want ErrNotFound", err)
	}
}

func TestDeployBuildFailureRecordsFailed(t *testing.T) {
	s, path := newStore(t)
	buildErr := errors.New("build failed")
	rt := &runtime.FakeRuntime{BuildErr: buildErr}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); !errors.Is(err, buildErr) {
		t.Fatalf("Deploy error = %v, want build error", err)
	}
	if got := deploymentCountWithStatus(t, path, "failed"); got != 1 {
		t.Errorf("failed deployment count = %d, want 1", got)
	}
}

func TestDeployRunFailureRecordsFailed(t *testing.T) {
	s, path := newStore(t)
	runErr := errors.New("run failed")
	rt := &runtime.FakeRuntime{
		BuildResultVal: runtime.BuildResult{ImageID: "img1"},
		RunErr:         runErr,
	}
	d := New(s, rt, newFakeCaddy(), "piper.localhost")

	if _, err := d.Deploy(context.Background(), "blog", t.TempDir()); !errors.Is(err, runErr) {
		t.Fatalf("Deploy error = %v, want run error", err)
	}
	if got := deploymentCountWithStatus(t, path, "failed"); got != 1 {
		t.Errorf("failed deployment count = %d, want 1", got)
	}
}
