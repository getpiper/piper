// Package config loads piperd runtime configuration from the environment.
package config

import "os"

type Config struct {
	APIAddr    string // control API listen address
	DataDir    string // directory for the SQLite file
	BaseDomain string // apps served at <name>.<BaseDomain>
	CaddyAdmin string // Caddy admin API base URL
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load builds a Config from env vars, applying defaults.
func Load() Config {
	return Config{
		APIAddr:    env("PIPER_API_ADDR", "127.0.0.1:8088"),
		DataDir:    env("PIPER_DATA_DIR", "./data"),
		BaseDomain: env("PIPER_BASE_DOMAIN", "piper.localhost"),
		CaddyAdmin: env("PIPER_CADDY_ADMIN", "http://127.0.0.1:2019"),
	}
}
