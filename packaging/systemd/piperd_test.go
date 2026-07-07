package systemd

import (
	"os"
	"strings"
	"testing"
)

func TestPiperdServiceContract(t *testing.T) {
	b, err := os.ReadFile("piperd.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(b)

	required := []string{
		"After=docker.service network-online.target",
		"Wants=docker.service network-online.target",
		"ExecStart=/usr/local/bin/piperd",
		"Environment=PIPER_DATA_DIR=/var/lib/piper",
		"Environment=XDG_DATA_HOME=/var/lib/piper",
		"Environment=XDG_CONFIG_HOME=/var/lib/piper",
		"EnvironmentFile=-/etc/piper/piperd.env",
		"DynamicUser=yes",
		"SupplementaryGroups=docker",
		"StateDirectory=piper",
		"AmbientCapabilities=CAP_NET_BIND_SERVICE",
		"CapabilityBoundingSet=CAP_NET_BIND_SERVICE",
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"Restart=on-failure",
		"RestartSec=2s",
		"WantedBy=multi-user.target",
	}
	for _, directive := range required {
		if !strings.Contains(unit, directive) {
			t.Errorf("unit missing %q", directive)
		}
	}
}

func TestPiperdEnvExample(t *testing.T) {
	b, err := os.ReadFile("piperd.env.example")
	if err != nil {
		t.Fatal(err)
	}
	env := string(b)
	for _, text := range []string{
		"PIPER_API_ADDR",
		"PIPER_BASE_DOMAIN",
	} {
		if !strings.Contains(env, text) {
			t.Errorf("env example missing %q", text)
		}
	}
}

func TestPiperdDocumentation(t *testing.T) {
	manual := repositoryFile(t, "docs", "manual-setup.md")
	for _, text := range []string{
		"packaging/systemd/piperd.service",
		"systemctl enable --now piperd",
	} {
		if !strings.Contains(manual, text) {
			t.Errorf("docs/manual-setup.md missing %q", text)
		}
	}

	runbook := repositoryFile(t, "docs", "runbooks", "git-deploy-e2e.md")
	for _, text := range []string{
		"systemctl enable --now piperd",
		"PIPER_DATA_DIR=/var/lib/piper",
		"journalctl -u piperd",
	} {
		if !strings.Contains(runbook, text) {
			t.Errorf("runbook missing %q", text)
		}
	}
}
