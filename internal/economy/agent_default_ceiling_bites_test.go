package economy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// THE DEFAULT AGENT SPEND CEILING NOW HAS A TEST THAT DEPENDS ON ITS VALUE.
//
// MEASURED 2026-08-28 (tab-k2w8, W4.36/W4.38) with a reach probe that mutates
// the constant and runs all 114 packages with internal/defaultsguard EXCLUDED —
// excluded because that guard reds on any change to a pinned constant and would
// otherwise make every value read as enforced
// (~/talyvor-queue/w436-reach-probe-k2w8.py).
//
// RESULT: DefaultAgentCeilingLXC 50_000_000 -> 50_000_000_000 (x1000) was
// noticed by NOTHING, with the probe positive-controlled 3/3.
//
// ⚠ THE SUB-BUDGET PATH IS WELL TESTED — idempotency, concurrency, crash
// recovery, metadata, SEC-2 integer µLXC all have real-Postgres proofs. Every
// one of them sets an EXPLICIT ceiling via SetAgentCeiling. Nothing exercised
// the ceiling an agent gets when NOBODY sets one, which is the one every scoped
// key starts with.
//
// ⚠ WHAT THIS ASSERTS, AND WHAT IT DELIBERATELY DOES NOT. W4.33 already pinned
// the constant's VALUE, so a test asserting the INSERTed row equals the
// constant would be a second pin wearing an enforcement costume. This asserts
// the only thing a ceiling is for: that a debit crossing it is REFUSED. The
// funding is deliberately far above the ceiling so the refusal cannot come from
// an empty wallet, and the error is matched exactly — ErrSubBudgetExceeded, not
// ErrInsufficientLXC — because those two failures mean opposite things about
// whether the ceiling did any work.

func TestAgentDefaultCeiling_RefusesTheDebitThatCrossesIt(t *testing.T) {
	s := agentHarness(t)
	ctx := context.Background()

	// Fund far above the ceiling: if the wallet were the binding constraint the
	// refusal below would be ErrInsufficientLXC and would prove nothing about
	// the ceiling.
	fund(t, s, "wsCeil", 4*DefaultAgentCeilingLXC)

	// NON-VACUITY: spending exactly the default ceiling must SUCCEED. Without
	// this, "the crossing debit is refused" is satisfiable by a path that
	// refuses everything.
	if err := s.SpendLXCForAgent(ctx, "keyCeil", "wsCeil", "req-at-ceiling",
		DefaultAgentCeilingLXC, "spend up to the ceiling", AgentDebitMeta{}); err != nil {
		t.Fatalf("spending exactly the default ceiling (%d µLXC) must succeed with a funded "+
			"wallet, got: %v", DefaultAgentCeilingLXC, err)
	}

	balBefore, spentBefore, _ := agentState(t, s, "wsCeil", "keyCeil")
	if spentBefore != DefaultAgentCeilingLXC {
		t.Fatalf("spent_lxc = %d after spending the ceiling, want %d", spentBefore, DefaultAgentCeilingLXC)
	}

	// THE ASSERTION: one more µLXC crosses the default ceiling and must be refused.
	err := s.SpendLXCForAgent(ctx, "keyCeil", "wsCeil", "req-over-ceiling",
		1, "one µLXC past the ceiling", AgentDebitMeta{})
	if !errors.Is(err, ErrSubBudgetExceeded) {
		t.Fatalf("a debit crossing the DEFAULT agent ceiling was not refused with "+
			"ErrSubBudgetExceeded, got: %v.\nThe default ceiling is what every scoped key "+
			"starts with — if it does not bite, an agent key nobody configured has no spend "+
			"bound at all.", err)
	}

	// The refused debit must leave nothing behind: no balance movement, no
	// spent_lxc bump, and no orphan claim that would block the retry.
	balAfter, spentAfter, claims := agentState(t, s, "wsCeil", "keyCeil")
	if balAfter != balBefore {
		t.Errorf("refused debit moved the workspace balance: %d -> %d", balBefore, balAfter)
	}
	if spentAfter != spentBefore {
		t.Errorf("refused debit bumped spent_lxc: %d -> %d", spentBefore, spentAfter)
	}
	if claims != 1 {
		t.Errorf("lxc_spend_claims holds %d claim(s) for this key, want 1 — the refused debit "+
			"left an orphan claim, which would make the retry a silent no-op", claims)
	}
}

// ⚠ THE TEST ABOVE PROVES ENFORCEMENT AND IS DELIBERATELY VALUE-INVARIANT, AND
// THAT WAS MEASURED RATHER THAN ASSUMED: with DefaultAgentCeilingLXC multiplied
// by 1000 it still PASSES, because every quantity in it is expressed in terms of
// the constant. A test written entirely in terms of a constant cannot notice
// that constant changing — the same shape found in eval_consensus_gate_test.go
// and keel_test.go the same day, one level subtler.
//
// So the VALUE is bound here instead, against a copy that is maintained
// independently: the SQL column default. `agent_lxc_subbudgets.ceiling_lxc` has
// a DEFAULT in the migrations (0079 created it at 50 LXC; 0083 converted the
// table to integer µLXC and reset it to 50000000), and nothing compared it to
// the Go constant. Two copies of a money default that must agree, with no
// cross-check, is the shape this repository keeps finding elsewhere.

// migrationCeilingDefault returns the LAST default the migrations set for
// agent_lxc_subbudgets.ceiling_lxc, in migration order — the one a freshly
// migrated deployment actually has. Absence is a FAILURE: a guard that finds
// nothing to compare is the defect it exists to catch.
func migrationCeilingDefault(t *testing.T) int64 {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found (err=%v) — nothing to compare the constant against", err)
	}
	sort.Strings(files)
	// Matches both `ceiling_lxc <type> NOT NULL DEFAULT <n>` and
	// `ALTER COLUMN ceiling_lxc SET DEFAULT <n>`.
	re := regexp.MustCompile(`(?i)ceiling_lxc\b[^;\n]*?DEFAULT\s+([0-9_]+)`)
	found := int64(-1)
	var fromFile string
	for _, f := range files {
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("reading %s: %v", f, rerr)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			n, perr := strconv.ParseInt(strings.ReplaceAll(m[1], "_", ""), 10, 64)
			if perr != nil {
				t.Fatalf("%s: unparseable ceiling_lxc default %q", filepath.Base(f), m[1])
			}
			found, fromFile = n, filepath.Base(f)
		}
	}
	if found < 0 {
		t.Fatal("no ceiling_lxc DEFAULT found in any migration — the parser is broken, or the " +
			"column stopped having a default; either way this guard is comparing nothing")
	}
	t.Logf("authoritative ceiling_lxc default %d, last set by %s", found, fromFile)
	return found
}

// TestAgentDefaultCeiling_GoConstantMatchesTheMigration binds the constant's
// VALUE to the schema a deployment gets. A row inserted without an explicit
// ceiling — by any path that does not go through SpendLXCForAgent — takes the
// SQL default, so the two disagreeing means two different ceilings depending on
// which door created the row.
func TestAgentDefaultCeiling_GoConstantMatchesTheMigration(t *testing.T) {
	if got := migrationCeilingDefault(t); got != DefaultAgentCeilingLXC {
		t.Fatalf("migrations default agent_lxc_subbudgets.ceiling_lxc to %d µLXC but "+
			"DefaultAgentCeilingLXC is %d µLXC.\nA sub-budget row created by the Go path would "+
			"get one ceiling and a row created by any other path the other. Change both together "+
			"or neither — 0083 is the migration that last set it, and it exists precisely because "+
			"0079's default was in LXC and the table moved to µLXC.", got, DefaultAgentCeilingLXC)
	}
}
