package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("PIPER_API_ADDR", "")
	// Isolate the data dir so Load() can't read the developer's real
	// ~/.piper relay.json (an enrolled box's apex would shadow the default).
	t.Setenv("PIPER_DATA_DIR", t.TempDir())
	t.Setenv("PIPER_BASE_DOMAIN", "")
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
	t.Setenv("PIPER_DATA_DIR", t.TempDir())
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
	t.Setenv("PIPER_DATA_DIR", t.TempDir())
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

func TestDefaultDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PIPER_DATA_DIR", "")
	got := Load().DataDir
	want := filepath.Join(home, ".piper", "piperd")
	if got != want {
		t.Fatalf("DataDir = %q, want %q", got, want)
	}
}

func TestLoadClientDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Addr != "http://127.0.0.1:8088" || cc.Token != "" {
		t.Fatalf("cc = %+v", cc)
	}
}

func TestSaveAndLoadClient(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	if err := SaveClient(ClientConfig{Addr: "http://box:8088", Token: "secret"}); err != nil {
		t.Fatal(err)
	}
	cc, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Addr != "http://box:8088" || cc.Token != "secret" {
		t.Fatalf("cc = %+v", cc)
	}
}

func TestLoadClientEnvOverridesFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	if err := SaveClient(ClientConfig{Addr: "http://box:8088", Token: "filetok"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPER_TOKEN", "envtok")
	cc, _ := LoadClient()
	if cc.Token != "envtok" {
		t.Fatalf("token = %q, want envtok", cc.Token)
	}
}

func TestClientConfigRoundTripsRelayFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPER_ADDR", "")
	t.Setenv("PIPER_TOKEN", "")
	if err := SaveClient(ClientConfig{
		Addr: "http://127.0.0.1:8088", RelayAPI: "https://api.public.getpiper.co",
		AccountCredential: "cred-xyz",
	}); err != nil {
		t.Fatalf("SaveClient: %v", err)
	}
	cc, err := LoadClient()
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if cc.RelayAPI != "https://api.public.getpiper.co" || cc.AccountCredential != "cred-xyz" {
		t.Fatalf("cc = %+v", cc)
	}
}

func TestRelayFileRoundTripAndMissing(t *testing.T) {
	dir := t.TempDir()
	if _, found, err := LoadRelayFile(dir); err != nil || found {
		t.Fatalf("missing relay file: found=%v err=%v", found, err)
	}
	rf := RelayFile{RelayAddr: "relay:7000", RelayToken: "enr-1", BaseDomain: "ab12-alice.public.getpiper.co"}
	if err := SaveRelayFile(dir, rf); err != nil {
		t.Fatalf("SaveRelayFile: %v", err)
	}
	got, found, err := LoadRelayFile(dir)
	if err != nil || !found {
		t.Fatalf("LoadRelayFile: found=%v err=%v", found, err)
	}
	if got != rf {
		t.Fatalf("relay file = %+v, want %+v", got, rf)
	}
}

func TestLoadReadsRelayFileWhenEnvUnset(t *testing.T) {
	dir := t.TempDir()
	if err := SaveRelayFile(dir, RelayFile{
		RelayAddr: "relay:7000", RelayToken: "enr-1", BaseDomain: "ab12-alice.public.getpiper.co",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPER_DATA_DIR", dir)
	t.Setenv("PIPER_RELAY_ADDR", "")
	t.Setenv("PIPER_RELAY_TOKEN", "")
	t.Setenv("PIPER_BASE_DOMAIN", "")
	cfg := Load()
	if cfg.RelayAddr != "relay:7000" || cfg.RelayToken != "enr-1" ||
		cfg.BaseDomain != "ab12-alice.public.getpiper.co" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadEnvOverridesRelayFile(t *testing.T) {
	dir := t.TempDir()
	if err := SaveRelayFile(dir, RelayFile{
		RelayAddr: "relay:7000", RelayToken: "enr-1", BaseDomain: "ab12-alice.public.getpiper.co",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPER_DATA_DIR", dir)
	t.Setenv("PIPER_RELAY_ADDR", "override:9000")
	t.Setenv("PIPER_RELAY_TOKEN", "")
	t.Setenv("PIPER_BASE_DOMAIN", "")
	cfg := Load()
	if cfg.RelayAddr != "override:9000" {
		t.Fatalf("RelayAddr = %q, want env override", cfg.RelayAddr)
	}
	if cfg.RelayToken != "enr-1" { // env unset ⇒ file value
		t.Fatalf("RelayToken = %q, want file value", cfg.RelayToken)
	}
}

func TestSystemManaged(t *testing.T) {
	old := SystemEnvDir
	defer func() { SystemEnvDir = old }()

	dir := t.TempDir()
	SystemEnvDir = dir
	if !SystemManaged() {
		t.Fatal("SystemManaged = false with an existing /etc/piper, want true")
	}
	if got, want := SystemEnvFile(), filepath.Join(dir, "piperd.env"); got != want {
		t.Fatalf("SystemEnvFile = %q, want %q", got, want)
	}

	SystemEnvDir = filepath.Join(dir, "absent")
	if SystemManaged() {
		t.Fatal("SystemManaged = true with an absent dir, want false")
	}
}

func TestLoadIgnoresCorruptRelayFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "relay.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPER_DATA_DIR", dir)
	t.Setenv("PIPER_RELAY_ADDR", "")
	t.Setenv("PIPER_RELAY_TOKEN", "")
	t.Setenv("PIPER_BASE_DOMAIN", "")
	cfg := Load() // must not panic; degrades to zero relay values + default domain
	if cfg.RelayAddr != "" || cfg.RelayToken != "" {
		t.Fatalf("corrupt relay.json leaked values: %+v", cfg)
	}
	if cfg.BaseDomain != "piper.localhost" {
		t.Fatalf("BaseDomain = %q, want default", cfg.BaseDomain)
	}
}

func TestRelayFileTerminatedRoundTripAndLoad(t *testing.T) {
	dir := t.TempDir()
	if err := SaveRelayFile(dir, RelayFile{
		RelayAddr: "relay:7000", RelayToken: "enr-1",
		BaseDomain: "aaaa-alice.public.getpiper.co", Terminated: true,
	}); err != nil {
		t.Fatal(err)
	}
	got, _, err := LoadRelayFile(dir)
	if err != nil || !got.Terminated {
		t.Fatalf("relay file terminated = %v (err %v)", got.Terminated, err)
	}

	t.Setenv("PIPER_DATA_DIR", dir)
	t.Setenv("PIPER_RELAY_ADDR", "")
	t.Setenv("PIPER_RELAY_TOKEN", "")
	t.Setenv("PIPER_BASE_DOMAIN", "")
	t.Setenv("PIPER_RELAY_TERMINATED", "")
	if cfg := Load(); !cfg.Terminated {
		t.Fatal("Load did not read terminated from relay.json")
	}
	t.Setenv("PIPER_RELAY_TERMINATED", "0")
	// An explicit env value of 0 should win over the file's true only if we treat
	// env as authoritative; keep the rule simple: env "1" forces on, otherwise the
	// file decides. Document via this assertion:
	if cfg := Load(); !cfg.Terminated {
		t.Fatal("non-1 env must not override a terminated relay.json")
	}
}

func TestLoadClientFileMigratesLegacyFlat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".piper", "piper")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"addr":"http://192.168.1.6:8088","token":"tok","relay_api":"https://api.relay","account_credential":"cred"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	cf, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.Boxes) != 1 || cf.Current != "default" {
		t.Fatalf("want 1 box current=default, got %+v", cf)
	}
	b := cf.Boxes[0]
	if b.Name != "default" || b.Addr != "http://192.168.1.6:8088" || b.Token != "tok" ||
		b.RelayAPI != "https://api.relay" || b.AccountCredential != "cred" {
		t.Fatalf("migrated box wrong: %+v", b)
	}
}

func TestLoadClientFileMissingIsEmptyNotError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cf, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.Boxes) != 0 {
		t.Fatalf("want no boxes, got %+v", cf)
	}
}

func TestSaveClientFileRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	in := ClientFile{
		Boxes: []Box{
			{Name: "pi4", Addr: "http://192.168.1.6:8088", Token: "a"},
			{Name: "local", Addr: "http://127.0.0.1:8088", Token: "b", RelayAPI: "https://api.relay", AccountCredential: "cred"},
		},
		Current: "pi4",
	}
	if err := SaveClientFile(in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadClientFile()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch:\n in: %+v\nout: %+v", in, out)
	}
}

func TestCurrentBoxFallsBackToFirst(t *testing.T) {
	cf := ClientFile{Boxes: []Box{{Name: "a"}, {Name: "b"}}, Current: "missing"}
	b, ok := cf.CurrentBox()
	if !ok || b.Name != "a" {
		t.Fatalf("want first box fallback, got %+v ok=%v", b, ok)
	}
	if _, ok := (ClientFile{}).CurrentBox(); ok {
		t.Fatal("empty file must report no box")
	}
}
