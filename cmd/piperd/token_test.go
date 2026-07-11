package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getpiper/piper/internal/store"
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

func TestDropToStateDirOwnerNoopWhenAlreadyOwner(t *testing.T) {
	// The dir is owned by whoever runs the test, so euid already matches and
	// no setuid is attempted — this covers the decision, not the syscall.
	if err := dropToStateDirOwner(t.TempDir()); err != nil {
		t.Fatalf("want nil for already-owned dir, got %v", err)
	}
}
