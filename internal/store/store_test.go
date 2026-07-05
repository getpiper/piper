package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateAndGetApp(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	got, err := s.GetApp("blog")
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.Name != "blog" || got.Port != 8080 {
		t.Errorf("got %+v", got)
	}
}

func TestUpdateAppRepoAndAppByRepo(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateApp("blog", 8080); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAppRepo("blog", "alice/blog", "main"); err != nil {
		t.Fatalf("UpdateAppRepo: %v", err)
	}

	got, err := s.AppByRepo("alice/blog")
	if err != nil {
		t.Fatalf("AppByRepo: %v", err)
	}
	if got.Name != "blog" || got.Repo != "alice/blog" || got.Branch != "main" {
		t.Fatalf("got %+v", got)
	}

	if _, err := s.AppByRepo("nobody/none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestGetAppNotFound(t *testing.T) {
	s := openTemp(t)
	if _, err := s.GetApp("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestCreateAppDuplicate(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	if _, err := s.CreateApp("blog", 8080); err == nil {
		t.Error("expected error on duplicate app")
	}
}

func TestListApps(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	s.CreateApp("api", 3000)
	apps, err := s.ListApps()
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 2 || apps[0].Name != "api" || apps[1].Name != "blog" {
		t.Errorf("apps = %+v, want [api blog] ordered", apps)
	}
}

func TestLatestRunning(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	if _, err := s.LatestRunning("blog"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty LatestRunning err = %v, want ErrNotFound", err)
	}
	d1, _ := s.CreateDeployment("blog", "img1", "c1", 40001, "running")
	s.CreateDeployment("blog", "img2", "c2", 40002, "failed")
	got, err := s.LatestRunning("blog")
	if err != nil {
		t.Fatalf("LatestRunning: %v", err)
	}
	if got.ID != d1.ID {
		t.Errorf("LatestRunning ID = %s, want %s", got.ID, d1.ID)
	}
}

func TestUpdateDeploymentStatus(t *testing.T) {
	s := openTemp(t)
	s.CreateApp("blog", 8080)
	d, _ := s.CreateDeployment("blog", "img1", "c1", 40001, "running")
	if err := s.UpdateDeploymentStatus(d.ID, "stopped"); err != nil {
		t.Fatalf("UpdateDeploymentStatus: %v", err)
	}
	if _, err := s.LatestRunning("blog"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after status change to stopped, LatestRunning err = %v, want ErrNotFound", err)
	}
}
