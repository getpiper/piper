package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	oidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// GoogleVerifier brokers Google's OAuth 2.0 device authorization grant and
// verifies the returned ID token. It holds the relay's Google client secret so
// callers never see it. Each Start spawns a background goroutine that blocks on
// Google's polling until the user approves, expires, or the process exits; Poll
// reports progress without blocking.
type GoogleVerifier struct {
	cfg      *oauth2.Config
	verifier *oidc.IDTokenVerifier

	mu    sync.Mutex
	flows map[string]*googleFlow
}

type googleFlow struct {
	done bool
	id   Identity
	err  error
}

func NewGoogleVerifier(ctx context.Context, clientID, clientSecret string) (*GoogleVerifier, error) {
	provider, err := oidc.NewProvider(ctx, "https://accounts.google.com")
	if err != nil {
		return nil, err
	}
	return &GoogleVerifier{
		cfg: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     google.Endpoint,
			Scopes:       []string{oidc.ScopeOpenID, "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
		flows:    map[string]*googleFlow{},
	}, nil
}

func (g *GoogleVerifier) Start(ctx context.Context) (string, DeviceAuth, error) {
	da, err := g.cfg.DeviceAuth(ctx)
	if err != nil {
		return "", DeviceAuth{}, err
	}
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	handle := hex.EncodeToString(raw)

	fl := &googleFlow{}
	g.mu.Lock()
	g.flows[handle] = fl
	g.mu.Unlock()

	// Block on Google's poll loop in the background; DeviceAccessToken honours the
	// server's interval and returns on approval or expiry.
	go func() {
		tok, err := g.cfg.DeviceAccessToken(context.Background(), da)
		id, verr := g.identityFromToken(context.Background(), tok, err)
		g.mu.Lock()
		fl.done, fl.id, fl.err = true, id, verr
		g.mu.Unlock()
	}()

	return handle, DeviceAuth{
		UserCode:        da.UserCode,
		VerificationURI: da.VerificationURI,
		Interval:        int(da.Interval),
		ExpiresIn:       int(time.Until(da.Expiry).Seconds()),
	}, nil
}

func (g *GoogleVerifier) identityFromToken(ctx context.Context, tok *oauth2.Token, tokErr error) (Identity, error) {
	if tokErr != nil {
		return Identity{}, tokErr
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return Identity{}, errors.New("no id_token in token response")
	}
	idt, err := g.verifier.Verify(ctx, rawID)
	if err != nil {
		return Identity{}, err
	}
	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if err := idt.Claims(&claims); err != nil {
		return Identity{}, err
	}
	return Identity{Subject: claims.Sub, Email: claims.Email}, nil
}

func (g *GoogleVerifier) Poll(_ context.Context, handle string) (Identity, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fl, ok := g.flows[handle]
	if !ok {
		return Identity{}, errors.New("unknown handle")
	}
	if !fl.done {
		return Identity{}, ErrAuthPending
	}
	return fl.id, fl.err
}
