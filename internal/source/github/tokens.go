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

	"github.com/getpiper/piper/internal/ghjwt"
	"github.com/getpiper/piper/internal/source"
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
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.apiBase, ev.InstallationID)
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
