package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRemoteFlagRejectedForLocalOnlyCommands(t *testing.T) {
	for _, cmd := range []string{"version", "login", "connect"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--remote", "box.example.com", cmd}, &stdout, &stderr); code != 2 {
			t.Errorf("%s: code = %d, want 2", cmd, code)
		}
		if got := stderr.String(); !strings.Contains(got, "--remote does not apply") {
			t.Errorf("%s: stderr = %q", cmd, got)
		}
	}
}

// Pins the env-vs-flag guard-rail asymmetry: PIPER_REMOTE must NOT affect
// local-only commands (it passes trivially today; it guards against Task 2
// and later work wiring the env into these commands by accident).
func TestRunVersionIgnoresPiperRemoteEnv(t *testing.T) {
	t.Setenv("PIPER_REMOTE", "box.example.com")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
}
