package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAgentNonDarwinGate(t *testing.T) {
	agentGOOS = "linux"
	defer func() { agentGOOS = runtime.GOOS }()
	var out, errb bytes.Buffer
	if code := agent([]string{"up"}, &out, &errb); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "systemctl") {
		t.Errorf("stderr = %q, want systemctl hint", errb.String())
	}
}

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
