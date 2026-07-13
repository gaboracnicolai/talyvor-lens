// Package attest wires Talyvor's OWN sandboxed compile (internal/buildverify) to the K4 verdict store: given
// a bonded output_id and a candidate source tree, it REPRODUCES the build and records ONLY a trustworthy
// compile verdict as verdict_source='talyvor_verified'. It is the producer step 3 promised.
//
// ⚠ SOURCE PROVENANCE — the binding, stated honestly. An output_id commits (via k4_output_verdicts.
// response_sha256) to the RAW UPSTREAM RESPONSE BYTES; lens stores only the hash, never the bytes. The ONLY
// sound binding is therefore: sha256(supplied tree) == response_sha256. That matches only if the supplied
// bytes ARE the exact committed output. For today's chat-completion outputs the committed bytes are a JSON
// response envelope, NOT a buildable tree, so the binding REFUSES every real output — no attested verdict is
// ever recorded, and H5 stays fail-open. It becomes usable only for a FUTURE gateway convention that commits
// a buildable-module provenance hash at generation time. We REFUSE rather than build an UNBOUND tree, because
// an attested verdict on an input we cannot tie to the output is worse than no attestation.
//
// This package is mint-free: it reads k4_output_verdicts and writes k4_mechanical_verdicts; it never touches
// the ledger. verdict_source is HARD-CODED 'talyvor_verified' and workspace_id is the OWNER from
// k4_output_verdicts (never caller-supplied). buildverify only ever returns compiled|compile_failed|
// not_verifiable, so this writer can NEVER attempt a test verdict (the 0087 CHECK is never even reached).
package attest

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/buildverify"
	"github.com/talyvor/lens/internal/outputverify"
)

// Outcome is the result of an attestation attempt.
type Outcome string

const (
	OutcomeAttested      Outcome = "attested"       // a talyvor_verified verdict was recorded (compiled or compile_failed)
	OutcomeRefused       Outcome = "refused"        // disabled / unbound tree / unknown output — NO row, NO effect
	OutcomeNotVerifiable Outcome = "not_verifiable" // built, but the sandbox produced no trustworthy verdict — NO row
)

// Result reports what happened. Recorded is true only when a row was actually written.
type Result struct {
	Outcome  Outcome
	Verdict  string
	Reason   string
	Recorded bool
}

// verifier is the compile-only sandbox (*buildverify.Verifier satisfies it).
type verifier interface {
	Verify(ctx context.Context, srcDir string) buildverify.Result
}

// Attestor reproduces a bonded output's build in Talyvor's sandbox and records the attested verdict.
type Attestor struct {
	db      *pgxpool.Pool
	verify  verifier
	enabled bool
	maxTree int64
}

// NewAttestor wires the PRIMARY pool + the sandbox verifier. enabled=false makes Attest refuse without doing
// anything. Gated by LENS_H5_ATTEST_ENABLED.
func NewAttestor(pool *pgxpool.Pool, v verifier, enabled bool) *Attestor {
	return &Attestor{db: pool, verify: v, enabled: enabled, maxTree: 64 << 20}
}

const insertAttestedSQL = `INSERT INTO k4_mechanical_verdicts
    (output_id, workspace_id, verdict, exit_code, tool, reason, verdict_source, platform)
VALUES ($1, $2, $3, $4, 'go build (sandboxed)', $5, 'talyvor_verified', $6)
ON CONFLICT (output_id, verdict_source) DO NOTHING`

// Attest binds treeTar to output_id, reproduces the build in the sandbox, and records ONLY a trustworthy
// compile verdict as talyvor_verified. A not_verifiable build records NOTHING (the bond stays on the
// self-reported / fail-open path). An unbound tree, unknown output, or disabled attestor REFUSES — no row.
func (a *Attestor) Attest(ctx context.Context, outputID string, treeTar []byte) (Result, error) {
	if !a.enabled {
		return Result{Outcome: OutcomeRefused, Reason: "attestation disabled (LENS_H5_ATTEST_ENABLED=false)"}, nil
	}

	// Look up what the output committed to + who owns it.
	var responseSHA, ownerWS string
	err := a.db.QueryRow(ctx,
		`SELECT response_sha256, workspace_id FROM k4_output_verdicts WHERE output_id=$1`, outputID).
		Scan(&responseSHA, &ownerWS)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{Outcome: OutcomeRefused, Reason: "output_id not recorded (K4 off?) — cannot bind a source, refusing"}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("attest: lookup: %w", err)
	}

	// THE BINDING: the supplied bytes must be EXACTLY the output's committed response. Anything else is an
	// unbound tree and is refused — we never build an input we cannot tie to the output_id.
	sum := sha256.Sum256(treeTar)
	if hex.EncodeToString(sum[:]) != responseSHA {
		return Result{Outcome: OutcomeRefused, Reason: "supplied source does not match the output's committed response hash — unbound tree, refusing"}, nil
	}

	// Extract the bound bytes as a source tree (host-side, hardened against path-traversal/symlink/bomb).
	dir, err := os.MkdirTemp("", "attest-src-*")
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := safeExtractTar(treeTar, dir, a.maxTree); err != nil {
		return Result{Outcome: OutcomeRefused, Reason: "bound bytes are not a safe buildable archive: " + err.Error()}, nil
	}

	// Talyvor's OWN sandboxed build — reproduce-before-burn.
	r := a.verify.Verify(ctx, dir)
	if r.Verdict == buildverify.NotVerifiable {
		// FAIL OPEN ON DOUBT — no row, no attested burn. The bond stays on the self-reported path.
		return Result{Outcome: OutcomeNotVerifiable, Verdict: string(r.Verdict), Reason: r.Reason}, nil
	}

	verdict := outputverify.MechCompiled
	exit := 0
	if r.Verdict == buildverify.CompileFailed {
		verdict, exit = outputverify.MechCompileFailed, 1
	}
	platform := strings.TrimSpace(r.Toolchain + " " + r.Platform)
	tag, err := a.db.Exec(ctx, insertAttestedSQL, outputID, ownerWS, verdict, exit, truncate(r.Reason, 200), platform)
	if err != nil {
		return Result{}, fmt.Errorf("attest: record: %w", err)
	}
	return Result{Outcome: OutcomeAttested, Verdict: string(r.Verdict), Recorded: tag.RowsAffected() == 1,
		Reason: "talyvor_verified " + string(r.Verdict) + " on " + platform}, nil
}

// safeExtractTar extracts a tar to dest with maximum caution (this runs on the HOST before the sandbox):
// paths are confined to dest, absolute/`..` paths are rejected, symlinks/hardlinks/devices are SKIPPED (never
// created), and total size + entry count are bounded (tar bomb).
func safeExtractTar(data []byte, dest string, maxBytes int64) error {
	tr := tar.NewReader(bytes.NewReader(data))
	cleanDest := filepath.Clean(dest)
	var total int64
	count := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("bad tar: %w", err)
		}
		count++
		if count > 10000 {
			return errors.New("too many entries")
		}
		name := filepath.Clean(hdr.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe path %q", hdr.Name)
		}
		target := filepath.Join(cleanDest, name)
		if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(filepath.Separator)) {
			return fmt.Errorf("path escapes destination: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			total += hdr.Size
			if total > maxBytes {
				return errors.New("archive exceeds size limit")
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.CopyN(f, tr, hdr.Size); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// symlink / hardlink / device / fifo — SKIP (never extract; defeats symlink & zip-slip attacks).
			continue
		}
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
