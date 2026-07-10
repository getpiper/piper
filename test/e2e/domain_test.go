package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRelayCustomDomainSelfService proves the free-tier box can self-serve a
// BYO custom domain (#102) on top of the relay-terminated loop: piper login →
// piper connect → piper deploy (as in TestRelayTerminatedSelfService), then
// PUT /v1/domain on the box's control API drives DNS-01 issuance (stubbed via
// PIPER_TEST_ISSUER=selfsigned) → live Caddy activation → relay SNI splice,
// and a visitor reaches the app on the custom domain through the relay while
// the shared-domain URL keeps serving.
func TestRelayCustomDomainSelfService(t *testing.T) {
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
		"PIPER_TEST_ISSUER=selfsigned",
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

	// ---- Custom domain via the control API (#102) ----
	custom := "shop.localhost"

	// PUT /v1/domain on the box's local control API.
	put := func() (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodPut, "http://127.0.0.1:8088/v1/domain",
			strings.NewReader(`{"domain":"`+custom+`","dns_provider":"cloudflare","dns_token":"fake-for-selfsigned"}`))
		req.Header.Set("Authorization", "Bearer "+apiToken)
		return http.DefaultClient.Do(req)
	}
	resp, err := put()
	if err != nil {
		t.Fatalf("PUT /v1/domain: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /v1/domain = %d: %s", resp.StatusCode, b)
	}

	// Poll GET /v1/domain until active; assert the token never leaks.
	deadline = time.Now().Add(30 * time.Second)
	var domBody string
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8088/v1/domain", nil)
		req.Header.Set("Authorization", "Bearer "+apiToken)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			gb, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			domBody = string(gb)
			if strings.Contains(domBody, `"status":"active"`) {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !strings.Contains(domBody, `"status":"active"`) {
		t.Fatalf("domain never became active: %s", domBody)
	}
	if strings.Contains(domBody, "fake-for-selfsigned") {
		t.Fatalf("GET /v1/domain leaks the dns token: %s", domBody)
	}
	if !strings.Contains(domBody, `"dns_records"`) || !strings.Contains(domBody, `"*.`+custom+`"`) {
		t.Fatalf("GET /v1/domain missing guided-setup records: %s", domBody)
	}

	// Visitor on the custom domain: TLS SNI blog.shop.localhost → relay:8443
	// splices passthrough → box :443 terminates. E2E TLS: relay never decrypts.
	var customResp string
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		d := &tls.Dialer{Config: &tls.Config{ServerName: "blog." + custom, InsecureSkipVerify: true}}
		conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:8443")
		if err == nil {
			fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: blog.%s\r\nConnection: close\r\n\r\n", custom)
			cb, _ := io.ReadAll(conn)
			conn.Close()
			if len(cb) > 0 {
				customResp = string(cb)
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if customResp == "" {
		t.Fatal("no response on the custom domain through the relay")
	}

	// Coexistence: the shared-domain URL still serves.
	hostname := terminatedHostname(t, relayData)
	d := &tls.Dialer{Config: &tls.Config{ServerName: hostname, InsecureSkipVerify: true}}
	conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:8443")
	if err != nil {
		t.Fatalf("shared-domain dial after custom domain: %v", err)
	}
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", hostname)
	sb, _ := io.ReadAll(conn)
	conn.Close()
	if len(sb) == 0 {
		t.Fatal("shared-domain URL broke after adding the custom domain")
	}
}
