// Package relayclient is the piper CLI's HTTP client for the relay control API:
// the GitHub device-flow login and account-bound box enrollment. It is the
// CLI-side counterpart of the relay's internal/relay control handlers.
package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DeviceAuth is the relay's response to starting a device-flow login.
type DeviceAuth struct {
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	DeviceCode      string `json:"device_code"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

// Account is the completed-login result: a relay account credential + username.
// InstallURL is the relay's GitHub App install-and-authorize page; empty when
// the relay has no App configured (or the App has no slug), in which case
// login ends without an install step.
type Account struct {
	AccountCredential string `json:"account_credential"`
	Username          string `json:"username"`
	InstallURL        string `json:"install_url"`
}

// Enrollment is the result of claiming a box: an enrollment token, the assigned
// base domain, and the relay tunnel endpoint the box should dial.
type Enrollment struct {
	EnrollmentToken string `json:"enrollment_token"`
	BaseDomain      string `json:"base_domain"`
	TunnelEndpoint  string `json:"tunnel_endpoint"`
	WebhookSecret   string `json:"webhook_secret"`
	GitHubApp       bool   `json:"github_app"`
}

// ErrAuthPending means the user has not yet completed the device flow.
var ErrAuthPending = errors.New("authorization_pending")

// ErrBadCredential means the relay rejected the account credential (unknown or
// a disabled account).
var ErrBadCredential = errors.New("relay rejected account credential")

// ErrQuotaExceeded means the account is already at its agent cap.
var ErrQuotaExceeded = errors.New("account agent quota exceeded")

// ErrNoInstallation means the relay's GitHub App is not installed for this
// account yet — the caller should keep polling, not fail.
var ErrNoInstallation = errors.New("github app not installed for this account")

// Client talks to a relay's control API rooted at base.
type Client struct {
	base string
	http *http.Client
}

// New returns a Client for the relay control API at base (e.g.
// https://api.public.getpiper.dev).
func New(base string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) post(ctx context.Context, path string, body any, auth string) (*http.Response, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	return c.http.Do(req)
}

// LoginDevice starts a device-flow login and returns the user code + URL to show.
func (c *Client) LoginDevice(ctx context.Context) (DeviceAuth, error) {
	resp, err := c.post(ctx, "/v1/login/device", nil, "")
	if err != nil {
		return DeviceAuth{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return DeviceAuth{}, fmt.Errorf("relay device login: %s", resp.Status)
	}
	var da DeviceAuth
	if err := json.NewDecoder(resp.Body).Decode(&da); err != nil {
		return DeviceAuth{}, err
	}
	return da, nil
}

// LoginPoll polls once for completion of the device flow. It returns
// ErrAuthPending while the user has not finished, or the Account on success.
func (c *Client) LoginPoll(ctx context.Context, deviceCode string) (Account, error) {
	resp, err := c.post(ctx, "/v1/login/poll", map[string]string{"device_code": deviceCode}, "")
	if err != nil {
		return Account{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var acc Account
		if err := json.NewDecoder(resp.Body).Decode(&acc); err != nil {
			return Account{}, err
		}
		return acc, nil
	case http.StatusAccepted:
		return Account{}, ErrAuthPending
	default:
		return Account{}, fmt.Errorf("relay login poll: %s", resp.Status)
	}
}

// Enroll claims a box for the account behind accountCredential, returning the
// enrollment token, assigned base domain, and tunnel endpoint.
func (c *Client) Enroll(ctx context.Context, accountCredential string) (Enrollment, error) {
	resp, err := c.post(ctx, "/v1/enroll", nil, accountCredential)
	if err != nil {
		return Enrollment{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var en Enrollment
		if err := json.NewDecoder(resp.Body).Decode(&en); err != nil {
			return Enrollment{}, err
		}
		return en, nil
	case http.StatusUnauthorized:
		return Enrollment{}, ErrBadCredential
	case http.StatusTooManyRequests:
		return Enrollment{}, ErrQuotaExceeded
	default:
		return Enrollment{}, fmt.Errorf("relay enroll: %s", resp.Status)
	}
}

// GitHubRepos lists the repositories the account's GitHub App installation can
// reach. A relay 404 (not installed yet) maps to ErrNoInstallation so a poll
// loop can retry on that specific condition and fail fast on everything else.
func (c *Client) GitHubRepos(ctx context.Context, accountCredential string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/github/repos", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accountCredential)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var body struct {
			Repos []string `json:"repos"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, err
		}
		return body.Repos, nil
	case http.StatusNotFound:
		return nil, ErrNoInstallation
	default:
		return nil, fmt.Errorf("relay github repos: %s", resp.Status)
	}
}
