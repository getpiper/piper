package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

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
	jwt, err := a.appJWT(time.Now())
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

// appJWT mints a short-lived GitHub App JWT (RS256) signed with the app key.
func (a *appTokenSource) appJWT(now time.Time) (string, error) {
	header := b64url([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"%d"}`,
		now.Add(-30*time.Second).Unix(), now.Add(9*time.Minute).Unix(), a.appID)
	signingInput := header + "." + b64url([]byte(claims))
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64url(sig), nil
}
