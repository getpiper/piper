package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// BuildManifest returns a GitHub App manifest for a per-user Piper App. The App
// receives push + pull_request events at webhookURL; GitHub redirects the
// browser to redirectURL with a temporary ?code= after creation.
func BuildManifest(appName, webhookURL, redirectURL string) ([]byte, error) {
	m := map[string]any{
		"name":         slugName(appName),
		"url":          "https://github.com/getpiper/piper",
		"redirect_url": redirectURL,
		"public":       false,
		"hook_attributes": map[string]any{
			"url": webhookURL,
		},
		"default_events": []string{"push", "pull_request"},
		"default_permissions": map[string]string{
			"contents":      "read",
			"deployments":   "write",
			"pull_requests": "read",
		},
	}
	return json.Marshal(m)
}

// slugName coerces a name into a valid GitHub App name: lowercase, only
// [a-z0-9-], collapsed/trimmed hyphens, capped at GitHub's 34-char limit.
func slugName(name string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		default:
			if b.Len() > 0 && !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 34 {
		s = strings.TrimRight(s[:34], "-")
	}
	return s
}

type AppCredentials struct {
	AppID         int64
	Slug          string
	PrivateKeyPEM string
	WebhookSecret string
}

// ExchangeCode converts a manifest code into App credentials.
func ExchangeCode(ctx context.Context, apiBase, code string) (AppCredentials, error) {
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	url := strings.TrimRight(apiBase, "/") + "/app-manifests/" + code + "/conversions"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return AppCredentials{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return AppCredentials{}, fmt.Errorf("exchange code: %s: %s", resp.Status, b)
	}
	var out struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		PEM           string `json:"pem"`
		WebhookSecret string `json:"webhook_secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return AppCredentials{}, err
	}
	return AppCredentials{AppID: out.ID, Slug: out.Slug, PrivateKeyPEM: out.PEM, WebhookSecret: out.WebhookSecret}, nil
}
