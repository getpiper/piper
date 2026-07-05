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
		"name":         appName,
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

type AppCredentials struct {
	AppID         int64
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
		PEM           string `json:"pem"`
		WebhookSecret string `json:"webhook_secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return AppCredentials{}, err
	}
	return AppCredentials{AppID: out.ID, PrivateKeyPEM: out.PEM, WebhookSecret: out.WebhookSecret}, nil
}
