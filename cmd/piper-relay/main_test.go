package main

import (
	"path/filepath"
	"testing"

	"github.com/getpiper/piper/internal/relay"
	"github.com/getpiper/piper/internal/version"
)

func TestRunAdminDisable(t *testing.T) {
	st, err := relay.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	acc, err := st.UpsertAccount("sub-1", "leo")
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

func TestParseWebRedirects(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"root path kept", "https://dash.getpiper.co/", []string{"https://dash.getpiper.co/"}},
		{"subpath kept", "https://dash.getpiper.co/auth", []string{"https://dash.getpiper.co/auth"}},
		{"no path dropped", "https://dash.getpiper.co", nil},
		{"no scheme dropped", "dash.getpiper.co/", nil},
		{"empty dropped", "", nil},
		{"whitespace-only dropped", "   ", nil},
		{"mixed list", "https://dash.getpiper.co/, https://dash.getpiper.co, https://ok.example/x",
			[]string{"https://dash.getpiper.co/", "https://ok.example/x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseWebRedirects(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("parseWebRedirects(%q) = %v, want %v", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("parseWebRedirects(%q) = %v, want %v", c.in, got, c.want)
				}
			}
		})
	}
}

func TestApiAddrIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{":8080", false},
		{"0.0.0.0:8080", false},
		{"127.0.0.1:8080", true},
		{"localhost:8080", true},
		{"[::1]:8080", true},
		{"192.168.1.5:8080", false},
	}
	for _, c := range cases {
		if got := apiAddrIsLoopback(c.addr); got != c.want {
			t.Errorf("apiAddrIsLoopback(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

// piper-relay must have a version surface so the release ldflags stamp lands
// and the binary can report its build. #61.
func TestVersionRequested(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		if !versionRequested(args) {
			t.Errorf("versionRequested(%v) = false, want true", args)
		}
	}
	for _, args := range [][]string{nil, {"admin"}, {"enroll"}} {
		if versionRequested(args) {
			t.Errorf("versionRequested(%v) = true, want false", args)
		}
	}
	if version.String() == "" {
		t.Error("version.String() is empty")
	}
}
