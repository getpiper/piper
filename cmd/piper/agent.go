package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const launchdLabel = "com.getpiper.piperd"

// agentGOOS is runtime.GOOS; a var so tests can exercise the non-darwin gate.
var agentGOOS = runtime.GOOS

// launchdPlistPath returns the installed LaunchAgent path; a var so tests can
// point it at a temp file.
var launchdPlistPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

// launchctlRun runs `launchctl <args...>` and returns combined output; a var so
// tests can substitute it without shelling out to a real launchd.
var launchctlRun = func(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return string(out), err
}

func guiTarget() string { return "gui/" + strconv.Itoa(os.Getuid()) }

// agent dispatches `piper agent <up|down|status>` — toggling the rootless macOS
// launchd LaunchAgent. macOS only; on other platforms it points at systemd.
func agent(args []string, stdout, stderr io.Writer) int {
	if agentGOOS != "darwin" {
		fmt.Fprintln(stderr, "error: `piper agent` manages the macOS launchd agent; on Linux use `sudo systemctl enable --now piperd`")
		return 2
	}
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status>")
		return 2
	}
	switch args[0] {
	case "up":
		return agentUp(stdout, stderr)
	case "down":
		return agentDown(stdout, stderr)
	case "status":
		return agentStatus(stdout, stderr)
	default:
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status>")
		return 2
	}
}

func agentUp(stdout, stderr io.Writer) int {
	plist, err := launchdPlistPath()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, err := os.Stat(plist); err != nil {
		fmt.Fprintf(stderr, "error: launchd agent not installed at %s\nsee docs/manual-setup.md (Run the agent on macOS)\n", plist)
		return 1
	}
	out, err := launchctlRun("bootstrap", guiTarget(), plist)
	if err != nil {
		if strings.Contains(out, "already") || strings.Contains(out, "5: Input/output error") {
			fmt.Fprintln(stdout, "piperd already running")
			return 0
		}
		fmt.Fprintf(stderr, "error: launchctl bootstrap failed: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintln(stdout, "piperd started")
	return 0
}

func agentDown(stdout, stderr io.Writer) int {
	out, err := launchctlRun("bootout", guiTarget()+"/"+launchdLabel)
	if err != nil {
		if strings.Contains(out, "No such process") || strings.Contains(out, "not find") {
			fmt.Fprintln(stdout, "piperd already stopped")
			return 0
		}
		fmt.Fprintf(stderr, "error: launchctl bootout failed: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintln(stdout, "piperd stopped")
	return 0
}

func agentStatus(stdout, stderr io.Writer) int {
	plist, err := launchdPlistPath()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, err := os.Stat(plist); err != nil {
		fmt.Fprintln(stdout, "piperd: not installed")
		return 0
	}
	out, err := launchctlRun("print", guiTarget()+"/"+launchdLabel)
	if err != nil {
		fmt.Fprintln(stdout, "piperd: stopped")
		return 0
	}
	if strings.Contains(out, "state = running") {
		fmt.Fprintln(stdout, "piperd: running")
	} else {
		fmt.Fprintln(stdout, "piperd: loaded (not running)")
	}
	return 0
}
