package relay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/getpiper/piper/internal/ghjwt"
)

const defaultGitHubAPIBase = "https://api.github.com"

// GitHubAppConfig is the relay's App credentials. An empty AppID means the
// relay runs BYO-only: no ingress endpoint, no token brokering.
type GitHubAppConfig struct {
	AppID         string
	PrivateKeyPEM string
	WebhookSecret string
	APIBase       string // defaults to https://api.github.com
	Slug          string // the App's URL slug; empty disables InstallURL
}

// GitHubApp is the relay's view of one GitHub App: webhook signature
// verification, repo-scoped installation tokens, and repository listing. The
// private key never leaves this type.
type GitHubApp struct {
	appID   string
	key     *rsa.PrivateKey
	secret  string
	apiBase string
	slug    string
	http    *http.Client
}

func NewGitHubApp(cfg GitHubAppConfig) (*GitHubApp, error) {
	key, err := ghjwt.ParseKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse app private key: %w", err)
	}
	base := cfg.APIBase
	if base == "" {
		base = defaultGitHubAPIBase
	}
	return &GitHubApp{
		appID:   cfg.AppID,
		key:     key,
		secret:  cfg.WebhookSecret,
		apiBase: strings.TrimRight(base, "/"),
		slug:    cfg.Slug,
		http:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// InstallURL is the App's install-and-authorize page. Empty when the operator
// configured no slug; the CLI then prints no install link.
func (g *GitHubApp) InstallURL() string {
	if g.slug == "" {
		return ""
	}
	return "https://github.com/apps/" + url.PathEscape(g.slug) + "/installations/new"
}

// VerifySignature checks GitHub's X-Hub-Signature-256 header against the App
// webhook secret in constant time.
func (g *GitHubApp) VerifySignature(header string, body []byte) bool {
	m := hmac.New(sha256.New, []byte(g.secret))
	m.Write(body)
	want := "sha256=" + hex.EncodeToString(m.Sum(nil))
	return hmac.Equal([]byte(header), []byte(want))
}

// installationToken mints an unscoped installation token. Only Repos uses it;
// everything on the deploy path goes through RepoToken.
func (g *GitHubApp) installationToken(ctx context.Context, installationID string, body any) (string, time.Time, error) {
	jwt, err := ghjwt.Sign(g.appID, g.key, time.Now())
	if err != nil {
		return "", time.Time{}, err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return "", time.Time{}, err
		}
		rdr = bytes.NewReader(b)
	}
	url := g.apiBase + "/app/installations/" + installationID + "/access_tokens"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, rdr)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", time.Time{}, fmt.Errorf("installation token: %s: %s", resp.Status, b)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", time.Time{}, err
	}
	return out.Token, out.ExpiresAt, nil
}

// RepoToken mints an installation token scoped to a single repository with the
// minimum permissions a deploy needs. Scoping is what bounds the blast radius
// of a compromised box to the one repo it already deploys.
func (g *GitHubApp) RepoToken(ctx context.Context, installationID, repo string) (string, time.Time, error) {
	name := repo
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		name = repo[i+1:] // GitHub's "repositories" field takes bare names
	}
	return g.installationToken(ctx, installationID, map[string]any{
		"repositories": []string{name},
		"permissions": map[string]string{
			"contents":    "read",
			"deployments": "write",
		},
	})
}

// InstallationAccountID resolves an installation to the GitHub account id
// (user or org) that owns it, authenticated as the App itself rather than
// through the installation (which would beg the question). Callers use this
// to confirm an installation id actually belongs to the identity presenting
// it before trusting any binding built on that id.
func (g *GitHubApp) InstallationAccountID(ctx context.Context, installationID string) (string, error) {
	// installationID comes from an unauthenticated callback query parameter,
	// not from us — GitHub installation ids are always integers, so reject
	// anything else before it reaches a URL rather than merely escaping it.
	if _, err := strconv.ParseInt(installationID, 10, 64); err != nil {
		return "", fmt.Errorf("installation id %q is not numeric", installationID)
	}
	jwt, err := ghjwt.Sign(g.appID, g.key, time.Now())
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.apiBase+"/app/installations/"+installationID, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("get installation: %s: %s", resp.Status, b)
	}
	var out struct {
		Account struct {
			ID int64 `json:"id"`
		} `json:"account"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d", out.Account.ID), nil
}

// Repos lists the repositories an installation can reach, as "owner/name".
// This is what a dashboard's repo picker renders; no list is ever cached.
func (g *GitHubApp) Repos(ctx context.Context, installationID string) ([]string, error) {
	tok, _, err := g.installationToken(ctx, installationID, nil)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.apiBase+"/installation/repositories", nil)
	req.Header.Set("Authorization", "token "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("list repositories: %s: %s", resp.Status, b)
	}
	var out struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Repositories))
	for _, r := range out.Repositories {
		names = append(names, r.FullName)
	}
	return names, nil
}
