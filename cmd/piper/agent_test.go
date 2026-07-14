package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAgentUnsupportedGOOS(t *testing.T) {
	agentGOOS = "windows"
	defer func() { agentGOOS = runtime.GOOS }()
	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "macOS and Linux only") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestAgentUpLinuxStarts(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()

	dir := t.TempDir()
	unit := filepath.Join(dir, "piperd.service")
	if err := os.WriteFile(unit, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldPath := userUnitPath
	userUnitPath = func() (string, error) { return unit, nil }
	defer func() { userUnitPath = oldPath }()

	var gotArgs []string
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) { gotArgs = args; return "", nil }
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	want := []string{"--user", "start", "piperd"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", gotArgs, want)
	}
	if !strings.Contains(out.String(), "started") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestAgentUpLinuxNotInstalled(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()
	oldPath := userUnitPath
	userUnitPath = func() (string, error) { return filepath.Join(t.TempDir(), "absent.service"), nil }
	defer func() { userUnitPath = oldPath }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "not installed") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestAgentDownLinuxStops(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()
	var gotArgs []string
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) { gotArgs = args; return "", nil }
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"down"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	want := []string{"--user", "stop", "piperd"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", gotArgs, want)
	}
}

func TestAgentStatusLinux(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()

	dir := t.TempDir()
	unit := filepath.Join(dir, "piperd.service")
	if err := os.WriteFile(unit, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldPath := userUnitPath
	userUnitPath = func() (string, error) { return unit, nil }
	defer func() { userUnitPath = oldPath }()

	cases := []struct {
		active string
		err    error
		want   string
	}{
		{"active\n", nil, "piperd: running"},
		{"inactive\n", errFake, "piperd: stopped"},
	}
	for _, c := range cases {
		oldRun := systemctlRun
		systemctlRun = func(args ...string) (string, error) { return c.active, c.err }
		var out, errb bytes.Buffer
		if code := agent([]string{"status"}, &out, &errb); code != 0 {
			t.Fatalf("code = %d", code)
		}
		if !strings.Contains(out.String(), c.want) {
			t.Errorf("active=%q: stdout = %q, want %q", c.active, out.String(), c.want)
		}
		systemctlRun = oldRun
	}
}

func TestAgentStatusLinuxNotInstalled(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()
	oldPath := userUnitPath
	userUnitPath = func() (string, error) { return filepath.Join(t.TempDir(), "absent.service"), nil }
	defer func() { userUnitPath = oldPath }()

	var out, errb bytes.Buffer
	if code := agent([]string{"status"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out.String(), "not installed") {
		t.Errorf("stdout = %q", out.String())
	}
}

var errFake = fmt.Errorf("exit status 3")

func TestAgentUpBootstraps(t *testing.T) {
	agentGOOS = "darwin"
	defer func() { agentGOOS = runtime.GOOS }()

	dir := t.TempDir()
	plist := filepath.Join(dir, "com.getpiper.piperd.plist")
	if err := os.WriteFile(plist, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldPath := launchdPlistPath
	launchdPlistPath = func() (string, error) { return plist, nil }
	defer func() { launchdPlistPath = oldPath }()

	var gotArgs []string
	oldRun := launchctlRun
	launchctlRun = func(args ...string) (string, error) { gotArgs = args; return "", nil }
	defer func() { launchctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if len(gotArgs) < 1 || gotArgs[0] != "bootstrap" {
		t.Errorf("launchctl args = %v, want bootstrap ...", gotArgs)
	}
	if !strings.Contains(out.String(), "started") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestAgentDownBootsOut(t *testing.T) {
	agentGOOS = "darwin"
	defer func() { agentGOOS = runtime.GOOS }()
	var gotArgs []string
	oldRun := launchctlRun
	launchctlRun = func(args ...string) (string, error) { gotArgs = args; return "", nil }
	defer func() { launchctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"down"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if len(gotArgs) < 1 || gotArgs[0] != "bootout" {
		t.Errorf("launchctl args = %v, want bootout ...", gotArgs)
	}
}

func TestAgentUsage(t *testing.T) {
	agentGOOS = "darwin"
	defer func() { agentGOOS = runtime.GOOS }()
	var out, errb bytes.Buffer
	if code := agent([]string{"bogus"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage: piper agent") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestEmbeddedSystemFilesMatchCanonical(t *testing.T) {
	for _, name := range []string{"piperd.service", "piperd.env.example"} {
		got, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join("..", "..", "packaging", "systemd", name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("cmd/piper/%s differs from packaging/systemd/%s — re-copy it", name, name)
		}
	}
}

func TestDaemonizeNeedsRoot(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()
	oldEUID := agentEUID
	agentEUID = func() int { return 1000 }
	defer func() { agentEUID = oldEUID }()

	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "sudo") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestDaemonizeNeedsSudoUser(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()
	oldEUID := agentEUID
	agentEUID = func() int { return 0 }
	defer func() { agentEUID = oldEUID }()
	t.Setenv("SUDO_USER", "")

	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "SUDO_USER") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestDaemonizePromotes(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()
	oldEUID := agentEUID
	agentEUID = func() int { return 0 }
	defer func() { agentEUID = oldEUID }()
	t.Setenv("SUDO_USER", "alice")

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".local", "bin", "piperd"), []byte("PIPERD-BIN"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldHome := userHomeDir
	userHomeDir = func(u string) (string, error) { return home, nil }
	defer func() { userHomeDir = oldHome }()

	binDir, unitDir, envDir := t.TempDir(), t.TempDir(), t.TempDir()
	oldBin, oldUnit, oldEnv := systemBinDir, systemUnitDir, systemEnvDir
	systemBinDir, systemUnitDir, systemEnvDir = binDir, unitDir, envDir
	defer func() { systemBinDir, systemUnitDir, systemEnvDir = oldBin, oldUnit, oldEnv }()

	var calls [][]string
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) { calls = append(calls, args); return "", nil }
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	// user teardown, daemon-reload, enable --now
	joined := ""
	for _, c := range calls {
		joined += strings.Join(c, " ") + "\n"
	}
	for _, want := range []string{
		"--user --machine=alice@.host disable --now piperd",
		"daemon-reload",
		"enable --now piperd",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing systemctl call %q; got:\n%s", want, joined)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(binDir, "piperd")); string(b) != "PIPERD-BIN" {
		t.Errorf("piperd not installed to system bindir; got %q", string(b))
	}
	if _, err := os.Stat(filepath.Join(unitDir, "piperd.service")); err != nil {
		t.Errorf("system unit not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(envDir, "piperd.env")); err != nil {
		t.Errorf("env not seeded: %v", err)
	}
	if !strings.Contains(out.String(), "daemonized") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestDaemonizeDoesNotClobberEnv(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()
	oldEUID := agentEUID
	agentEUID = func() int { return 0 }
	defer func() { agentEUID = oldEUID }()
	t.Setenv("SUDO_USER", "alice")

	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755)
	os.WriteFile(filepath.Join(home, ".local", "bin", "piperd"), []byte("x"), 0o755)
	oldHome := userHomeDir
	userHomeDir = func(u string) (string, error) { return home, nil }
	defer func() { userHomeDir = oldHome }()

	binDir, unitDir, envDir := t.TempDir(), t.TempDir(), t.TempDir()
	oldBin, oldUnit, oldEnv := systemBinDir, systemUnitDir, systemEnvDir
	systemBinDir, systemUnitDir, systemEnvDir = binDir, unitDir, envDir
	defer func() { systemBinDir, systemUnitDir, systemEnvDir = oldBin, oldUnit, oldEnv }()

	edited := "PIPER_BASE_DOMAIN=example.com\n"
	if err := os.WriteFile(filepath.Join(envDir, "piperd.env"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) { return "", nil }
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if b, _ := os.ReadFile(filepath.Join(envDir, "piperd.env")); string(b) != edited {
		t.Errorf("env clobbered: got %q", string(b))
	}
}

func TestCopyFileOverwritesAndSetsMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing dst with different content and a restrictive mode: copyFile
	// removes then recreates, so both content and mode reflect the copy.
	if err := os.WriteFile(dst, []byte("OLD"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst, 0o755); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "NEW" {
		t.Errorf("content = %q, want NEW", string(b))
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %o, want 755", fi.Mode().Perm())
	}
}
