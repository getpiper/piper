// Package caddy drives a running Caddy instance through its JSON admin API.
package caddy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

type Client struct {
	base string
	http *http.Client
}

func NewClient(adminBase string) *Client {
	return &Client{base: adminBase, http: &http.Client{}}
}

func routeID(host string) string { return "piper-" + host }

// UpsertRoute makes Caddy reverse-proxy host to 127.0.0.1:<port> over HTTP.
// The route carries a stable @id so re-deploys replace it: existing route is
// removed by id (404 ignored), then a fresh one is appended.
func (c *Client) UpsertRoute(host string, upstreamHostPort int) error {
	route := map[string]any{
		"@id":   routeID(host),
		"match": []map[string]any{{"host": []string{host}}},
		"handle": []map[string]any{{
			"handler":   "reverse_proxy",
			"upstreams": []map[string]any{{"dial": fmt.Sprintf("127.0.0.1:%d", upstreamHostPort)}},
		}},
	}
	if err := c.RemoveRoute(host); err != nil {
		return err
	}
	body, _ := json.Marshal(route)
	url := c.base + "/config/apps/http/servers/piper/routes"
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("caddy upsert route: status %d", resp.StatusCode)
	}
	return nil
}

// RemoveRoute deletes the route addressed by the host's stable id.
// A missing route (404) is not an error.
func (c *Client) RemoveRoute(host string) error {
	url := c.base + "/id/" + routeID(host)
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("caddy remove route: status %d", resp.StatusCode)
}

// LoadCert appends a PEM cert/key pair to Caddy's tls.certificates.load_pem so
// Caddy serves it for matching SNI. Requires the tls app to exist (WithHTTPS).
func (c *Client) LoadCert(certPEM, keyPEM string) error {
	body, _ := json.Marshal(map[string]string{"certificate": certPEM, "key": keyPEM})
	url := c.base + "/config/apps/tls/certificates/load_pem"
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("caddy load cert: status %d", resp.StatusCode)
	}
	return nil
}

// ReplaceCert replaces Caddy's complete load_pem certificate list with one
// cert/key pair. Renewal uses this instead of appending duplicate entries.
func (c *Client) ReplaceCert(certPEM, keyPEM string) error {
	body, _ := json.Marshal([]map[string]string{{
		"certificate": certPEM,
		"key":         keyPEM,
	}})
	url := c.base + "/config/apps/tls/certificates/load_pem"
	req, _ := http.NewRequest(http.MethodPatch, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("caddy replace cert: status %d", resp.StatusCode)
	}
	return nil
}
