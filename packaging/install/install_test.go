package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// scriptPath returns the repo-root install.sh, found by walking up to go.mod.
func scriptPath(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "install.sh")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test dir")
		}
		dir = parent
	}
}

// hostOSArch maps the running machine to goreleaser's os/arch tokens.
func hostOSArch() (string, string) {
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	return runtime.GOOS, arch
}

// tarGz wraps a single named file (mode 0755) in a gzipped tar.
func tarGz(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newReleaseServer serves assets under /{repo}/releases/download/{tag}/{file}
// and synthesises a checksums.txt from them. checksumsOverride, when non-nil,
// replaces the computed checksums.txt body (to simulate corruption).
func newReleaseServer(t *testing.T, assets map[string][]byte, checksumsOverride []byte) *httptest.Server {
	t.Helper()
	var sums strings.Builder
	for name, body := range assets {
		sum := sha256.Sum256(body)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	body := []byte(sums.String())
	if checksumsOverride != nil {
		body = checksumsOverride
	}
	assets["checksums.txt"] = body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := filepath.Base(r.URL.Path)
		if b, ok := assets[base]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// run executes install.sh with the given args and env overlay.
func run(t *testing.T, args []string, env map[string]string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available")
	}
	cmd := exec.Command("sh", append([]string{scriptPath(t)}, args...)...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestCLIOnlyInstall(t *testing.T) {
	osTok, archTok := hostOSArch()
	tag := "v9.9.9"
	archive := fmt.Sprintf("piper_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), osTok, archTok)
	assets := map[string][]byte{archive: tarGz(t, "piper", "#!/bin/sh\necho fake-piper\n")}
	srv := newReleaseServer(t, assets, nil)

	prefix := t.TempDir()
	out, err := run(t, []string{"--cli-only", "--no-enable"}, map[string]string{
		"PIPER_REPO":     "getpiper/piper",
		"PIPER_BASE_URL": srv.URL,
		"PIPER_VERSION":  tag,
		"PIPER_PREFIX":   prefix,
	})
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	info, err := os.Stat(filepath.Join(prefix, "piper"))
	if err != nil {
		t.Fatalf("piper not installed: %v\n%s", err, out)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("piper not executable: %v", info.Mode())
	}
}

func TestChecksumMismatchAborts(t *testing.T) {
	osTok, archTok := hostOSArch()
	tag := "v9.9.9"
	archive := fmt.Sprintf("piper_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), osTok, archTok)
	assets := map[string][]byte{archive: tarGz(t, "piper", "real-bytes")}
	// checksums.txt claims a bogus hash for the archive.
	bogus := []byte(fmt.Sprintf("%064d  %s\n", 0, archive))
	srv := newReleaseServer(t, assets, bogus)

	prefix := t.TempDir()
	out, err := run(t, []string{"--cli-only", "--no-enable"}, map[string]string{
		"PIPER_BASE_URL": srv.URL,
		"PIPER_VERSION":  tag,
		"PIPER_PREFIX":   prefix,
	})
	if err == nil {
		t.Fatalf("expected non-zero exit on checksum mismatch, got success:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(prefix, "piper")); statErr == nil {
		t.Error("piper was installed despite checksum mismatch")
	}
	if !strings.Contains(out, "checksum mismatch") {
		t.Errorf("expected checksum error message, got:\n%s", out)
	}
}

// newAPIServer serves GitHub-shaped release JSON at /repos/{repo}/releases and
// /repos/{repo}/releases/latest. latestTag may be "" to simulate no stable
// release (404 on /latest). allTags lists newest-first for the /releases list.
func newAPIServer(t *testing.T, latestTag string, allTags []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			if latestTag == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"tag_name": %q}`, latestTag)
		case strings.HasSuffix(r.URL.Path, "/releases"):
			parts := make([]string, len(allTags))
			for i, tg := range allTags {
				parts[i] = fmt.Sprintf(`{"tag_name": %q, "prerelease": true}`, tg)
			}
			fmt.Fprintf(w, "[%s]", strings.Join(parts, ","))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveRCPicksNewestPrerelease(t *testing.T) {
	osTok, archTok := hostOSArch()
	tag := "v0.2.0-rc.1"
	archive := fmt.Sprintf("piper_%s_%s_%s.tar.gz", strings.TrimPrefix(tag, "v"), osTok, archTok)
	dl := newReleaseServer(t, map[string][]byte{archive: tarGz(t, "piper", "x")}, nil)
	api := newAPIServer(t, "", []string{tag, "v0.1.0-rc.1"})

	prefix := t.TempDir()
	out, err := run(t, []string{"--cli-only", "--rc", "--no-enable"}, map[string]string{
		"PIPER_BASE_URL": dl.URL,
		"PIPER_API_URL":  api.URL,
		"PIPER_PREFIX":   prefix,
	})
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(prefix, "piper")); err != nil {
		t.Fatalf("piper (from %s) not installed: %v\n%s", tag, err, out)
	}
}

func TestDefaultNoStableReleaseErrors(t *testing.T) {
	api := newAPIServer(t, "", []string{"v0.1.0-rc.1"}) // no stable
	out, err := run(t, []string{"--cli-only", "--no-enable"}, map[string]string{
		"PIPER_BASE_URL": "http://127.0.0.1:0",
		"PIPER_API_URL":  api.URL,
		"PIPER_PREFIX":   t.TempDir(),
	})
	if err == nil {
		t.Fatalf("expected error when no stable release exists:\n%s", out)
	}
	if !strings.Contains(out, "--rc") {
		t.Errorf("expected message pointing to --rc, got:\n%s", out)
	}
}

func TestAgentInstallDropsUnitAndEnv(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("agent install path targets Linux/systemd")
	}
	osTok, archTok := hostOSArch()
	tag := "v9.9.9"
	ver := strings.TrimPrefix(tag, "v")
	assets := map[string][]byte{
		fmt.Sprintf("piperd_%s_%s_%s.tar.gz", ver, osTok, archTok): tarGz(t, "piperd", "fake-piperd"),
		fmt.Sprintf("piper_%s_%s_%s.tar.gz", ver, osTok, archTok):  tarGz(t, "piper", "fake-piper"),
		"piperd.service":     []byte("[Service]\nExecStart=/usr/local/bin/piperd\n"),
		"piperd.env.example": []byte("#PIPER_API_ADDR=127.0.0.1:8088\n"),
	}
	srv := newReleaseServer(t, assets, nil)

	prefix := t.TempDir()
	unitDir := t.TempDir()
	envDir := t.TempDir()
	env := map[string]string{
		"PIPER_BASE_URL":    srv.URL,
		"PIPER_VERSION":     tag,
		"PIPER_PREFIX":      prefix,
		"PIPER_SYSTEMD_DIR": unitDir,
		"PIPER_ENV_DIR":     envDir,
	}
	out, err := run(t, []string{"--no-enable"}, env)
	if err != nil {
		t.Fatalf("agent install failed: %v\n%s", err, out)
	}
	for _, p := range []string{
		filepath.Join(prefix, "piperd"),
		filepath.Join(prefix, "piper"),
		filepath.Join(unitDir, "piperd.service"),
		filepath.Join(envDir, "piperd.env"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v\n%s", p, err, out)
		}
	}

	// Re-run must not clobber an operator-edited env file.
	edited := "PIPER_BASE_DOMAIN=example.com\n"
	if err := os.WriteFile(filepath.Join(envDir, "piperd.env"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, []string{"--no-enable"}, env); err != nil {
		t.Fatalf("re-run failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(envDir, "piperd.env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != edited {
		t.Errorf("env file was clobbered on re-run: got %q", string(got))
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	return filepath.Dir(scriptPath(t))
}

func TestInstallDocumentation(t *testing.T) {
	// The README is the lean quick-start entry point; install flags and env
	// overrides live in docs/getting-started.md (see #181).
	docs := map[string][]string{
		"README.md": {
			"raw.githubusercontent.com/getpiper/piper/main/install.sh",
		},
		filepath.Join("docs", "getting-started.md"): {
			"--cli-only",
			"--rc",
			"PIPER_ADDR",
		},
	}
	for name, wants := range docs {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), name))
		if err != nil {
			t.Fatal(err)
		}
		content := string(b)
		for _, want := range wants {
			if !strings.Contains(content, want) {
				t.Errorf("%s missing %q", name, want)
			}
		}
	}
}
