package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getpiper/piper/internal/client"
)

// TestEndToEndDeploy builds piperd, runs it against real Docker + Caddy, deploys
// the sample app, and fetches it through Caddy's :80 by Host header.
// Skips unless RUN_E2E=1 and Docker is available (Caddy is embedded in piperd).
func TestEndToEndDeploy(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker + free :80/:8088/:2019)")
	}
	repoRoot, _ := filepath.Abs("../..")
	dataDir := t.TempDir()

	// Build piperd.
	bin := filepath.Join(t.TempDir(), "piperd")
	build := exec.Command("go", "build", "-o", bin, "./cmd/piperd")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build piperd: %v\n%s", err, out)
	}

	// Mint a token before starting the daemon, so there's only one writer to
	// piper.db at a time.
	tokenCmd := exec.Command(bin, "token", "create", "--name", "e2e")
	tokenCmd.Env = append(os.Environ(), "PIPER_DATA_DIR="+dataDir)
	tokenOut, err := tokenCmd.Output()
	if err != nil {
		t.Fatalf("token create: %v", err)
	}
	token := strings.TrimSpace(string(tokenOut))
	if token == "" {
		t.Fatal("token create: empty token")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(),
		"PIPER_DATA_DIR="+dataDir,
		"PIPER_API_ADDR=127.0.0.1:8088",
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start piperd: %v", err)
	}
	defer cmd.Process.Kill()

	waitPort(t, "127.0.0.1:8088", 15*time.Second)

	c := client.New("http://127.0.0.1:8088", token)
	if err := c.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	dep, err := c.Deploy("blog", filepath.Join(repoRoot, "test/e2e/sampleapp"))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	// Deploy now returns at 202, before the build runs: follow the deployment
	// to completion (the real exercise of the async polling contract) so the
	// curl-retry window below only has to absorb route propagation, not the
	// whole Docker build.
	followCtx, cancelFollow := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelFollow()
	final, err := c.FollowDeploy(followCtx, "blog", dep.ID, io.Discard)
	if err != nil {
		t.Fatalf("FollowDeploy: %v", err)
	}
	if final.Status != "running" {
		t.Fatalf("deploy status = %q, want running", final.Status)
	}

	// Fetch through Caddy on :80 with Host: blog.piper.localhost. Retry until
	// we see the sample app's exact response — any 200 isn't enough, since a
	// stray server squatting on :80 can answer 200 with an empty body (#126).
	const wantBody = "hello piper\n"
	req, _ := http.NewRequest("GET", "http://127.0.0.1:80/", nil)
	req.Host = "blog.piper.localhost"
	var body string
	var lastStatus int
	var lastErr error
	ok := false
	for i := 0; i < 20; i++ {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lastStatus = resp.StatusCode
		body = string(b)
		if lastStatus == http.StatusOK && body == wantBody {
			ok = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ok {
		if lastStatus == 0 {
			t.Fatalf("never connected through Caddy: %v", lastErr)
		}
		t.Fatalf("connected through Caddy but never got the app's response: last status %d, body %q (want 200 with %q)", lastStatus, body, wantBody)
	}
	fmt.Printf("e2e response: %q\n", body)

	// Stop: the hostname must stop serving the app; the app stays listed as
	// "stopped" with its history intact.
	if err := c.StopApp("blog"); err != nil {
		t.Fatalf("StopApp: %v", err)
	}
	afterStop, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch after stop: %v", err)
	}
	stopped, _ := io.ReadAll(afterStop.Body)
	afterStop.Body.Close()
	if string(stopped) == body {
		t.Fatalf("hostname still serves the app after stop: %q", stopped)
	}
	apps, err := c.ListApps()
	if err != nil {
		t.Fatalf("ListApps after stop: %v", err)
	}
	if len(apps) != 1 || apps[0].Status != "stopped" {
		t.Fatalf("apps after stop = %+v, want blog stopped", apps)
	}

	// Delete: app and state gone.
	if err := c.DeleteApp("blog"); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	apps, err = c.ListApps()
	if err != nil {
		t.Fatalf("ListApps after delete: %v", err)
	}
	if len(apps) != 0 {
		t.Fatalf("apps after delete = %+v, want none", apps)
	}
}

func waitPort(t *testing.T, addr string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			conn.Close()
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("port %s never opened", addr)
}
