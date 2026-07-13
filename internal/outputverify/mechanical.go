package outputverify

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Mechanical verdict values (did it compile / did tests pass) — SQL-enforced enum (migration 0085).
const (
	MechCompiled      = "compiled"
	MechCompileFailed = "compile_failed"
	MechTestsPassed   = "tests_passed"
	MechTestsFailed   = "tests_failed"
)

// Verdict sources.
//   - self_reported: reported by the producing workspace itself (the self-report endpoint hard-codes this).
//   - talyvor_verified: produced by Talyvor's OWN sandboxed re-run — the only ATTESTED source. NO PRODUCER
//     EXISTS YET; the sandboxed compile executor is step 2. Adding the source + its slash rule here is step 1.
const (
	SourceSelfReported    = "self_reported"
	SourceTalyvorVerified = "talyvor_verified"
)

// IsSlashUsable reports whether a mechanical verdict may be used as H5 SLASH evidence. It is the ONLY gate
// that authorizes a burn, and the truth table is deliberately narrow:
//
//	self_reported    + (compile_failed | tests_failed)  → TRUE  — credible AGAINST INTEREST (nobody falsely
//	                                                              confesses to being slashable). Unchanged.
//	talyvor_verified + compile_failed                   → TRUE  — attested, deterministic, reproducible.
//	talyvor_verified + tests_failed                     → FALSE — THE LOAD-BEARING RULE (see below).
//	talyvor_verified + (compiled | tests_passed)        → FALSE — a pass never slashes.
//	anything else (unknown source, pass)                → FALSE.
//
// WHY tests_failed can NEVER be attested: `go test` COMPILES AND RUNS the test binary, i.e. it executes
// arbitrary code — flaky tests, t.Parallel() data races, time.Now(), RNG seeds, network, CGO/host toolchain.
// A test verdict is therefore NOT reproducible across environments: an honest workspace's tests can fail on a
// runner whose environment merely differed. Attesting a test failure would produce FALSE SLASHES — burning an
// honest workspace's collateral for a difference it did not cause. A false slash is WORSE than the current
// fail-open hole. Only `go build` (pinned toolchain, verified deps, no network, no CGO) is BOTH deterministic
// AND free of target-code execution, so only compile_failed is attestable. Test verdicts stay self-reported
// FOREVER — by design, not by omission. (Defense in depth: migration 0087 also makes a talyvor_verified test
// row unrepresentable at the DB, so both the schema and this function must fail before a false slash occurs.)
func IsSlashUsable(verdict, source string) bool {
	switch source {
	case SourceSelfReported:
		return verdict == MechCompileFailed || verdict == MechTestsFailed
	case SourceTalyvorVerified:
		return verdict == MechCompileFailed // NOT tests_failed — a test verdict is never reproducible.
	default:
		return false
	}
}

// mechanicalDB needs a read (ownership check against k4_output_verdicts) + the append-only write. It exposes
// no Begin — no arbitrary transaction surface. *pgxpool.Pool satisfies it.
type mechanicalDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// MechanicalReport is one self-reported mechanical verdict for a produced output.
type MechanicalReport struct {
	OutputID    string
	WorkspaceID string // the CALLER — must own output_id
	Verdict     string
	ExitCode    int
	Tool        string
	Reason      string
}

// MechanicalWriter records self-reported mechanical verdicts, ownership-bound.
type MechanicalWriter struct{ db mechanicalDB }

func NewMechanicalWriter(db mechanicalDB) *MechanicalWriter { return &MechanicalWriter{db: db} }

const ownsOutputSQL = `SELECT EXISTS (SELECT 1 FROM k4_output_verdicts WHERE output_id = $1 AND workspace_id = $2)`

// insertMechanicalIfOwnedSQL inserts ONLY WHERE the caller owns the output_id (it appears in
// k4_output_verdicts with the caller's workspace_id), append-only on (output_id, verdict_source).
const insertMechanicalIfOwnedSQL = `INSERT INTO k4_mechanical_verdicts
    (output_id, workspace_id, verdict, exit_code, tool, reason, verdict_source)
SELECT $1, $2, $3, $4, $5, $6, 'self_reported'
WHERE EXISTS (SELECT 1 FROM k4_output_verdicts WHERE output_id = $1 AND workspace_id = $2)
ON CONFLICT (output_id, verdict_source) DO NOTHING`

// RecordMechanicalIfOwned records a self-reported mechanical verdict ONLY if the caller produced the
// output_id. owned=false ⇒ the caller is not the producer (handler → 403); a workspace can never report on
// another workspace's output. recorded=true on the first report; false on an append-only dedup (still
// owned). It NEVER overwrites an existing verdict.
func (w *MechanicalWriter) RecordMechanicalIfOwned(ctx context.Context, r MechanicalReport) (owned, recorded bool, err error) {
	if w == nil || w.db == nil {
		return false, false, nil
	}
	if err := w.db.QueryRow(ctx, ownsOutputSQL, r.OutputID, r.WorkspaceID).Scan(&owned); err != nil {
		return false, false, fmt.Errorf("outputverify: ownership check: %w", err)
	}
	if !owned {
		return false, false, nil
	}
	tag, err := w.db.Exec(ctx, insertMechanicalIfOwnedSQL, r.OutputID, r.WorkspaceID, r.Verdict, r.ExitCode, r.Tool, r.Reason)
	if err != nil {
		return true, false, fmt.Errorf("outputverify: record mechanical verdict: %w", err)
	}
	return true, tag.RowsAffected() == 1, nil
}
