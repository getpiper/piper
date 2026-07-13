package tui

import "testing"

func TestAppURL(t *testing.T) {
	if got := appURL("", false); got != "" {
		t.Fatalf("empty hostname: got %q", got)
	}
	if got := appURL("blog.piper.localhost", false); got != "http://blog.piper.localhost" {
		t.Fatalf("got %q", got)
	}
	if got := appURL("blog.example.dev", true); got != "https://blog.example.dev" {
		t.Fatalf("relay got %q", got)
	}
}

func TestStatusIcon(t *testing.T) {
	cases := map[string]string{
		"running": "●", "building": "◐", "failed": "✗", "stopped": "○", "": "—",
	}
	for status, want := range cases {
		if got := statusIcon(status); got != want {
			t.Fatalf("statusIcon(%q) = %q, want %q", status, got, want)
		}
	}
}
