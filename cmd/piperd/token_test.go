package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/store"
)

func tokenTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "piperd.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTokenCmdCreateListRevoke(t *testing.T) {
	s := tokenTestStore(t)
	var out bytes.Buffer

	if err := runTokenCmd(s, []string{"create", "--name", "laptop"}, &out); err != nil {
		t.Fatalf("create: %v", err)
	}
	tok := strings.TrimSpace(out.String())
	if tok == "" {
		t.Fatal("no token printed")
	}
	if _, err := s.AuthenticateToken(tok); err != nil {
		t.Fatalf("created token not valid: %v", err)
	}

	out.Reset()
	if err := runTokenCmd(s, []string{"list"}, &out); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), "laptop") {
		t.Fatalf("list missing token: %q", out.String())
	}

	out.Reset()
	if err := runTokenCmd(s, []string{"revoke", "laptop"}, &out); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.AuthenticateToken(tok); err == nil {
		t.Fatal("token still valid after revoke")
	}
}

func TestTokenCmdCreateRequiresName(t *testing.T) {
	s := tokenTestStore(t)
	if err := runTokenCmd(s, []string{"create"}, &bytes.Buffer{}); err == nil {
		t.Fatal("want error when --name missing")
	}
}

func TestOwnerOfReturnsCurrentUID(t *testing.T) {
	uid, _, err := ownerOf(t.TempDir())
	if err != nil {
		t.Fatalf("ownerOf: %v", err)
	}
	if uid != os.Getuid() {
		t.Errorf("uid = %d, want %d", uid, os.Getuid())
	}
}

func TestOwnerOfMissingPath(t *testing.T) {
	if _, _, err := ownerOf(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("want error for missing path")
	}
}

func TestChownDataFilesSkipsMissingAndChownsExisting(t *testing.T) {
	dir := t.TempDir()
	// Only piper.db exists; the -wal/-shm side files are absent, as they are
	// after a plain create against a checkpointed DB.
	if err := os.WriteFile(filepath.Join(dir, "piper.db"), nil, 0o600); err != nil {
		t.Fatalf("write db: %v", err)
	}
	// Chowning to our own uid/gid is permitted without privilege, so this
	// exercises the chown call and the missing-file skip without needing root.
	if err := chownDataFiles(dir, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("chownDataFiles: %v", err)
	}
}

// systemManaged points config at temp dirs simulating a systemd install:
// /etc/piper exists, and the state dir is stateDir. Restores on cleanup.
func systemManaged(t *testing.T, stateDir string) {
	t.Helper()
	oldEnv, oldState := config.SystemEnvDir, config.SystemStateDir
	config.SystemEnvDir = t.TempDir()
	config.SystemStateDir = stateDir
	t.Cleanup(func() { config.SystemEnvDir, config.SystemStateDir = oldEnv, oldState })
}

func TestResolveTokenDataDirEnvWins(t *testing.T) {
	t.Setenv("PIPER_DATA_DIR", "/custom/dir")
	systemManaged(t, t.TempDir()) // even on a systemd box

	dir, owner, err := resolveTokenDataDir([]string{"create", "--name", "x"})
	if err != nil {
		t.Fatalf("resolveTokenDataDir: %v", err)
	}
	if dir != "/custom/dir" {
		t.Errorf("dir = %q, want /custom/dir", dir)
	}
	if owner != nil {
		t.Errorf("owner = %+v, want nil for an explicit PIPER_DATA_DIR", owner)
	}
}

func TestResolveTokenDataDirDefaultWhenNotManaged(t *testing.T) {
	t.Setenv("PIPER_DATA_DIR", "")
	old := config.SystemEnvDir
	config.SystemEnvDir = filepath.Join(t.TempDir(), "absent") // not systemd-managed
	defer func() { config.SystemEnvDir = old }()

	dir, owner, err := resolveTokenDataDir([]string{"list"})
	if err != nil {
		t.Fatalf("resolveTokenDataDir: %v", err)
	}
	if want := config.DefaultDataDir(); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	if owner != nil {
		t.Errorf("owner = %+v, want nil when not systemd-managed", owner)
	}
}

func TestResolveTokenDataDirSystemManagedNonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires non-root")
	}
	t.Setenv("PIPER_DATA_DIR", "")
	systemManaged(t, t.TempDir())

	_, _, err := resolveTokenDataDir([]string{"create", "--name", "laptop"})
	if err == nil {
		t.Fatal("want error for non-root on a systemd-managed box")
	}
	if !strings.Contains(err.Error(), "sudo piperd token create --name laptop") {
		t.Errorf("error %q does not name the sudo command to run", err)
	}
}

func TestResolveTokenDataDirSudoHintQuotesSpacedArgs(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires non-root")
	}
	t.Setenv("PIPER_DATA_DIR", "")
	systemManaged(t, t.TempDir())

	// The hint is meant to be pasted verbatim, so a name with a space must stay
	// a single argument — an unquoted `--name my laptop` re-parses into two (#145).
	_, _, err := resolveTokenDataDir([]string{"create", "--name", "my laptop"})
	if err == nil {
		t.Fatal("want error for non-root on a systemd-managed box")
	}
	if !strings.Contains(err.Error(), "sudo piperd token create --name 'my laptop'") {
		t.Errorf("error %q does not shell-quote the spaced arg", err)
	}
}

func TestResolveTokenDataDirStateDirMissing(t *testing.T) {
	t.Setenv("PIPER_DATA_DIR", "")
	systemManaged(t, filepath.Join(t.TempDir(), "absent"))

	_, _, err := resolveTokenDataDir([]string{"list"})
	if err == nil {
		t.Fatal("want error when the state dir does not exist")
	}
	if !strings.Contains(err.Error(), "systemctl start piperd") {
		t.Errorf("error %q does not say to start the service", err)
	}
}

func TestResolveTokenDataDirSystemManagedRootStaysRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}
	t.Setenv("PIPER_DATA_DIR", "")
	// Mirror the DynamicUser StateDirectory layout: the state dir is a symlink
	// (systemd points /var/lib/piper -> private/piper). Root must resolve it,
	// return the owner to chown DB files to, and — the #212 regression — stay
	// root rather than setuid to the dir owner, which would strand it outside
	// the 0700 /var/lib/private wrapper.
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "piper")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	systemManaged(t, link)

	dir, owner, err := resolveTokenDataDir([]string{"create", "--name", "x"})
	if err != nil {
		t.Fatalf("resolveTokenDataDir: %v", err)
	}
	if os.Geteuid() != 0 {
		t.Fatalf("euid = %d, want 0 (process must not drop privileges)", os.Geteuid())
	}
	if owner == nil {
		t.Fatal("owner = nil, want the state-dir owner to chown DB files to")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) must succeed as root through the symlink: %v", dir, err)
	}
}

func TestShellQuote(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"laptop", "laptop"},
		{"--name", "--name"},
		{"/var/lib/piper", "/var/lib/piper"},
		{"", "''"},
		{"my laptop", "'my laptop'"},
		{"it's", `'it'\''s'`},
		{"a&b;c", "'a&b;c'"},
	} {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
