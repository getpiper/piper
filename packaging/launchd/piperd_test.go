package launchd

import (
	"os"
	"strings"
	"testing"
)

func TestPiperdPlistContract(t *testing.T) {
	b, err := os.ReadFile("com.getpiper.piperd.plist")
	if err != nil {
		t.Fatal(err)
	}
	plist := string(b)
	required := []string{
		"<string>com.getpiper.piperd</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<string>/bin/sh</string>",
		`PIPER_HTTP_ADDR=":8080"`,
		`PIPER_HTTPS_ADDR=":8443"`,
		`XDG_DATA_HOME="$HOME/.piper/piperd"`,
		`$HOME/.piper/piperd.env`,
		`$HOME/.piper/piper.log`,
		"exec /usr/local/bin/piperd",
	}
	for _, s := range required {
		if !strings.Contains(plist, s) {
			t.Errorf("plist missing %q", s)
		}
	}
}

func TestPiperdEnvMacosExample(t *testing.T) {
	b, err := os.ReadFile("piperd.env.macos.example")
	if err != nil {
		t.Fatal(err)
	}
	env := string(b)
	for _, s := range []string{"PIPER_API_ADDR", "PIPER_BASE_DOMAIN", "DOCKER_HOST"} {
		if !strings.Contains(env, s) {
			t.Errorf("env example missing %q", s)
		}
	}
}
