package certs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateAccountKeyIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acme_account.key")
	k1, err := LoadOrCreateAccountKey(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perms = %v, want 0600", fi.Mode().Perm())
	}
	k2, err := LoadOrCreateAccountKey(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if k1.D.Cmp(k2.D) != 0 {
		t.Fatal("second load returned a different key")
	}
}

func TestNewCloudflareIssuerRequiresToken(t *testing.T) {
	key, err := LoadOrCreateAccountKey(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewCloudflareIssuer("a@b.c", "", "", key); err == nil {
		t.Fatal("empty token: want error")
	}
}
