// Package client is the piper CLI's HTTP client to piperd.
package client

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/getpiper/piper/internal/api"
	"github.com/getpiper/piper/internal/store"
)

type Client struct {
	base  string
	token string
	http  *http.Client
}

func New(base, token string) *Client {
	if base == "" {
		base = "http://127.0.0.1:8088"
	}
	return &Client{base: base, token: token, http: &http.Client{}}
}

// do builds a request to c.base+path, attaches the auth header (when set) and
// the content type (when non-empty), and sends it.
func (c *Client) do(method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return c.http.Do(req)
}

func (c *Client) CreateApp(name string, port int) error {
	body, err := json.Marshal(map[string]any{"name": name, "port": port})
	if err != nil {
		return err
	}
	resp, err := c.do(http.MethodPost, "/v1/apps", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return responseError("create app", resp)
	}
	return nil
}

func (c *Client) ListApps() ([]api.App, error) {
	resp, err := c.do(http.MethodGet, "/v1/apps", "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, responseError("list apps", resp)
	}
	var apps []api.App
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return nil, err
	}
	return apps, nil
}

// Liveness reports the relay's view of the box: whether its tunnel session is
// currently connected. It GETs the client's base path itself, which on a
// remote client is the relay's /agents/<base-domain> resource — it has no
// meaning against a local piperd address.
type Liveness struct {
	Agent     string `json:"agent"`
	Connected bool   `json:"connected"`
}

func (c *Client) Liveness() (Liveness, error) {
	resp, err := c.do(http.MethodGet, "", "", nil)
	if err != nil {
		return Liveness{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return Liveness{}, responseError("liveness", resp)
	}
	var l Liveness
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		return Liveness{}, err
	}
	return l, nil
}

func (c *Client) Deploy(name, srcDir string) (store.Deployment, error) {
	var body bytes.Buffer
	if err := TarDir(srcDir, &body); err != nil {
		return store.Deployment{}, err
	}
	resp, err := c.do(http.MethodPost, "/v1/apps/"+name+"/deploy", "application/x-tar", &body)
	if err != nil {
		return store.Deployment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return store.Deployment{}, responseError("deploy", resp)
	}
	var dep store.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&dep); err != nil {
		return store.Deployment{}, err
	}
	return dep, nil
}

func (c *Client) LinkApp(name, repo, branch string) error {
	body, _ := json.Marshal(map[string]string{"repo": repo, "branch": branch})
	resp, err := c.do(http.MethodPost, "/v1/apps/"+name+"/link", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return responseError("link", resp)
	}
	return nil
}

func (c *Client) StopApp(name string) error {
	resp, err := c.do(http.MethodPost, "/v1/apps/"+name+"/stop", "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return responseError("stop", resp)
	}
	return nil
}

func (c *Client) DeleteApp(name string) error {
	resp, err := c.do(http.MethodDelete, "/v1/apps/"+name, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return responseError("delete", resp)
	}
	return nil
}

func (c *Client) Manifest(redirectURL string) (string, error) {
	body, _ := json.Marshal(map[string]string{"redirect_url": redirectURL})
	resp, err := c.do(http.MethodPost, "/v1/github/manifest", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", responseError("manifest", resp)
	}
	var out struct {
		Manifest string `json:"manifest"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Manifest, nil
}

func (c *Client) ExchangeGitHub(code string) error {
	body, _ := json.Marshal(map[string]string{"code": code})
	resp, err := c.do(http.MethodPost, "/v1/github/exchange", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return responseError("exchange", resp)
	}
	return nil
}

func responseError(action string, resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: %s: read response: %w", action, resp.Status, err)
	}
	return fmt.Errorf("%s: %s: %s", action, resp.Status, strings.TrimSpace(string(body)))
}

// TarDir writes the regular files under dir to w using relative, slash-separated names.
func TarDir(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	closeErr := tw.Close()
	if walkErr != nil {
		return walkErr
	}
	return closeErr
}
