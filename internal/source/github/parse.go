package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/getpiper/piper/internal/source"
)

func (p *Provider) verify(headers http.Header, body []byte) error {
	sig := headers.Get("X-Hub-Signature-256")
	m := hmac.New(sha256.New, []byte(p.secret))
	m.Write(body)
	want := "sha256=" + hex.EncodeToString(m.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return source.ErrBadSignature
	}
	return nil
}

func (p *Provider) Parse(headers http.Header, body []byte) (source.Event, error) {
	if err := p.verify(headers, body); err != nil {
		return source.Event{}, err
	}
	var payload struct {
		Ref         string `json:"ref"`
		After       string `json:"after"`
		Action      string `json:"action"`
		Number      int    `json:"number"`
		PullRequest struct {
			Head struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return source.Event{}, fmt.Errorf("parse payload: %w", err)
	}
	ev := source.Event{
		Repo:           payload.Repository.FullName,
		InstallationID: payload.Installation.ID,
	}
	switch headers.Get("X-GitHub-Event") {
	case "ping":
		ev.Kind = source.KindPing
	case "push":
		ev.Kind = source.KindPush
		ev.Ref = payload.Ref
		ev.SHA = payload.After
	case "pull_request":
		ev.PR = payload.Number
		ev.Ref = payload.PullRequest.Head.Ref
		ev.SHA = payload.PullRequest.Head.SHA
		switch payload.Action {
		case "opened", "reopened":
			ev.Kind = source.KindPROpened
		case "synchronize":
			ev.Kind = source.KindPRSynced
		case "closed":
			ev.Kind = source.KindPRClosed
		default:
			ev.Kind = source.KindOther
		}
	default:
		ev.Kind = source.KindOther
	}
	return ev, nil
}
