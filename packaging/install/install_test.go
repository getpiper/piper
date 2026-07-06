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
