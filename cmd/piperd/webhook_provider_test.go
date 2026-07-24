package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piperbox/piper/internal/api"
	"github.com/piperbox/piper/internal/config"
	"github.com/piperbox/piper/internal/store"
)

// TestDecideWebhookProvider pins the precedence rule that both run() and the
// boot gate consult: a local App row is an explicit BYO override and always
// wins, only store.ErrNotFound permits brokered mode, and any other store
// error fails closed rather than falling back to the relay.
func TestDecideWebhookProvider(t *testing.T) {
	brokeredCfg := config.Config{GitHubBrokered: true, WebhookSecret: "shh"}

	tests := []struct {
		name       string
		ghErr      error
		cfg        config.Config
		hasGHToken bool
		want       webhookProvider
	}{
		{
			name:       "local row present wins over a brokering relay",
			ghErr:      nil,
			cfg:        brokeredCfg,
			hasGHToken: true,
			want:       webhookProviderBYO,
		},
		{
			name:       "no local row, brokered and fully configured",
			ghErr:      store.ErrNotFound,
			cfg:        brokeredCfg,
			hasGHToken: true,
			want:       webhookProviderBrokered,
		},
		{
			name:       "no local row, not brokered",
			ghErr:      store.ErrNotFound,
			cfg:        config.Config{},
			hasGHToken: false,
			want:       webhookProviderNone,
		},
		{
			name:       "no local row, brokered but no webhook secret",
			ghErr:      store.ErrNotFound,
			cfg:        config.Config{GitHubBrokered: true},
			hasGHToken: true,
			want:       webhookProviderNone,
		},
		{
			name:       "no local row, brokered but no relay token func",
			ghErr:      store.ErrNotFound,
			cfg:        brokeredCfg,
			hasGHToken: false,
			want:       webhookProviderNone,
		},
		{
			name:       "non-ErrNotFound store error fails closed, even brokered",
			ghErr:      errors.New("database is locked"),
			cfg:        brokeredCfg,
			hasGHToken: true,
			want:       webhookProviderNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideWebhookProvider(tt.ghErr, tt.cfg, tt.hasGHToken)
			if got != tt.want {
				t.Fatalf("decideWebhookProvider(%v, %+v, %v) = %v, want %v", tt.ghErr, tt.cfg, tt.hasGHToken, got, tt.want)
			}
		})
	}
}

// TestNewRepoFetcherWithoutGitHubReturnsErrNoGitHubApp pins the seam the
// deploy-from-repo endpoint relies on for its 409: with no credential source
// at call time, the fetcher must fail with api.ErrNoGitHubApp, not a generic
// error the handler would report as a fetch failure.
func TestNewRepoFetcherWithoutGitHubReturnsErrNoGitHubApp(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "piperd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fetch := newRepoFetcher(st, config.Config{}, nil)
	err = fetch(context.Background(), "alice/blog", "main", t.TempDir())
	if !errors.Is(err, api.ErrNoGitHubApp) {
		t.Fatalf("fetch with no GitHub App = %v, want api.ErrNoGitHubApp", err)
	}
}

// TestWebhookProviderName pins the strings `piper github reset` reports back to
// the operator, so the CLI can phrase its follow-up per provider.
func TestWebhookProviderName(t *testing.T) {
	for prov, want := range map[webhookProvider]string{
		webhookProviderBYO:      "byo",
		webhookProviderBrokered: "brokered",
		webhookProviderNone:     "none",
	} {
		if got := prov.name(); got != want {
			t.Errorf("%v.name() = %q, want %q", prov, got, want)
		}
	}
}

// TestShadowWarning covers the line whose absence cost an hour of diagnosis
// (#299): when a stored App wins on a relay that brokers one, nothing else in
// the log says the brokered App was passed over.
func TestShadowWarning(t *testing.T) {
	brokeredCfg := config.Config{GitHubBrokered: true, WebhookSecret: "shh"}

	if got := shadowWarning(webhookProviderBYO, brokeredCfg); got == "" {
		t.Error("BYO on a brokering relay warned nothing; want the shadowing warning")
	} else if !strings.Contains(got, "piper github reset") {
		t.Errorf("warning = %q, want the reset command", got)
	}
	if got := shadowWarning(webhookProviderBYO, config.Config{}); got != "" {
		t.Errorf("BYO with no brokered offer warned %q, want silence", got)
	}
	if got := shadowWarning(webhookProviderBrokered, brokeredCfg); got != "" {
		t.Errorf("brokered shadows nothing, warned %q", got)
	}
}
