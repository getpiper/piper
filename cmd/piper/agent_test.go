package main

import (
	"bytes"
	"fmt"
	"io"
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

// onLinux points agentGOOS at linux and systemUnitDir at an empty temp dir, so
// tier detection sees a rootless box; restoration is registered via t.Cleanup
// so it runs after daemonizeDirs' cleanup (LIFO) and the globals end up back at
// their real values.
func onLinux(t *testing.T) {
	t.Helper()
	agentGOOS = "linux"
	oldUnitDir := systemUnitDir
	systemUnitDir = t.TempDir()
	t.Cleanup(func() {
		agentGOOS = runtime.GOOS
		systemUnitDir = oldUnitDir
	})
}

// daemonized marks the current (temp) systemUnitDir as holding a system unit,
// flipping tier detection to the system service.
func daemonized(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(systemUnitDir, "piperd.service"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// rootlessPaths points userUnitPath/userEnvPath into a temp dir and returns the
// two paths, so `up` materializes into a sandbox; restore via the returned func.
func rootlessPaths(t *testing.T) (unit, env string, restore func()) {
	t.Helper()
	dir := t.TempDir()
	unit = filepath.Join(dir, "systemd", "piperd.service")
	env = filepath.Join(dir, ".piper", "piperd.env")
	oldUnit, oldEnv := userUnitPath, userEnvPath
	userUnitPath = func() (string, error) { return unit, nil }
	userEnvPath = func() (string, error) { return env, nil }
	return unit, env, func() { userUnitPath, userEnvPath = oldUnit, oldEnv }
}

// fastPoll zeroes waitActive's inter-poll delay so readiness-check tests don't
// sleep; it returns a restore func for defer.
func fastPoll(t *testing.T) func() {
	t.Helper()
	old := activePollDelay
	activePollDelay = 0
	return func() { activePollDelay = old }
}

func TestAgentUpLinuxMaterializesAndStarts(t *testing.T) {
	onLinux(t)
	unit, env, restore := rootlessPaths(t)
	defer restore()

	defer fastPoll(t)()
	var calls [][]string
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) {
		calls = append(calls, args)
		if len(args) >= 2 && args[1] == "is-active" {
			return "active", nil // stays up
		}
		return "", nil
	}
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if b, err := os.ReadFile(unit); err != nil || string(b) != embeddedUserUnit {
		t.Errorf("user unit not materialized from embed: %v", err)
	}
	if b, err := os.ReadFile(env); err != nil || string(b) != embeddedUserEnv {
		t.Errorf("env not seeded from embed: %v", err)
	}
	joined := ""
	for _, c := range calls {
		joined += strings.Join(c, " ") + "\n"
	}
	for _, want := range []string{"--user daemon-reload", "--user start piperd"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing systemctl call %q; got:\n%s", want, joined)
		}
	}
	if !strings.Contains(out.String(), "started") {
		t.Errorf("stdout = %q", out.String())
	}
	if !strings.Contains(out.String(), "won't survive a reboot") {
		t.Errorf("up must note rootless ephemerality: %q", out.String())
	}
}

func TestAgentUpLinuxRefreshesStaleUnitKeepsEnv(t *testing.T) {
	onLinux(t)
	unit, env, restore := rootlessPaths(t)
	defer restore()
	os.MkdirAll(filepath.Dir(unit), 0o755)
	os.WriteFile(unit, []byte("stale unit from an older piper"), 0o644)
	os.MkdirAll(filepath.Dir(env), 0o700)
	edited := "PIPER_API_ADDR=0.0.0.0:8088\n"
	os.WriteFile(env, []byte(edited), 0o600)

	defer fastPoll(t)()
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) {
		if len(args) >= 2 && args[1] == "is-active" {
			return "active", nil
		}
		return "", nil
	}
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if b, _ := os.ReadFile(unit); string(b) != embeddedUserUnit {
		t.Errorf("stale unit not refreshed; got %q", string(b))
	}
	if b, _ := os.ReadFile(env); string(b) != edited {
		t.Errorf("env clobbered: got %q", string(b))
	}
}

func TestAgentUpLinuxSkipsReloadWhenUnitCurrent(t *testing.T) {
	onLinux(t)
	unit, _, restore := rootlessPaths(t)
	defer restore()
	os.MkdirAll(filepath.Dir(unit), 0o755)
	os.WriteFile(unit, []byte(embeddedUserUnit), 0o644)

	defer fastPoll(t)()
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) {
		if len(args) >= 2 && args[1] == "daemon-reload" {
			t.Errorf("daemon-reload must be skipped when the unit is current")
		}
		if len(args) >= 2 && args[1] == "is-active" {
			return "active", nil
		}
		return "", nil
	}
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
}

func TestAgentUpLinuxReportsCrashLoop(t *testing.T) {
	onLinux(t)
	_, _, restore := rootlessPaths(t)
	defer restore()

	defer fastPoll(t)()
	oldRun := systemctlRun
	// start succeeds, but is-active reports the unit fell into Restart= backoff.
	systemctlRun = func(args ...string) (string, error) {
		if len(args) >= 2 && args[1] == "is-active" {
			return "activating\n", nil
		}
		return "", nil
	}
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1 (stderr=%q)", code, errb.String())
	}
	if strings.Contains(out.String(), "started") {
		t.Errorf("must not report success when crash-looping: %q", out.String())
	}
	if !strings.Contains(errb.String(), "activating") || !strings.Contains(errb.String(), "crash-looping") {
		t.Errorf("stderr should name the state and the crash-loop: %q", errb.String())
	}
}

func TestAgentUpSystemEscalates(t *testing.T) {
	onLinux(t)
	daemonized(t)
	oldEUID := agentEUID
	agentEUID = func() int { return 1000 }
	defer func() { agentEUID = oldEUID }()

	var gotArgs []string
	oldExec := selfExecSudo
	selfExecSudo = func(args []string, stdout, stderr io.Writer) int { gotArgs = args; return 7 }
	defer func() { selfExecSudo = oldExec }()

	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) {
		t.Fatalf("must not run systemctl before escalating; called %v", args)
		return "", nil
	}
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 7 {
		t.Fatalf("code = %d, want the re-exec's exit code 7", code)
	}
	if strings.Join(gotArgs, " ") != "agent up" {
		t.Errorf("re-exec args = %v, want [agent up]", gotArgs)
	}
}

func TestAgentUpSystemStarts(t *testing.T) {
	onLinux(t)
	daemonized(t)
	oldEUID := agentEUID
	agentEUID = func() int { return 0 }
	defer func() { agentEUID = oldEUID }()

	defer fastPoll(t)()
	var startArgs []string
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) {
		if len(args) >= 1 && args[0] == "start" {
			startArgs = args
		}
		if len(args) >= 1 && args[0] == "is-active" {
			return "active", nil
		}
		return "", nil
	}
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if strings.Join(startArgs, " ") != "start piperd" {
		t.Errorf("start args = %v, want [start piperd] (no --user)", startArgs)
	}
	if !strings.Contains(out.String(), "system service") {
		t.Errorf("stdout should name the tier: %q", out.String())
	}
	if strings.Contains(out.String(), "won't survive") {
		t.Errorf("system tier must not print the rootless ephemerality note: %q", out.String())
	}
}

func TestAgentDownLinuxStops(t *testing.T) {
	onLinux(t)
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

func TestAgentDownSystemStops(t *testing.T) {
	onLinux(t)
	daemonized(t)
	oldEUID := agentEUID
	agentEUID = func() int { return 0 }
	defer func() { agentEUID = oldEUID }()

	var gotArgs []string
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) { gotArgs = args; return "", nil }
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"down"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if strings.Join(gotArgs, " ") != "stop piperd" {
		t.Errorf("args = %v, want [stop piperd] (no --user)", gotArgs)
	}
	if !strings.Contains(out.String(), "system service") {
		t.Errorf("stdout should name the tier: %q", out.String())
	}
}

func TestAgentDownSystemEscalates(t *testing.T) {
	onLinux(t)
	daemonized(t)
	oldEUID := agentEUID
	agentEUID = func() int { return 1000 }
	defer func() { agentEUID = oldEUID }()

	var gotArgs []string
	oldExec := selfExecSudo
	selfExecSudo = func(args []string, stdout, stderr io.Writer) int { gotArgs = args; return 0 }
	defer func() { selfExecSudo = oldExec }()

	var out, errb bytes.Buffer
	if code := agent([]string{"down"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if strings.Join(gotArgs, " ") != "agent down" {
		t.Errorf("re-exec args = %v, want [agent down]", gotArgs)
	}
}

func TestAgentStatusLinux(t *testing.T) {
	onLinux(t)
	unit, _, restore := rootlessPaths(t)
	defer restore()
	os.MkdirAll(filepath.Dir(unit), 0o755)
	os.WriteFile(unit, []byte("x"), 0o644)

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

func TestAgentStatusLinuxShowsAddresses(t *testing.T) {
	onLinux(t)
	unit, _, restore := rootlessPaths(t)
	defer restore()
	os.MkdirAll(filepath.Dir(unit), 0o755)
	os.WriteFile(unit, []byte("x"), 0o644)

	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) { return "active\n", nil }
	defer func() { systemctlRun = oldRun }()

	oldEnv := agentEnviron
	agentEnviron = func(scope ...string) map[string]string {
		return map[string]string{
			"PIPER_API_ADDR":   "0.0.0.0:8088",
			"PIPER_HTTP_ADDR":  ":8080",
			"PIPER_HTTPS_ADDR": ":8443",
			"PIPER_DATA_DIR":   "/home/pi/.piper/piperd",
		}
	}
	defer func() { agentEnviron = oldEnv }()

	var out, errb bytes.Buffer
	if code := agent([]string{"status"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{"piperd: running", "http://0.0.0.0:8088", ":8080", ":8443", "/home/pi/.piper/piperd"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status missing %q:\n%s", want, out.String())
		}
	}
}

func TestAgentStatusLinuxNotSetUp(t *testing.T) {
	onLinux(t)
	_, _, restore := rootlessPaths(t)
	defer restore()

	var out, errb bytes.Buffer
	if code := agent([]string{"status"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out.String(), "piper agent up") {
		t.Errorf("stdout should point at `piper agent up`: %q", out.String())
	}
}

func TestAgentStatusSystem(t *testing.T) {
	onLinux(t)
	daemonized(t)

	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) { return "active\n", nil }
	defer func() { systemctlRun = oldRun }()
	oldEnv := agentEnviron
	agentEnviron = func(scope ...string) map[string]string { return nil } // root-only /proc
	defer func() { agentEnviron = oldEnv }()

	var out, errb bytes.Buffer
	if code := agent([]string{"status"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{"piperd: running (system service)", "http://127.0.0.1:8088", ":80", ":443", "/var/lib/piper"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status missing %q:\n%s", want, out.String())
		}
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

func TestAgentUsageLinuxDaemonizeBadFlag(t *testing.T) {
	onLinux(t)
	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize", "--bogus"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "--undo") {
		t.Errorf("usage should mention --undo: %q", errb.String())
	}
}

func TestEmbeddedSystemFilesMatchCanonical(t *testing.T) {
	for _, name := range []string{"piperd.service", "piperd.env.example", "piperd.user.service", "piperd.env.user.example"} {
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

func TestDaemonizeSelfEscalatesWhenNotRoot(t *testing.T) {
	onLinux(t)
	oldEUID := agentEUID
	agentEUID = func() int { return 1000 }
	defer func() { agentEUID = oldEUID }()

	var gotArgs []string
	oldExec := selfExecSudo
	selfExecSudo = func(args []string, stdout, stderr io.Writer) int { gotArgs = args; return 7 }
	defer func() { selfExecSudo = oldExec }()

	// If it tried the actual promotion instead of re-execing, this would fire.
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) {
		t.Fatalf("must not run promotion before escalating; called systemctl %v", args)
		return "", nil
	}
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize"}, &out, &errb); code != 7 {
		t.Fatalf("code = %d, want the re-exec's exit code 7", code)
	}
	if strings.Join(gotArgs, " ") != "agent daemonize" {
		t.Errorf("re-exec args = %v, want [agent daemonize]", gotArgs)
	}
	if !strings.Contains(errb.String(), "under sudo") {
		t.Errorf("stderr should announce the escalation: %q", errb.String())
	}
}

// daemonizeDirs sandboxes the system install targets and returns them.
func daemonizeDirs(t *testing.T) (binDir, unitDir, envDir string) {
	t.Helper()
	binDir, unitDir, envDir = t.TempDir(), t.TempDir(), t.TempDir()
	oldBin, oldUnit, oldEnv := systemBinDir, systemUnitDir, systemEnvDir
	systemBinDir, systemUnitDir, systemEnvDir = binDir, unitDir, envDir
	t.Cleanup(func() { systemBinDir, systemUnitDir, systemEnvDir = oldBin, oldUnit, oldEnv })
	return binDir, unitDir, envDir
}

func TestDaemonizePromotes(t *testing.T) {
	onLinux(t)
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
	if err := os.WriteFile(filepath.Join(home, ".local", "bin", "piper"), []byte("PIPER-CLI"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldHome := userHomeDir
	userHomeDir = func(u string) (string, error) { return home, nil }
	defer func() { userHomeDir = oldHome }()

	binDir, unitDir, envDir := daemonizeDirs(t)

	defer fastPoll(t)()
	var calls [][]string
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) {
		calls = append(calls, args)
		if len(args) >= 1 && args[0] == "is-active" {
			return "active", nil // system service stays up
		}
		return "", nil
	}
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
	if b, _ := os.ReadFile(filepath.Join(binDir, "piper")); string(b) != "PIPER-CLI" {
		t.Errorf("piper CLI not installed to system bindir; got %q", string(b))
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

func TestDaemonizeRealRootUsesInstalledBinaries(t *testing.T) {
	onLinux(t)
	oldEUID := agentEUID
	agentEUID = func() int { return 0 }
	defer func() { agentEUID = oldEUID }()
	t.Setenv("SUDO_USER", "") // real root login, not sudo

	binDir, unitDir, _ := daemonizeDirs(t)
	// The installer already placed the binaries in the system bindir.
	os.WriteFile(filepath.Join(binDir, "piperd"), []byte("PIPERD-BIN"), 0o755)
	os.WriteFile(filepath.Join(binDir, "piper"), []byte("PIPER-CLI"), 0o755)

	defer fastPoll(t)()
	var calls [][]string
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) {
		calls = append(calls, args)
		if len(args) >= 1 && args[0] == "is-active" {
			return "active", nil
		}
		return "", nil
	}
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	for _, c := range calls {
		if strings.Contains(strings.Join(c, " "), "--machine") {
			t.Errorf("real root has no rootless service to tear down; called %v", c)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(binDir, "piperd")); string(b) != "PIPERD-BIN" {
		t.Errorf("pre-installed piperd must be kept; got %q", string(b))
	}
	if _, err := os.Stat(filepath.Join(unitDir, "piperd.service")); err != nil {
		t.Errorf("system unit not written: %v", err)
	}
	if !strings.Contains(out.String(), "daemonized") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestDaemonizeRealRootMissingBinaries(t *testing.T) {
	onLinux(t)
	oldEUID := agentEUID
	agentEUID = func() int { return 0 }
	defer func() { agentEUID = oldEUID }()
	t.Setenv("SUDO_USER", "")

	daemonizeDirs(t) // empty bindir: nothing installed anywhere

	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) { return "", nil }
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1 (stderr=%q)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "installer") {
		t.Errorf("stderr should point at the installer: %q", errb.String())
	}
}

func TestDaemonizeDoesNotClobberEnv(t *testing.T) {
	onLinux(t)
	oldEUID := agentEUID
	agentEUID = func() int { return 0 }
	defer func() { agentEUID = oldEUID }()
	t.Setenv("SUDO_USER", "alice")

	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755)
	os.WriteFile(filepath.Join(home, ".local", "bin", "piperd"), []byte("x"), 0o755)
	os.WriteFile(filepath.Join(home, ".local", "bin", "piper"), []byte("x"), 0o755)
	oldHome := userHomeDir
	userHomeDir = func(u string) (string, error) { return home, nil }
	defer func() { userHomeDir = oldHome }()

	_, _, envDir := daemonizeDirs(t)

	edited := "PIPER_BASE_DOMAIN=example.com\n"
	if err := os.WriteFile(filepath.Join(envDir, "piperd.env"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	defer fastPoll(t)()
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) {
		if len(args) >= 1 && args[0] == "is-active" {
			return "active", nil
		}
		return "", nil
	}
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if b, _ := os.ReadFile(filepath.Join(envDir, "piperd.env")); string(b) != edited {
		t.Errorf("env clobbered: got %q", string(b))
	}
}

func TestDaemonizeReportsCrashLoop(t *testing.T) {
	onLinux(t)
	oldEUID := agentEUID
	agentEUID = func() int { return 0 }
	defer func() { agentEUID = oldEUID }()
	t.Setenv("SUDO_USER", "alice")

	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755)
	os.WriteFile(filepath.Join(home, ".local", "bin", "piperd"), []byte("x"), 0o755)
	os.WriteFile(filepath.Join(home, ".local", "bin", "piper"), []byte("x"), 0o755)
	oldHome := userHomeDir
	userHomeDir = func(u string) (string, error) { return home, nil }
	defer func() { userHomeDir = oldHome }()

	daemonizeDirs(t)

	defer fastPoll(t)()
	oldRun := systemctlRun
	// enable --now succeeds, but the system service never reaches active — e.g.
	// a rootless piperd the machined teardown could not reach still holds :2019.
	systemctlRun = func(args ...string) (string, error) {
		if len(args) >= 1 && args[0] == "is-active" {
			return "activating\n", nil
		}
		return "", nil
	}
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize"}, &out, &errb); code != 1 {
		t.Fatalf("code = %d, want 1 (stderr=%q)", code, errb.String())
	}
	if strings.Contains(out.String(), "daemonized") {
		t.Errorf("must not report success when the system service isn't active: %q", out.String())
	}
	if !strings.Contains(errb.String(), "not active") || !strings.Contains(errb.String(), "piper agent down") {
		t.Errorf("stderr should flag the failure and the remedy: %q", errb.String())
	}
}

func TestDaemonizeUndoEscalates(t *testing.T) {
	onLinux(t)
	oldEUID := agentEUID
	agentEUID = func() int { return 1000 }
	defer func() { agentEUID = oldEUID }()

	var gotArgs []string
	oldExec := selfExecSudo
	selfExecSudo = func(args []string, stdout, stderr io.Writer) int { gotArgs = args; return 0 }
	defer func() { selfExecSudo = oldExec }()

	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize", "--undo"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if strings.Join(gotArgs, " ") != "agent daemonize --undo" {
		t.Errorf("re-exec args = %v, want [agent daemonize --undo]", gotArgs)
	}
}

func TestDaemonizeUndoDemotes(t *testing.T) {
	onLinux(t)
	oldEUID := agentEUID
	agentEUID = func() int { return 0 }
	defer func() { agentEUID = oldEUID }()

	_, unitDir, envDir := daemonizeDirs(t)
	daemonized(t) // systemUnitDir == unitDir now holds piperd.service
	edited := "PIPER_BASE_DOMAIN=example.com\n"
	if err := os.WriteFile(filepath.Join(envDir, "piperd.env"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	var calls [][]string
	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) { calls = append(calls, args); return "", nil }
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize", "--undo"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	joined := ""
	for _, c := range calls {
		joined += strings.Join(c, " ") + "\n"
	}
	for _, want := range []string{"disable --now piperd", "daemon-reload"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing systemctl call %q; got:\n%s", want, joined)
		}
	}
	if _, err := os.Stat(filepath.Join(unitDir, "piperd.service")); err == nil {
		t.Errorf("system unit must be removed")
	}
	if b, _ := os.ReadFile(filepath.Join(envDir, "piperd.env")); string(b) != edited {
		t.Errorf("demotion must keep the env file: got %q", string(b))
	}
	if !strings.Contains(out.String(), "un-daemonized") {
		t.Errorf("stdout = %q", out.String())
	}
	if !strings.Contains(out.String(), "not migrated") {
		t.Errorf("stdout should note the fresh-state stance: %q", out.String())
	}
}

func TestDaemonizeUndoNothingToUndo(t *testing.T) {
	onLinux(t)
	oldEUID := agentEUID
	agentEUID = func() int { return 0 }
	defer func() { agentEUID = oldEUID }()

	oldRun := systemctlRun
	systemctlRun = func(args ...string) (string, error) {
		t.Fatalf("nothing to undo must not touch systemctl; called %v", args)
		return "", nil
	}
	defer func() { systemctlRun = oldRun }()

	var out, errb bytes.Buffer
	if code := agent([]string{"daemonize", "--undo"}, &out, &errb); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "nothing to undo") {
		t.Errorf("stdout = %q", out.String())
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
