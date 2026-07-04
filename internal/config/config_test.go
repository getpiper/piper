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
