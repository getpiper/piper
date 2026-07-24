package github

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/piperbox/piper/internal/ghjwt"
	"github.com/piperbox/piper/internal/source"
)

// TokenSource yields a GitHub token authorized for ev's repository. BYO mints
// one from the App private key; brokered mode asks the relay for a repo-scoped
// token over the tunnel.
type TokenSource interface {
	Token(ctx context.Context, ev source.Event) (string, error)
}

// appTokenSource mints installation tokens directly from a GitHub App key.
type appTokenSource struct {
	appID   int64
	key     *rsa.PrivateKey
	apiBase string
	http    *http.Client
}

func (a *appTokenSource) Token(ctx context.Context, ev source.Event) (string, error) {
	jwt, err := ghjwt.Sign(strconv.FormatInt(a.appID, 10), a.key, time.Now())
	if err != nil {
		return "", err
	}
	// Webhook events carry the installation ID; manual (API-triggered) deploys
	// have no event, so resolve it from the repository instead.
	instID := ev.InstallationID
	if instID == 0 {
		instID, err = a.installationForRepo(ctx, jwt, ev.Repo)
		if err != nil {
			return "", err
		}
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.apiBase, instID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := a.http.Do(req)
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

// installationForRepo finds the App's installation covering repo.
func (a *appTokenSource) installationForRepo(ctx context.Context, jwt, repo string) (int64, error) {
	url := fmt.Sprintf("%s/repos/%s/installation", a.apiBase, repo)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := a.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return 0, fmt.Errorf("repo installation: %s: %s", resp.Status, body)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.ID, nil
}
