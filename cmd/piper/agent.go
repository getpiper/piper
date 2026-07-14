package main

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

//go:embed piperd.service
var embeddedSystemUnit string

//go:embed piperd.env.example
var embeddedSystemEnv string

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

const userUnitName = "piperd"

// systemctlRun runs `systemctl <args...>` and returns combined output; a var so
// tests can substitute it without a real systemd.
var systemctlRun = func(args ...string) (string, error) {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	return string(out), err
}

// userUnitPath returns the installed systemd user-unit path; a var so tests can
// point it at a temp file.
var userUnitPath = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", userUnitName+".service"), nil
}

// Overridable system install targets + identity, so daemonize unit-tests
// against temp dirs and stubbed identity.
var (
	agentEUID     = os.Geteuid
	systemBinDir  = "/usr/local/bin"
	systemUnitDir = "/etc/systemd/system"
	systemEnvDir  = "/etc/piper"
)

// userHomeDir resolves a username to its home directory; a var so tests can stub it.
var userHomeDir = func(username string) (string, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

// agent dispatches `piper agent ...` to the platform's rootless agent manager.
func agent(args []string, stdout, stderr io.Writer) int {
	switch agentGOOS {
	case "darwin":
		return agentDarwin(args, stdout, stderr)
	case "linux":
		return agentLinux(args, stdout, stderr)
	default:
		fmt.Fprintln(stderr, "error: `piper agent` supports macOS and Linux only")
		return 2
	}
}

func agentDarwin(args []string, stdout, stderr io.Writer) int {
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

func agentLinux(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status|daemonize>")
		return 2
	}
	switch args[0] {
	case "up":
		return agentUpLinux(stdout, stderr)
	case "down":
		return agentDownLinux(stdout, stderr)
	case "status":
		return agentStatusLinux(stdout, stderr)
	case "daemonize":
		return agentDaemonize(stdout, stderr)
	default:
		fmt.Fprintln(stderr, "usage: piper agent <up|down|status|daemonize>")
		return 2
	}
}

func agentUpLinux(stdout, stderr io.Writer) int {
	unit, err := userUnitPath()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, err := os.Stat(unit); err != nil {
		fmt.Fprintf(stderr, "error: user service not installed at %s\nsee docs/manual-setup.md (Run the agent on Linux, rootless)\n", unit)
		return 1
	}
	if out, err := systemctlRun("--user", "start", userUnitName); err != nil {
		fmt.Fprintf(stderr, "error: systemctl --user start failed: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintln(stdout, "piperd started")
	return 0
}

func agentDownLinux(stdout, stderr io.Writer) int {
	if out, err := systemctlRun("--user", "stop", userUnitName); err != nil {
		fmt.Fprintf(stderr, "error: systemctl --user stop failed: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintln(stdout, "piperd stopped")
	return 0
}

func agentStatusLinux(stdout, stderr io.Writer) int {
	unit, err := userUnitPath()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, err := os.Stat(unit); err != nil {
		fmt.Fprintln(stdout, "piperd: not installed")
		return 0
	}
	out, _ := systemctlRun("--user", "is-active", userUnitName)
	if strings.TrimSpace(out) == "active" {
		fmt.Fprintln(stdout, "piperd: running")
	} else {
		fmt.Fprintln(stdout, "piperd: stopped")
	}
	return 0
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

// agentDaemonize promotes the rootless per-user agent into the systemd system
// daemon (durable, :80/:443, boot-surviving). Linux + root only. It does NOT
// migrate ~/.piper state to /var/lib/piper — a fresh durable install.
func agentDaemonize(stdout, stderr io.Writer) int {
	if agentEUID() != 0 {
		fmt.Fprintln(stderr, "error: `piper agent daemonize` needs root — re-run with sudo")
		return 1
	}
	sudoUser := os.Getenv("SUDO_USER")
	if sudoUser == "" {
		fmt.Fprintln(stderr, "error: SUDO_USER unset — run via `sudo piper agent daemonize`")
		return 1
	}

	// 1. Tear down the invoking user's rootless service (best-effort).
	if out, err := systemctlRun("--user", "--machine="+sudoUser+"@.host", "disable", "--now", userUnitName); err != nil {
		fmt.Fprintf(stderr, "warning: could not stop the rootless service for %s (run `piper agent down` as %s if it lingers): %v\n%s", sudoUser, sudoUser, err, out)
	}

	// 2. Copy piperd from the user's ~/.local/bin into the system bindir.
	home, err := userHomeDir(sudoUser)
	if err != nil {
		fmt.Fprintf(stderr, "error: cannot resolve home for %s: %v\n", sudoUser, err)
		return 1
	}
	if err := os.MkdirAll(systemBinDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if err := copyFile(filepath.Join(home, ".local", "bin", "piperd"), filepath.Join(systemBinDir, "piperd"), 0o755); err != nil {
		fmt.Fprintf(stderr, "error: installing piperd to %s: %v\n", systemBinDir, err)
		return 1
	}

	// 3. Write the system unit.
	if err := os.MkdirAll(systemUnitDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(systemUnitDir, "piperd.service"), []byte(embeddedSystemUnit), 0o644); err != nil {
		fmt.Fprintf(stderr, "error: writing unit: %v\n", err)
		return 1
	}

	// 4. Seed the env file (skip-if-exists — never clobber operator edits).
	if err := os.MkdirAll(systemEnvDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	envPath := filepath.Join(systemEnvDir, "piperd.env")
	if _, err := os.Stat(envPath); err != nil {
		if err := os.WriteFile(envPath, []byte(embeddedSystemEnv), 0o600); err != nil {
			fmt.Fprintf(stderr, "error: writing env: %v\n", err)
			return 1
		}
	}

	// 5. Enable + start the system service.
	if out, err := systemctlRun("daemon-reload"); err != nil {
		fmt.Fprintf(stderr, "error: systemctl daemon-reload: %v\n%s", err, out)
		return 1
	}
	if out, err := systemctlRun("enable", "--now", "piperd"); err != nil {
		fmt.Fprintf(stderr, "error: systemctl enable --now piperd: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintln(stdout, "piperd daemonized — system service on :80/:443, boot-surviving")
	return 0
}

func copyFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, mode)
}
