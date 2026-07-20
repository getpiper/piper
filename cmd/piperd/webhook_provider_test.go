package main

import (
	"errors"
	"testing"

	"github.com/getpiper/piper/internal/config"
	"github.com/getpiper/piper/internal/store"
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
