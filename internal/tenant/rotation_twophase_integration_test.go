package tenant_test

// A ROTATION A RUNNING SERVICE CAN SURVIVE — W1.9.1, against a real Postgres, for the same reason
// rotation_overlap_integration_test.go is: the question is about the ORDER in which two states
// become visible to a third party, and no unit test of one function can see that.
//
// ⚠ WHAT IS ALREADY MEASURED AND IS NOT RE-MEASURED HERE. The sibling file pins the finding this
// builds on: RotateAPIKey is one transaction, both rows change visibility at COMMIT, and there is
// therefore no instant at which a client can authenticate with both keys. It also establishes that
// CreateAPIKey + RevokeAPIKey — two separate calls — DO produce a window. So the primitives for a
// safe rotation already existed; what did not exist was a supported PATH, and the one thing two
// raw calls cannot give you.
//
// ⚠ THAT ONE THING IS THE POINT OF THIS FILE. W1.9.1 says "VERIFY WITH AN AUTHENTICATED REQUEST,
// never a 200 from an unauthenticated health route — that is the exact shape of Track's
// lens_healthy defect, which reported healthy precisely when the credential was missing." As two
// calls that verification is an instruction a human either follows or does not, and the failure is
// silent: revoke too early and the outage starts when nobody is looking. Here it is a PRECONDITION
// the store enforces — CompleteRotation REFUSES until the new key has actually authenticated a
// request, and refuses in a way that leaves the old key working.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/tenant"
)

// twoPhase* reuse the sibling file's schema-isolation reasoning with their own schema: two
// independent findings must not share one teardown.
const twoPhaseSchema = "lens_it_rotate2p"

func twoPhasePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG two-phase rotation test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = twoPhaseSchema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// The fixture mirrors migrations 0018 and 0123. It is exercised by the REAL production SQL in
	// internal/tenant, so a column this fixture is missing surfaces as a hard SQL error rather than
	// as a passing test over a narrower table — and the partial UNIQUE index is carried across
	// because a fixture more PERMISSIVE than production is the half a missing column would not
	// catch.
	if _, err := pool.Exec(context.Background(), `
		DROP SCHEMA IF EXISTS `+twoPhaseSchema+` CASCADE;
		CREATE SCHEMA `+twoPhaseSchema+`;
		CREATE TABLE `+twoPhaseSchema+`.workspaces (
			id           TEXT PRIMARY KEY,
			name         TEXT NOT NULL,
			cache_prefix TEXT NOT NULL
		);
		CREATE TABLE `+twoPhaseSchema+`.workspace_api_keys (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id TEXT NOT NULL REFERENCES `+twoPhaseSchema+`.workspaces(id),
			key_hash     TEXT NOT NULL,
			key_prefix   TEXT NOT NULL,
			name         TEXT NOT NULL,
			scopes       TEXT[] NOT NULL,
			last_used_at TIMESTAMPTZ,
			expires_at   TIMESTAMPTZ,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE TABLE `+twoPhaseSchema+`.workspace_key_rotations (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id TEXT NOT NULL,
			old_key_id   UUID NOT NULL,
			new_key_id   UUID NOT NULL,
			started_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			completed_at TIMESTAMPTZ,
			outcome      TEXT
		);
		CREATE UNIQUE INDEX workspace_key_rotations_one_open_per_key
			ON `+twoPhaseSchema+`.workspace_key_rotations (old_key_id) WHERE completed_at IS NULL;
	`); err != nil {
		t.Fatalf("build fixture schema: %v", err)
	}
	return pool
}

// seedHolder gives a workspace one key, as a running service would already hold.
func seedHolder(t *testing.T, st *tenant.Store, ws string) (string, string) {
	t.Helper()
	raw, k, err := st.CreateAPIKey(context.Background(), ws, "the key a service holds",
		[]string{"proxy", "analytics"}, nil)
	if err != nil {
		t.Fatalf("seed key: %v", err)
	}
	return raw, k.ID
}

func authenticates(t *testing.T, st *tenant.Store, raw string) bool {
	t.Helper()
	_, err := st.ValidateAPIKey(context.Background(), raw)
	if err == nil {
		return true
	}
	if errors.Is(err, tenant.ErrInvalidKey) || errors.Is(err, tenant.ErrKeyExpired) {
		return false
	}
	t.Fatalf("ValidateAPIKey: unexpected error: %v", err)
	return false
}

// ⚠ THE HARNESS KEEPS ITS OWN POOL RATHER THAN THE STORE EXPOSING ONE. The first draft called
// st.Pool(); adding an exported accessor to a production type so a test can reach behind it is how
// an internal becomes an API, and this type guards credentials.
func twoPhaseStore(t *testing.T) (*tenant.Store, string, *pgxpool.Pool) {
	t.Helper()
	pool := twoPhasePool(t)
	st := tenant.NewStore(pool)
	ws := "ws-2p"
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1,$1,$1)`, ws); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	return st, ws, pool
}

// ── T1 the window exists ────────────────────────────────────────────────────────────────────

func TestT1_AfterBeginBothKeysAuthenticate(t *testing.T) {
	st, ws, _ := twoPhaseStore(t)
	oldRaw, oldID := seedHolder(t, st, ws)

	newRaw, fresh, rot, err := st.BeginRotation(context.Background(), ws, oldID)
	if err != nil {
		t.Fatalf("BeginRotation: %v", err)
	}
	if rot.ID == "" || fresh.ID == "" || newRaw == "" {
		t.Fatalf("[T1-SHAPE] BeginRotation returned an incomplete result: rot=%+v fresh=%+v", rot, fresh)
	}

	// ⚠ THIS IS THE WHOLE FEATURE. The sibling file measures that RotateAPIKey gives no instant at
	// which both keys work; a service holding the old one is locked out for the length of "write
	// the config, restart the process".
	if !authenticates(t, st, oldRaw) {
		t.Fatalf("[T1-OLD] the OLD key stopped authenticating at begin. There is then no window, and " +
			"this is RotateAPIKey with extra steps.")
	}
	if !authenticates(t, st, newRaw) {
		t.Fatalf("[T1-NEW] the NEW key does not authenticate, so the holder has nothing to switch to")
	}
	// and they are genuinely two different credentials
	if newRaw == oldRaw || fresh.ID == oldID {
		t.Fatalf("[T1-SAME] begin returned the same key it was asked to replace")
	}
	// ⚠ THE REPLACEMENT INHERITS THE OLD KEY'S SCOPES. A rotation that silently widened or
	// narrowed them would be a PRIVILEGE change wearing the clothes of a credential change — and
	// the narrowing direction breaks the holder in a way that looks like a bad key.
	if len(fresh.Scopes) != 2 || fresh.Scopes[0] != "proxy" || fresh.Scopes[1] != "analytics" {
		t.Fatalf("[T1-SCOPES] the replacement carries scopes %v, not the old key's [proxy analytics]", fresh.Scopes)
	}
	if fresh.Name != "the key a service holds" {
		t.Fatalf("[T1-NAME] the replacement is named %q, so an operator listing keys cannot tell which one this replaced", fresh.Name)
	}
}

// ── T2 completion is refused without proof, and refusing is safe ────────────────────────────

func TestT2_CompleteIsRefusedUntilTheNewKeyHasActuallyBeenUsed(t *testing.T) {
	st, ws, _ := twoPhaseStore(t)
	oldRaw, oldID := seedHolder(t, st, ws)

	newRaw, _, rot, err := st.BeginRotation(context.Background(), ws, oldID)
	if err != nil {
		t.Fatalf("BeginRotation: %v", err)
	}

	err = st.CompleteRotation(context.Background(), ws, rot.ID)
	if !errors.Is(err, tenant.ErrNewKeyUnused) {
		t.Fatalf("[T2] CompleteRotation returned %v, want ErrNewKeyUnused. The new key has never "+
			"authenticated anything, so completing here is precisely the outage W1.9.1 describes — "+
			"and the operator would have no way to know until the next request.", err)
	}
	// ⚠ REFUSING MUST BE SAFE. A guard that errors AND revokes has done the damage anyway.
	if !authenticates(t, st, oldRaw) {
		t.Fatalf("[T2-OLD] the refusal revoked the old key anyway — the guard caused the outage it exists to prevent")
	}
	if !authenticates(t, st, newRaw) {
		t.Fatalf("[T2-NEW] the refusal killed the new key too")
	}
}

// ── T3 proof is USE, and completion then works ──────────────────────────────────────────────

func TestT3_UsingTheNewKeyIsWhatUnlocksCompletion(t *testing.T) {
	st, ws, _ := twoPhaseStore(t)
	oldRaw, oldID := seedHolder(t, st, ws)

	newRaw, _, rot, err := st.BeginRotation(context.Background(), ws, oldID)
	if err != nil {
		t.Fatalf("BeginRotation: %v", err)
	}

	// The holder switches and makes ONE real authenticated request. Nothing else changes.
	if !authenticates(t, st, newRaw) {
		t.Fatalf("[T3-PREMISE] the new key must authenticate for this test to mean anything")
	}

	if err := st.CompleteRotation(context.Background(), ws, rot.ID); err != nil {
		t.Fatalf("[T3] CompleteRotation refused after the new key authenticated a request: %v", err)
	}
	if authenticates(t, st, oldRaw) {
		t.Fatalf("[T3-OLD] the old key still authenticates after completion — the rotation did not " +
			"finish, and a leaked credential is still live")
	}
	if !authenticates(t, st, newRaw) {
		t.Fatalf("[T3-NEW] completion killed the new key")
	}
}

// ── T4 the proof must be USE AFTER THE ROTATION OPENED, not any past use ────────────────────

func TestT4_UseFromBeforeTheRotationIsNotProof(t *testing.T) {
	st, ws, pool := twoPhaseStore(t)
	_, oldID := seedHolder(t, st, ws)

	newRaw, fresh, rot, err := st.BeginRotation(context.Background(), ws, oldID)
	if err != nil {
		t.Fatalf("BeginRotation: %v", err)
	}
	_ = newRaw

	// Backdate the new key's last_used_at to BEFORE the rotation opened. A key cannot really be
	// used before it exists — this is the shape of the mistake, written directly: a completion
	// gate that asks "is last_used_at set" rather than "is it after we started" would pass here,
	// and would pass for any pre-existing key an operator pointed the rotation at.
	if _, err := pool.Exec(context.Background(),
		`UPDATE workspace_api_keys SET last_used_at = $2 WHERE id = $1`,
		fresh.ID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	err = st.CompleteRotation(context.Background(), ws, rot.ID)
	if !errors.Is(err, tenant.ErrNewKeyUnused) {
		t.Fatalf("[T4] CompleteRotation accepted a last_used_at from BEFORE the rotation opened "+
			"(got %v). The gate is asking whether the column is set rather than whether the holder "+
			"switched.", err)
	}
}

// ── T5 the escape hatch: a rotation the holder could not take ───────────────────────────────

func TestT5_AbandonKillsTheNewKeyAndLeavesTheHolderWorking(t *testing.T) {
	st, ws, _ := twoPhaseStore(t)
	oldRaw, oldID := seedHolder(t, st, ws)

	newRaw, _, rot, err := st.BeginRotation(context.Background(), ws, oldID)
	if err != nil {
		t.Fatalf("BeginRotation: %v", err)
	}

	if err := st.AbandonRotation(context.Background(), ws, rot.ID); err != nil {
		t.Fatalf("[T5] AbandonRotation: %v", err)
	}
	// ⚠ ABANDON MUST KILL THE NEW KEY, NOT THE OLD ONE. Getting this backwards is the outage,
	// arrived at from the other direction — and an operator reaches for abandon precisely when
	// something has already gone wrong.
	// ⚠ BOTH ARE t.Errorf AND THE CONTROL IS WHY — FOR THE SECOND TIME IN THIS SESSION. As two
	// Fatalfs, the FIRST one always fired and [T5-OLD] was unreachable under every mutation: control
	// N5 (abandon revokes the OLD key) is caught by the new-key arm before the old-key arm can run,
	// so the assertion about the OUTAGE DIRECTION — the one this test is actually for — could not
	// fail. An ordering of Fatalfs is a silent priority between assertions.
	if authenticates(t, st, newRaw) {
		t.Errorf("[T5-NEW] the abandoned rotation's new key still authenticates — an unrotated key " +
			"is now live and nobody is holding it")
	}
	if !authenticates(t, st, oldRaw) {
		t.Errorf("[T5-OLD] abandon revoked the OLD key. That is the outage, reached by the path an " +
			"operator takes when they are already in trouble.")
	}
	// and the key is rotatable again afterwards — the partial unique index must not have pinned it
	if _, _, _, err := st.BeginRotation(context.Background(), ws, oldID); err != nil {
		t.Fatalf("[T5-REOPEN] the key cannot be rotated again after an abandon: %v", err)
	}
}

// ── T6 one open rotation per key ────────────────────────────────────────────────────────────

func TestT6_TwoOpenRotationsOnOneKeyAreRefused(t *testing.T) {
	st, ws, _ := twoPhaseStore(t)
	_, oldID := seedHolder(t, st, ws)

	if _, _, _, err := st.BeginRotation(context.Background(), ws, oldID); err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if _, _, _, err := st.BeginRotation(context.Background(), ws, oldID); !errors.Is(err, tenant.ErrRotationOpen) {
		t.Fatalf("[T6] a second begin on the same key returned %v, want ErrRotationOpen. Two open "+
			"rotations mint two replacements and the operator cannot say which one the holder took.", err)
	}
}

// ── T7 a rotation belongs to its workspace ──────────────────────────────────────────────────

func TestT7_ARotationCannotBeCompletedByAnotherWorkspace(t *testing.T) {
	st, ws, pool := twoPhaseStore(t)
	oldRaw, oldID := seedHolder(t, st, ws)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1,$1,$1)`, "ws-other"); err != nil {
		t.Fatalf("seed other workspace: %v", err)
	}

	newRaw, _, rot, err := st.BeginRotation(context.Background(), ws, oldID)
	if err != nil {
		t.Fatalf("BeginRotation: %v", err)
	}
	if !authenticates(t, st, newRaw) {
		t.Fatalf("[T7-PREMISE] the new key must authenticate, or the refusal below is about the wrong thing")
	}

	if err := st.CompleteRotation(context.Background(), "ws-other", rot.ID); !errors.Is(err, tenant.ErrRotationNotFound) {
		t.Fatalf("[T7] another workspace completed this rotation (got %v) — that revokes a key it "+
			"does not own", err)
	}
	if !authenticates(t, st, oldRaw) {
		t.Fatalf("[T7-OLD] the cross-workspace attempt revoked the key anyway")
	}
	// the control: the owning workspace CAN complete it, so the refusal above is about ownership
	// rather than about the rotation being uncompletable.
	if err := st.CompleteRotation(context.Background(), ws, rot.ID); err != nil {
		t.Fatalf("[T7-CONTROL] the owning workspace could not complete it either: %v", err)
	}
}
