package caddy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp"              // http app, server, host matcher
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy" // reverse_proxy handler
	_ "github.com/caddyserver/caddy/v2/modules/caddytls"               // tls app, load_pem
)

// Manager owns the in-process Caddy instance. Caddy is embedded as a library,
// so no external `caddy` binary is required.
type Manager struct{}

type managerOpts struct {
	httpListen  string
	httpsListen string // "" ⇒ no TLS listener
	adminAddr   string
}

// Option configures StartManager.
type Option func(*managerOpts)

// WithHTTPS adds a TLS listener on listen, disables Caddy's automatic HTTPS
// (piperd owns certs), and enables the tls app so load_pem certs are served.
func WithHTTPS(listen string) Option {
	return func(o *managerOpts) { o.httpsListen = listen }
}

// baseConfig builds the Caddy JSON bootstrap config for these options.
func (o *managerOpts) baseConfig() map[string]any {
	listens := []string{o.httpListen}
	piper := map[string]any{"listen": listens, "routes": []any{}}
	apps := map[string]any{"http": map[string]any{"servers": map[string]any{"piper": piper}}}
	if o.httpsListen != "" {
		piper["listen"] = []string{o.httpListen, o.httpsListen}
		piper["automatic_https"] = map[string]any{"disable": true}
		piper["tls_connection_policies"] = []any{map[string]any{}}
		apps["tls"] = map[string]any{"certificates": map[string]any{"load_pem": []any{}}}
	}
	return map[string]any{
		"admin": map[string]any{"listen": o.adminAddr},
		"apps":  apps,
	}
}

// StartManager runs Caddy in-process with an admin-enabled base config: one HTTP
// server named "piper" on httpListen with empty routes. Options can add a TLS
// listener (WithHTTPS). Teardown is via Manager.Stop.
func StartManager(adminBase, httpListen string, opts ...Option) (*Manager, error) {
	o := &managerOpts{httpListen: httpListen, adminAddr: strings.TrimPrefix(adminBase, "http://")}
	for _, opt := range opts {
		opt(o)
	}
	cfg, _ := json.Marshal(o.baseConfig())
	if err := caddy.Load(cfg, true); err != nil {
		return nil, fmt.Errorf("start embedded caddy: %w", err)
	}
	m := &Manager{}
	if err := waitAdmin(adminBase, 10*time.Second); err != nil {
		m.Stop()
		return nil, err
	}
	return m, nil
}

func waitAdmin(base string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/config/")
		if err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("caddy admin API not ready at %s", base)
}

func (m *Manager) Stop() {
	_ = caddy.Stop()
}
