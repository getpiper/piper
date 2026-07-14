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
