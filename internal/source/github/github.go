// Package github implements source.Provider for a per-user GitHub App:
// webhook verification, installation-token code fetch, and Deployments status.
package github

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const defaultAPIBase = "https://api.github.com"

type Config struct {
	AppID         int64
	PrivateKeyPEM string
	WebhookSecret string
	APIBase       string // defaults to https://api.github.com
}

type Provider struct {
	secret  string
	apiBase string
	http    *http.Client
	tokens  TokenSource
}

func New(cfg Config) (*Provider, error) {
	key, err := parsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse app private key: %w", err)
	}
	base := apiBaseOrDefault(cfg.APIBase)
	client := &http.Client{Timeout: 30 * time.Second}
	return &Provider{
		secret:  cfg.WebhookSecret,
		apiBase: base,
		http:    client,
		tokens:  &appTokenSource{appID: cfg.AppID, key: key, apiBase: base, http: client},
	}, nil
}

// NewWithTokens builds a Provider whose tokens come from ts. Brokered boxes
// hold no App key, so cfg.AppID and cfg.PrivateKeyPEM are ignored.
func NewWithTokens(cfg Config, ts TokenSource) *Provider {
	return &Provider{
		secret:  cfg.WebhookSecret,
		apiBase: apiBaseOrDefault(cfg.APIBase),
		http:    &http.Client{Timeout: 30 * time.Second},
		tokens:  ts,
	}
}

func apiBaseOrDefault(base string) string {
	if base == "" {
		base = defaultAPIBase
	}
	return strings.TrimRight(base, "/")
}

func parsePrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return rk, nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
