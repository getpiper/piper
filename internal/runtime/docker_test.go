package runtime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dockerAvailable(t *testing.T) *DockerRuntime {
	t.Helper()
	r, err := NewDockerRuntime()
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := r.cli.Ping(ctx); err != nil {
		t.Skipf("docker not reachable: %v", err)
	}
	return r
}

func TestDockerBuildRunHealthStop(t *testing.T) {
	r := dockerAvailable(t)
	dir := t.TempDir()
	// busybox httpd stays foreground and listens on :8080 so WaitHealthy's
	// TCP dial succeeds — no package install, minimal pull.
	df := "FROM busybox:1.36\nCMD [\"httpd\", \"-f\", \"-p\", \"8080\", \"-h\", \"/\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var progress bytes.Buffer
	b, err := r.Build(ctx, dir, "piper-runtime-test:latest", &progress)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if b.ImageID == "" {
		t.Fatal("empty image id")
	}
	if progress.Len() == 0 {
		t.Fatalf("expected live build output on progress writer")
	}

	run, err := r.Run(ctx, "piper-runtime-test:latest", 8080, map[string]string{"PORT": "8080"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() { r.Stop(context.Background(), run.ContainerID) })
	if run.HostPort == 0 {
		t.Fatal("no host port assigned")
	}

	if err := r.WaitHealthy(ctx, run.HostPort); err != nil {
		t.Fatalf("WaitHealthy: %v", err)
	}
}

func TestDockerBuildFailureReturnsLog(t *testing.T) {
	r := dockerAvailable(t)
	dir := t.TempDir()
	df := "FROM busybox:1.36\nRUN exit 7\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}

	var progress bytes.Buffer
	b, err := r.Build(context.Background(), dir, "piper-runtime-failtest:latest", &progress)
	if err == nil {
		t.Fatal("expected build error")
	}
	if b.Log == "" {
		t.Fatal("expected non-empty build log on failure")
	}
	if !strings.Contains(b.Log, "exit 7") {
		t.Errorf("log should show the failing step, got:\n%s", b.Log)
	}
}
