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
	"testing"
	"time"

	"github.com/getpiper/piper/internal/client"
)

// TestEndToEndDeploy builds piperd, runs it against real Docker + Caddy, deploys
// the sample app, and fetches it through Caddy's :80 by Host header.
// Skips unless RUN_E2E=1 and Docker + caddy are available.
func TestEndToEndDeploy(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker + caddy on PATH + free :80/:8088/:2019)")
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

	c := client.New("http://127.0.0.1:8088")
	if err := c.CreateApp("blog", 8080); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if _, err := c.Deploy("blog", filepath.Join(repoRoot, "test/e2e/sampleapp")); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// Fetch through Caddy on :80 with Host: blog.piper.localhost.
	req, _ := http.NewRequest("GET", "http://127.0.0.1:80/", nil)
	req.Host = "blog.piper.localhost"
	var body string
	for i := 0; i < 20; i++ {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if body == "" {
		t.Fatal("no response through Caddy")
	}
	fmt.Printf("e2e response: %q\n", body)
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
