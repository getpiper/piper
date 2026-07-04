package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("PIPER_API_ADDR", "")
	t.Setenv("PIPER_DATA_DIR", "")
	c := Load()
	if c.APIAddr != "127.0.0.1:8088" {
		t.Errorf("APIAddr = %q, want 127.0.0.1:8088", c.APIAddr)
	}
	if c.BaseDomain != "piper.localhost" {
		t.Errorf("BaseDomain = %q, want piper.localhost", c.BaseDomain)
	}
	if c.CaddyAdmin != "http://127.0.0.1:2019" {
		t.Errorf("CaddyAdmin = %q", c.CaddyAdmin)
	}
}

func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("PIPER_API_ADDR", "0.0.0.0:9000")
	if got := Load().APIAddr; got != "0.0.0.0:9000" {
		t.Errorf("APIAddr = %q, want 0.0.0.0:9000", got)
	}
}

func TestClientAddr(t *testing.T) {
	t.Setenv("PIPER_ADDR", "")
	if got := ClientAddr(); got != "http://127.0.0.1:8088" {
		t.Errorf("default ClientAddr = %q", got)
	}
	t.Setenv("PIPER_ADDR", "http://piper.test:9000")
	if got := ClientAddr(); got != "http://piper.test:9000" {
		t.Errorf("configured ClientAddr = %q", got)
	}
}

func TestLoadRelayFields(t *testing.T) {
	t.Setenv("PIPER_RELAY_ADDR", "relay.example.com:7000")
	t.Setenv("PIPER_RELAY_TOKEN", "tok-xyz")
	t.Setenv("PIPER_ACME_EMAIL", "me@example.com")
	cfg := Load()
	if cfg.RelayAddr != "relay.example.com:7000" {
		t.Errorf("RelayAddr = %q", cfg.RelayAddr)
	}
	if cfg.RelayToken != "tok-xyz" {
		t.Errorf("RelayToken = %q", cfg.RelayToken)
	}
	if cfg.ACMEEmail != "me@example.com" {
		t.Errorf("ACMEEmail = %q", cfg.ACMEEmail)
	}
}
