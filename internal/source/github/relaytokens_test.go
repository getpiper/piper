package github

import (
	"context"
	"errors"
	"testing"

	"github.com/piperbox/piper/internal/source"
)

func TestRelayTokensAsksForTheEventRepo(t *testing.T) {
	var asked string
	rt := RelayTokens{Ask: func(repo string) (string, error) {
		asked = repo
		return "ghs_from_relay", nil
	}}
	tok, err := rt.Token(context.Background(), source.Event{Repo: "alice/blog"})
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ghs_from_relay" {
		t.Fatalf("token = %q", tok)
	}
	if asked != "alice/blog" {
		t.Fatalf("asked for %q, want alice/blog", asked)
	}
}

func TestRelayTokensSurfacesRelayError(t *testing.T) {
	want := errors.New("relay says no")
	rt := RelayTokens{Ask: func(string) (string, error) { return "", want }}
	if _, err := rt.Token(context.Background(), source.Event{Repo: "alice/blog"}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}
