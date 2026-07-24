package github

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/piperbox/piper/internal/source"
)

func (p *Provider) Fetch(ctx context.Context, ev source.Event, destDir string) error {
	token, err := p.tokens.Token(ctx, ev)
	if err != nil {
		return err
	}
	url := p.apiBase + "/repos/" + ev.Repo + "/tarball/" + ev.SHA
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("tarball: %s: %s", resp.Status, body)
	}
	return extractStripped(resp.Body, destDir)
}

// extractStripped un-gzips and untars, removing the single top-level directory
// GitHub wraps repo tarballs in (e.g. "owner-repo-sha/").
func extractStripped(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		rel := stripFirst(hdr.Name)
		if rel == "" {
			continue
		}
		target := filepath.Join(destDir, rel)
		if !within(destDir, target) {
			return errors.New("tar entry escapes destination")
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
}

// stripFirst drops the leading path component ("owner-repo-sha/rest" -> "rest").
func stripFirst(name string) string {
	name = filepath.Clean("/" + name)[1:] // normalize, drop leading slash
	i := strings.IndexByte(name, '/')
	if i < 0 {
		return ""
	}
	return name[i+1:]
}

func within(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
