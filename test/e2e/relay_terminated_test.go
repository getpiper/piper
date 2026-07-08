package e2e

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestRelayTerminatedSelfService proves the full free-tier loop:
// piper login (device flow, auto-approved) → piper connect (account-bound enroll,
// terminated) → piper deploy → curl the relay-assigned hostname, which the relay
// terminates with its wildcard cert and forwards as HTTP to the box's :80.
func TestRelayTerminatedSelfService(t *testing.T) {
	if os.Getenv("RUN_E2E") != "1" {
		t.Skip("set RUN_E2E=1 to run (needs Docker; Caddy is embedded)")
	}
	repoRoot, _ := filepath.Abs("../..")
	apex := "public.localhost"
	certFile, keyFile := writeSelfSigned(t, apex) // *.public.localhost

	binDir := t.TempDir()
	for _, c := range []string{"piperd", "piper-relay", "piper"} {
		b := exec.Command("go", "build", "-o", filepath.Join(binDir, c), "./cmd/"+c)
		b.Dir = repoRoot
		if out, err := b.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", c, err, out)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	relayData := t.TempDir()
	relay := exec.CommandContext(ctx, filepath.Join(binDir, "piper-relay"))
	relay.Env = append(os.Environ(),
		"PIPER_RELAY_DATA_DIR="+relayData,
		"PIPER_RELAY_TLS_ADDR=127.0.0.1:8443",
		"PIPER_RELAY_TUNNEL_ADDR=127.0.0.1:7000",
		"PIPER_RELAY_API_ADDR=127.0.0.1:8080",
		"PIPER_RELAY_TUNNEL_PUBLIC=127.0.0.1:7000",
		"PIPER_RELAY_APEX="+apex,
		"PIPER_RELAY_TLS_CERT="+certFile,
		"PIPER_RELAY_TLS_KEY="+keyFile,
		"PIPER_RELAY_FAKE_APPROVE=1",
	)
	relay.Stdout, relay.Stderr = os.Stdout, os.Stderr
	if err := relay.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	defer relay.Process.Kill()
	waitPort(t, "127.0.0.1:7000", 10*time.Second)
	waitPort(t, "127.0.0.1:8080", 10*time.Second)

	// piper login (device flow auto-approves) → writes ~/.piper/piper.
	home := t.TempDir()
	piperEnv := append(os.Environ(), "HOME="+home, "PIPER_ADDR=", "PIPER_TOKEN=")
	login := exec.Command(filepath.Join(binDir, "piper"), "login", "--relay", "http://127.0.0.1:8080")
	login.Env = piperEnv
	if out, err := login.CombinedOutput(); err != nil {
		t.Fatalf("piper login: %v\n%s", err, out)
	}

	// piper connect --data-dir <piperd data> → account-bound enroll + relay.json (terminated).
	piperdData := t.TempDir()
	connect := exec.Command(filepath.Join(binDir, "piper"), "connect", "--data-dir", piperdData)
	connect.Env = piperEnv
	if out, err := connect.CombinedOutput(); err != nil {
		t.Fatalf("piper connect: %v\n%s", err, out)
	}

	// Mint a control-API token, then start piperd in terminated mode (reads relay.json).
	tokenCmd := exec.Command(filepath.Join(binDir, "piperd"), "token", "create", "--name", "e2e")
	tokenCmd.Env = append(os.Environ(), "PIPER_DATA_DIR="+piperdData)
	tokenOut, err := tokenCmd.Output()
	if err != nil {
		t.Fatalf("token create: %v", err)
	}
	apiToken := strings.TrimSpace(string(tokenOut))

	pd := exec.CommandContext(ctx, filepath.Join(binDir, "piperd"))
	pd.Env = append(os.Environ(),
		"PIPER_DATA_DIR="+piperdData,
		"PIPER_API_ADDR=127.0.0.1:8088",
	)
	pd.Stdout, pd.Stderr = os.Stdout, os.Stderr
	if err := pd.Start(); err != nil {
		t.Fatalf("start piperd: %v", err)
	}
	defer pd.Process.Kill()
	waitPort(t, "127.0.0.1:8088", 15*time.Second)

	// Create the app, then deploy. Terminated deploy registers the hostname over
	// the tunnel, so retry until piperd's tunnel client has connected to the relay.
	create := exec.Command(filepath.Join(binDir, "piper"), "create", "blog", "--port", "8080")
	create.Env = append(piperEnv, "PIPER_ADDR=http://127.0.0.1:8088", "PIPER_TOKEN="+apiToken)
	if out, err := create.CombinedOutput(); err != nil {
		t.Fatalf("piper create: %v\n%s", err, out)
	}
	var deployErr string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		dep := exec.Command(filepath.Join(binDir, "piper"),
			"deploy", "blog", "--path", filepath.Join(repoRoot, "test/e2e/sampleapp"))
		dep.Env = append(piperEnv, "PIPER_ADDR=http://127.0.0.1:8088", "PIPER_TOKEN="+apiToken)
		out, err := dep.CombinedOutput()
		if err == nil {
			deployErr = ""
			break
		}
		deployErr = fmt.Sprintf("%v\n%s", err, out)
		time.Sleep(1 * time.Second)
	}
	if deployErr != "" {
		t.Fatalf("piper deploy: %s", deployErr)
	}

	// The relay assigned <hash>-e2e.public.localhost; read it back from the relay's
	// hostnames registry (the box never composes the name).
	hostname := terminatedHostname(t, relayData)
	if !strings.HasSuffix(hostname, "."+apex) {
		t.Fatalf("assigned hostname %q not under %q", hostname, apex)
	}

	// Visitor: TLS to the relay :8443 with SNI = assigned hostname, GET /.
	var body string
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		d := &tls.Dialer{Config: &tls.Config{ServerName: hostname, InsecureSkipVerify: true}}
		conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:8443")
		if err == nil {
			fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", hostname)
			b, _ := io.ReadAll(conn)
			conn.Close()
			if len(b) > 0 {
				body = string(b)
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if body == "" {
		t.Fatal("no response through relay termination")
	}
	fmt.Printf("terminated e2e response:\n%s\n", body)
}

// terminatedHostname reads the single registered hostname from the relay's
// SQLite store. The relay owns naming, so this is the authoritative source for
// the assigned public hostname the box was given.
func terminatedHostname(t *testing.T, relayData string) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(relayData, "relay.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open relay db: %v", err)
	}
	defer db.Close()
	var hostname string
	if err := db.QueryRow(`SELECT hostname FROM hostnames LIMIT 1`).Scan(&hostname); err != nil {
		t.Fatalf("read hostname from relay db: %v", err)
	}
	return hostname
}
