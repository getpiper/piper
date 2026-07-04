// Package config loads piperd runtime configuration from the environment.
package config

import "os"

type Config struct {
	APIAddr    string // control API listen address
	DataDir    string // directory for the SQLite file
	BaseDomain string // apps served at <name>.<BaseDomain>
	CaddyAdmin string // Caddy admin API base URL

	RelayAddr   string // relay tunnel endpoint; empty ⇒ LAN-only (Plan 1)
	RelayToken  string // enrollment token presented to the relay
	ACMEEmail   string // ACME account email
	ACMECA      string // ACME directory URL; empty ⇒ Let's Encrypt production
	DNSProvider string // lego DNS provider name (e.g. "cloudflare")
	TLSCertFile string // static cert path; set ⇒ skip ACME (tests / BYO cert)
	TLSKeyFile  string // static key path
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

		RelayAddr:   env("PIPER_RELAY_ADDR", ""),
		RelayToken:  env("PIPER_RELAY_TOKEN", ""),
		ACMEEmail:   env("PIPER_ACME_EMAIL", ""),
		ACMECA:      env("PIPER_ACME_CA", ""),
		DNSProvider: env("PIPER_DNS_PROVIDER", ""),
		TLSCertFile: env("PIPER_TLS_CERT_FILE", ""),
		TLSKeyFile:  env("PIPER_TLS_KEY_FILE", ""),
	}
}

// ClientAddr returns the piperd base URL used by the piper CLI.
func ClientAddr() string {
	return env("PIPER_ADDR", "http://127.0.0.1:8088")
}
