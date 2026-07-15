package outputverify

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// artifact.go — H5 OPT-IN BUILDABLE-ARTIFACT manifest. The commitment a workspace bonds is the sha256 of a
// canonical MANIFEST over its buildable module's (path → content-sha256) entries — NOT the raw served
// envelope. This is the ONE seam that lets Talyvor's attestor bind a real, buildable tree to an output and
// reproduce its compile.
//
// SOUNDNESS lives in CommitArtifactSHA256: the OUTPUT SLOT is forced to the output's already-committed
// response_sha256 (the actually-served bytes, locked at generation) — a workspace cannot bind a module whose
// output slot differs from what it served. Everything else (attestor build, IsSlashUsable, the appeal window)
// is unchanged.

// ManifestEntry is one file's identity in a buildable module: its module-relative path (forward-slash) and
// the hex sha256 of its content. Content bytes never appear here — only the hash.
type ManifestEntry struct {
	Path          string `json:"path"`
	ContentSHA256 string `json:"content_sha256"`
}

// manifestDomain is domain-separation so a manifest hash can never collide with any other sha256 use.
const manifestDomain = "h5_artifact_manifest/v1\n"

// ManifestHash is the canonical, order-independent hash of a module manifest: entries sorted by path, each
// contributed as (path ‖ 0x00 ‖ content_sha256 ‖ 0x0A) under a domain-separation prefix. Two file sets hash
// equal iff they have the same {path: content_sha256} map.
func ManifestHash(entries []ManifestEntry) string {
	sorted := append([]ManifestEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	h.Write([]byte(manifestDomain))
	for _, e := range sorted {
		h.Write([]byte(e.Path))
		h.Write([]byte{0})
		h.Write([]byte(e.ContentSHA256))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CommitArtifactSHA256 is the GENERATION-TIME-BOUND commitment. It takes the workspace's module manifest, the
// path of the output slot, and the output's ALREADY-COMMITTED response hash (the actually-served bytes), and
// returns the manifest hash with the output slot FORCED to servedResponseSHA256. Any output-slot hash the
// workspace put in `entries` is dropped and replaced — the workspace cannot claim a different output than what
// it served. This forcing is the whole soundness of H5 opt-in bonds; removing it reopens the bond-time-supply
// hole (a workspace supplying a tree that compiles while having served different bytes).
func CommitArtifactSHA256(entries []ManifestEntry, outputPath, servedResponseSHA256 string) string {
	out := make([]ManifestEntry, 0, len(entries)+1)
	for _, e := range entries {
		if e.Path == outputPath {
			continue // drop any workspace claim for the output slot — the served hash wins
		}
		out = append(out, e)
	}
	out = append(out, ManifestEntry{Path: outputPath, ContentSHA256: servedResponseSHA256}) // BIND the served output
	return ManifestHash(out)
}

// ManifestHashDir walks an EXTRACTED source tree and returns (manifest hash, content-hash by module-relative
// path). Only regular files are hashed (the attestor's safeExtractTar never creates symlinks/devices, and we
// exclude anything non-regular here too). The attestor compares the returned hash to the committed
// artifact_sha256 and checks the output-slot file's hash equals response_sha256.
func ManifestHashDir(dir string) (hash string, contentByPath map[string]string, err error) {
	var entries []ManifestEntry
	contentByPath = map[string]string{}
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		ch := Sha256Hex(b)
		entries = append(entries, ManifestEntry{Path: rel, ContentSHA256: ch})
		contentByPath[rel] = ch
		return nil
	})
	if walkErr != nil {
		return "", nil, walkErr
	}
	return ManifestHash(entries), contentByPath, nil
}
