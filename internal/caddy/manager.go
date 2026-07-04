package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

type Manager struct{ cmd *exec.Cmd }

// StartManager launches `caddy run` with an admin-enabled base config that has
// one HTTP server named "piper" listening on httpListen with an empty routes list.
func StartManager(ctx context.Context, adminBase, httpListen string) (*Manager, error) {
	adminAddr := strings.TrimPrefix(adminBase, "http://")
	base := map[string]any{
		"admin": map[string]any{"listen": adminAddr},
		"apps": map[string]any{"http": map[string]any{"servers": map[string]any{
			"piper": map[string]any{"listen": []string{httpListen}, "routes": []any{}},
		}}},
	}
	cfg, _ := json.Marshal(base)
	cmd := exec.CommandContext(ctx, "caddy", "run", "--config", "-", "--adapter", "")
	cmd.Stdin = bytes.NewReader(cfg)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start caddy (is it installed?): %w", err)
	}
	m := &Manager{cmd: cmd}
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
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
		_ = m.cmd.Wait()
	}
}
