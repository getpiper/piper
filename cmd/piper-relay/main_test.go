package main

import (
	"path/filepath"
	"testing"

	"github.com/getpiper/piper/internal/relay"
)

func TestRunAdminDisable(t *testing.T) {
	st, err := relay.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	acc, err := st.UpsertAccount("sub-1", "leo@x.com")
	if err != nil {
		t.Fatal(err)
	}

	if err := runAdmin(st, []string{"disable", acc.Username}); err != nil {
		t.Fatalf("runAdmin disable: %v", err)
	}
	// Disabling again on a real account still succeeds (idempotent-ish); an
	// unknown username must error.
	if err := runAdmin(st, []string{"disable", "no-such-user"}); err == nil {
		t.Fatal("runAdmin disable unknown user succeeded, want error")
	}
}

func TestRunAdminUsage(t *testing.T) {
	st, _ := relay.Open(filepath.Join(t.TempDir(), "relay.db"))
	defer st.Close()
	if err := runAdmin(st, []string{"disable"}); err == nil {
		t.Fatal("runAdmin with no username succeeded, want usage error")
	}
}
