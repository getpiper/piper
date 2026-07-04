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

	"github.com/getpiper/piper/internal/store"
)

type Client struct {
	base string
	http *http.Client
}

func New(base string) *Client {
	if base == "" {
		base = "http://127.0.0.1:8088"
	}
	return &Client{base: base, http: &http.Client{}}
}

func (c *Client) CreateApp(name string, port int) error {
	body, err := json.Marshal(map[string]any{"name": name, "port": port})
	if err != nil {
		return err
	}
	resp, err := c.http.Post(c.base+"/v1/apps", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return responseError("create app", resp)
	}
	return nil
}

func (c *Client) ListApps() ([]store.App, error) {
	resp, err := c.http.Get(c.base + "/v1/apps")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, responseError("list apps", resp)
	}
	var apps []store.App
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return nil, err
	}
	return apps, nil
}

func (c *Client) Deploy(name, srcDir string) (store.Deployment, error) {
	var body bytes.Buffer
	if err := TarDir(srcDir, &body); err != nil {
		return store.Deployment{}, err
	}
	resp, err := c.http.Post(c.base+"/v1/apps/"+name+"/deploy", "application/x-tar", &body)
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
