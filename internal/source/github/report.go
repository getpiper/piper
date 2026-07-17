package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/getpiper/piper/internal/source"
)

func (p *Provider) Report(ctx context.Context, ev source.Event, status source.Status, url string) error {
	token, err := p.installationToken(ctx, ev.InstallationID)
	if err != nil {
		return err
	}
	if status == source.StatusPending {
		_, err := p.createDeployment(ctx, token, ev)
		return err
	}
	id, err := p.latestDeploymentID(ctx, token, ev)
	if err != nil {
		return err
	}
	state := "failure"
	switch status {
	case source.StatusSuccess:
		state = "success"
	case source.StatusInactive:
		state = "inactive"
	}
	return p.postStatus(ctx, token, ev.Repo, id, state, url)
}

func (p *Provider) do(ctx context.Context, method, url, token string, in any, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, url, body)
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, b)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (p *Provider) createDeployment(ctx context.Context, token string, ev source.Event) (int64, error) {
	in := map[string]any{
		"ref":               ev.SHA,
		"environment":       "production",
		"auto_merge":        false,
		"required_contexts": []string{},
		"description":       "piper deploy",
	}
	if ev.PR > 0 {
		in["environment"] = fmt.Sprintf("pr-%d", ev.PR)
		in["transient_environment"] = true
	}
	var out struct {
		ID int64 `json:"id"`
	}
	err := p.do(ctx, http.MethodPost, p.apiBase+"/repos/"+ev.Repo+"/deployments", token, in, &out)
	return out.ID, err
}

func (p *Provider) latestDeploymentID(ctx context.Context, token string, ev source.Event) (int64, error) {
	var out []struct {
		ID int64 `json:"id"`
	}
	// One SHA can carry both a pr-<N> and a production deployment (a PR-preview
	// SHA and its post-merge production deploy). Filtering on ?sha alone could
	// post a status to whichever the API lists first, so scope both PR and
	// non-PR events to their own environment (mirrors createDeployment's
	// pr-<N>/production split).
	url := p.apiBase + "/repos/" + ev.Repo + "/deployments?sha=" + ev.SHA
	if ev.PR > 0 {
		url += fmt.Sprintf("&environment=pr-%d", ev.PR)
	} else {
		url += "&environment=production"
	}
	err := p.do(ctx, http.MethodGet, url, token, nil, &out)
	if err != nil {
		return 0, err
	}
	if len(out) == 0 {
		return 0, fmt.Errorf("no deployment for sha %s", ev.SHA)
	}
	return out[0].ID, nil
}

func (p *Provider) postStatus(ctx context.Context, token, repo string, id int64, state, url string) error {
	in := map[string]any{"state": state}
	if url != "" {
		in["environment_url"] = url
	}
	endpoint := fmt.Sprintf("%s/repos/%s/deployments/%d/statuses", p.apiBase, repo, id)
	return p.do(ctx, http.MethodPost, endpoint, token, in, nil)
}
