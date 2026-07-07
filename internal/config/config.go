// Package config loads piperd runtime configuration from the environment.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type Config struct {
	APIAddr     string // control API listen address
	WebhookAddr string // loopback webhook listener (relay mode)
	DataDir     string // directory for the SQLite file
	BaseDomain  string // apps served at <name>.<BaseDomain>
	CaddyAdmin  string // Caddy admin API base URL

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

// Load builds a Config from env vars and the persisted relay.json, applying
// defaults. Env vars override relay.json, which overrides built-in defaults.
func Load() Config {
	dataDir := env("PIPER_DATA_DIR", DefaultDataDir())
	rf, _, _ := LoadRelayFile(dataDir) // best-effort: a corrupt file yields zero values

	return Config{
		APIAddr:     env("PIPER_API_ADDR", "127.0.0.1:8088"),
		WebhookAddr: env("PIPER_WEBHOOK_ADDR", "127.0.0.1:8089"),
		DataDir:     dataDir,
		BaseDomain:  firstNonEmpty(os.Getenv("PIPER_BASE_DOMAIN"), rf.BaseDomain, "piper.localhost"),
		CaddyAdmin:  env("PIPER_CADDY_ADMIN", "http://127.0.0.1:2019"),

		RelayAddr:   firstNonEmpty(os.Getenv("PIPER_RELAY_ADDR"), rf.RelayAddr),
		RelayToken:  firstNonEmpty(os.Getenv("PIPER_RELAY_TOKEN"), rf.RelayToken),
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

// defaultDataDir is piperd's SQLite home when PIPER_DATA_DIR is unset:
// ~/.piper/piperd. Falls back to ./data if the home dir can't be resolved.
func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "./data"
	}
	return filepath.Join(home, ".piper", "piperd")
}

// DefaultDataDir is piperd's data-dir default (~/.piper/piperd) when
// PIPER_DATA_DIR is unset. `piper connect` reuses it to write relay.json to the
// same place piperd reads it.
func DefaultDataDir() string { return defaultDataDir() }

// ClientConfig is the piper CLI's saved credentials/target. Addr/Token are the
// LAN path (bearer to piperd); RelayAPI/AccountCredential are the relay path
// (device-flow login), written by `piper login` and read by `piper connect`.
type ClientConfig struct {
	Addr              string `json:"addr"`
	Token             string `json:"token"`
	RelayAPI          string `json:"relay_api,omitempty"`
	AccountCredential string `json:"account_credential,omitempty"`
}

func clientConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".piper", "piper", "config.json"), nil
}

// LoadClient reads ~/.piper/piper/config.json, then applies PIPER_ADDR /
// PIPER_TOKEN env overrides and the localhost default for Addr. A missing file
// is not an error.
func LoadClient() (ClientConfig, error) {
	var cc ClientConfig
	path, err := clientConfigPath()
	if err != nil {
		return cc, err
	}
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, &cc)
	} else if !errors.Is(err, os.ErrNotExist) {
		return cc, err
	}
	if v := os.Getenv("PIPER_ADDR"); v != "" {
		cc.Addr = v
	}
	if cc.Addr == "" {
		cc.Addr = "http://127.0.0.1:8088"
	}
	if v := os.Getenv("PIPER_TOKEN"); v != "" {
		cc.Token = v
	}
	return cc, nil
}

// SaveClient writes cc to ~/.piper/piper/config.json with 0600 perms, creating
// the directory if needed.
func SaveClient(cc ClientConfig) error {
	path, err := clientConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// RelayFile is the persisted relay enrollment written by `piper connect` and
// read by piperd at startup. Environment variables override these values.
type RelayFile struct {
	RelayAddr  string `json:"relay_addr"`
	RelayToken string `json:"relay_token"`
	BaseDomain string `json:"base_domain"`
}

func relayFilePath(dataDir string) string { return filepath.Join(dataDir, "relay.json") }

// SaveRelayFile writes rf to <dataDir>/relay.json with 0600 perms, creating the
// directory if needed.
func SaveRelayFile(dataDir string, rf RelayFile) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(relayFilePath(dataDir), data, 0o600)
}

// LoadRelayFile reads <dataDir>/relay.json. A missing file is not an error:
// found is false and rf is the zero value.
func LoadRelayFile(dataDir string) (RelayFile, bool, error) {
	var rf RelayFile
	data, err := os.ReadFile(relayFilePath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return rf, false, nil
	}
	if err != nil {
		return rf, false, err
	}
	if err := json.Unmarshal(data, &rf); err != nil {
		return rf, false, err
	}
	return rf, true, nil
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
