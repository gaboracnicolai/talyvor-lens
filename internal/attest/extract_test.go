package attest

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func tarOf(entries []tar.Header, bodies map[string]string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		hh := h
		if b, ok := bodies[h.Name]; ok {
			hh.Size = int64(len(b))
		}
		_ = tw.WriteHeader(&hh)
		if b, ok := bodies[h.Name]; ok {
			_, _ = tw.Write([]byte(b))
		}
	}
	_ = tw.Close()
	return buf.Bytes()
}

// The extractor (host-side, before the sandbox) must reject path traversal + never create symlinks.
func TestSafeExtractTar_Hardening(t *testing.T) {
	t.Run("path traversal rejected", func(t *testing.T) {
		data := tarOf([]tar.Header{{Name: "../escape.go", Mode: 0o644, Typeflag: tar.TypeReg}}, map[string]string{"../escape.go": "x"})
		if err := safeExtractTar(data, t.TempDir(), 1<<20); err == nil {
			t.Error("a `..` path must be rejected")
		}
	})
	t.Run("absolute path rejected", func(t *testing.T) {
		data := tarOf([]tar.Header{{Name: "/etc/evil", Mode: 0o644, Typeflag: tar.TypeReg}}, map[string]string{"/etc/evil": "x"})
		if err := safeExtractTar(data, t.TempDir(), 1<<20); err == nil {
			t.Error("an absolute path must be rejected")
		}
	})
	t.Run("symlink skipped, never created", func(t *testing.T) {
		dir := t.TempDir()
		data := tarOf([]tar.Header{
			{Name: "ok.go", Mode: 0o644, Typeflag: tar.TypeReg},
			{Name: "evil", Linkname: "/etc/passwd", Mode: 0o777, Typeflag: tar.TypeSymlink},
		}, map[string]string{"ok.go": "package main"})
		if err := safeExtractTar(data, dir, 1<<20); err != nil {
			t.Fatalf("a valid file + a symlink should extract the file and skip the link: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(dir, "evil")); err == nil {
			t.Error("the symlink must NOT have been created")
		}
		if _, err := os.Stat(filepath.Join(dir, "ok.go")); err != nil {
			t.Error("the regular file should have been extracted")
		}
	})
	t.Run("size bomb rejected", func(t *testing.T) {
		big := make([]byte, 2<<20)
		data := tarOf([]tar.Header{{Name: "big.bin", Mode: 0o644, Typeflag: tar.TypeReg}}, map[string]string{"big.bin": string(big)})
		if err := safeExtractTar(data, t.TempDir(), 1<<20); err == nil {
			t.Error("an archive exceeding the size limit must be rejected")
		}
	})
}
