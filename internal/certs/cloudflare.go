package certs

import (
	"crypto/ecdsa"
	"fmt"

	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
)

// NewCloudflareIssuer builds a Manager whose DNS-01 challenges use the given
// Cloudflare API token explicitly (the API-managed domain path), instead of
// lego's env-var lookup.
func NewCloudflareIssuer(email, caDirURL, token string, accountKey *ecdsa.PrivateKey) (*Manager, error) {
	if token == "" {
		return nil, fmt.Errorf("cloudflare: empty API token")
	}
	cfg := cloudflare.NewDefaultConfig()
	cfg.AuthToken = token
	provider, err := cloudflare.NewDNSProviderConfig(cfg)
	if err != nil {
		return nil, err
	}
	return New(Config{Email: email, CADirURL: caDirURL, DNSProvider: provider, AccountKey: accountKey})
}
