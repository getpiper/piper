package systemd

import (
	"os"
	"strings"
	"testing"
)

func TestPiperRelayServiceContract(t *testing.T) {
	b, err := os.ReadFile("piper-relay.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(b)

	required := []string{
		"After=network-online.target",
		"Wants=network-online.target",
		"ExecStart=/usr/local/bin/piper-relay",
		"Environment=PIPER_RELAY_DATA_DIR=/var/lib/piper-relay",
		"EnvironmentFile=-/etc/piper-relay.env",
		"DynamicUser=yes",
		"StateDirectory=piper-relay",
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
