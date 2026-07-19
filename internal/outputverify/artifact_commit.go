package outputverify

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// artifact_commit.go — the GENERATION-TIME opt-in producer. A workspace that produced an output may commit,
// bound to that output, the sha256 of the buildable module it relies on. The soundness is that the OUTPUT
// SLOT is folded from the output's ALREADY-STORED output_content_sha256 (the canonical served content,
// locked at capture — content.go pins the byte definition) — never a workspace-supplied output hash. So even
// though the commit call lands after serving, the commitment binds what was served: a workspace cannot bind
// a module whose output differs from its output. And because the canonical content IS the code the flagship
// writer materializes on disk, the committed manifest is satisfiable by a real, BUILDABLE tree (the old
// envelope-hash fold was satisfiable by nothing buildable). Append-once + owner-bound. An output captured
// without a content binding (NULL — pre-0098 rows, extraction failure, streaming) can never commit.

var (
	// ErrOutputNotFound — no such output_id (K4 off, or never served).
	ErrOutputNotFound = errors.New("outputverify: output not found")
	// ErrNotOutputOwner — the caller did not produce this output; only the producer may commit an artifact.
	ErrNotOutputOwner = errors.New("outputverify: caller is not the output's producer")
	// ErrNoOutputPath — an artifact commitment must name the output slot path.
	ErrNoOutputPath = errors.New("outputverify: output_path required")
	// ErrNoContentBinding — the output has no output_content_sha256 (captured before 0098, extraction failed,
	// or a true stream); there is nothing a buildable tree could match, so no artifact can ever be committed.
	ErrNoContentBinding = errors.New("outputverify: output has no content binding; artifact commitment impossible")
)

// artifactCommitDB needs a read (content hash + owner) and the once-only owner-bound UPDATE.
type artifactCommitDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ArtifactCommitter records the opt-in buildable-artifact commitment on an output the caller produced.
type ArtifactCommitter struct{ db artifactCommitDB }

func NewArtifactCommitter(db artifactCommitDB) *ArtifactCommitter { return &ArtifactCommitter{db: db} }

const artifactCommitSQL = `UPDATE k4_output_verdicts
    SET artifact_sha256 = $2, artifact_output_path = $3
    WHERE output_id = $1 AND workspace_id = $4 AND artifact_sha256 IS NULL`

// Commit folds the output's stored output_content_sha256 (the canonical served content) into the workspace's
// context manifest at outputPath and records the resulting artifact_sha256 — ONCE, and only for the output's
// producer. Returns the committed hash and committed=true on the first commit; committed=false if the output
// was already committed (the existing commitment stands — this never overwrites).
func (c *ArtifactCommitter) Commit(ctx context.Context, outputID, callerWorkspaceID, outputPath string, contextManifest []ManifestEntry) (artifactSHA256 string, committed bool, err error) {
	if c == nil || c.db == nil {
		return "", false, nil
	}
	if outputPath == "" {
		return "", false, ErrNoOutputPath
	}
	var contentSHA *string
	var ownerWS string
	err = c.db.QueryRow(ctx, `SELECT output_content_sha256, workspace_id FROM k4_output_verdicts WHERE output_id=$1`, outputID).
		Scan(&contentSHA, &ownerWS)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrOutputNotFound
	}
	if err != nil {
		return "", false, fmt.Errorf("outputverify: artifact commit lookup: %w", err)
	}
	if ownerWS != callerWorkspaceID {
		return "", false, ErrNotOutputOwner
	}
	if contentSHA == nil || *contentSHA == "" {
		return "", false, ErrNoContentBinding
	}
	// THE GENERATION-TIME BINDING: the output slot is the canonical SERVED content hash (from the DB), not
	// anything the caller supplied. CommitArtifactSHA256 drops any output-slot the caller put in contextManifest.
	artifactSHA256 = CommitArtifactSHA256(contextManifest, outputPath, *contentSHA)
	tag, err := c.db.Exec(ctx, artifactCommitSQL, outputID, artifactSHA256, outputPath, callerWorkspaceID)
	if err != nil {
		return "", false, fmt.Errorf("outputverify: artifact commit: %w", err)
	}
	return artifactSHA256, tag.RowsAffected() == 1, nil
}
