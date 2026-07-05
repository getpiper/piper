// Package github implements source.Provider for a per-user GitHub App:
// webhook verification, installation-token code fetch, and Deployments status.
package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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
	appID   int64
	key     *rsa.PrivateKey
	secret  string
	apiBase string
	http    *http.Client
}

func New(cfg Config) (*Provider, error) {
	key, err := parsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse app private key: %w", err)
	}
	base := cfg.APIBase
	if base == "" {
		base = defaultAPIBase
	}
	return &Provider{
		appID:   cfg.AppID,
		key:     key,
		secret:  cfg.WebhookSecret,
		apiBase: strings.TrimRight(base, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}, nil
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

// appJWT mints a short-lived GitHub App JWT (RS256) signed with the app key.
func (p *Provider) appJWT(now time.Time) (string, error) {
	header := b64url([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"%d"}`,
		now.Add(-30*time.Second).Unix(), now.Add(9*time.Minute).Unix(), p.appID)
	signingInput := header + "." + b64url([]byte(claims))
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64url(sig), nil
}

// installationToken exchanges an app JWT for a short-lived installation token.
func (p *Provider) installationToken(ctx context.Context, installationID int64) (string, error) {
	jwt, err := p.appJWT(time.Now())
	if err != nil {
		return "", err
	}
	url := p.apiBase + "/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("installation token: %s: %s", resp.Status, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Token, nil
}
