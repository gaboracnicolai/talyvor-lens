package tare_test

// attribution_premise_test.go — W6.1.4's OWN FIRST INSTRUCTION, executed.
//
// W6.1.4 says:
//
//	⚠ THE WORK-ITEM ID COMES FROM THE GATEWAY-SIGNED HEADER a client cannot forge (the T7
//	  attribution path Track already uses). ⚠ VERIFY THAT PATH EXISTS BEFORE DEPENDING ON IT.
//
// ⚠ IT DOES NOT EXIST. The work-item id arrives as the `X-Talyvor-Feature` request header, is read
// straight off the request, is never signed and is never verified — and this repo ALREADY KNOWS
// that and already refuses, in two places, to let it reach anything that scores or mints:
//
//	internal/mining/pattern_mining.go — "it is the one caller-controlled field (the
//	  X-Talyvor-Feature header), so keying rarity on it would let a workspace manufacture
//	  uniqueness by varying the header. feature_category is still PERSISTED on the row (analytics)
//	  — it just cannot move the score."
//
//	internal/cohort/cohort.go — "feature_category is DECLARED at the boundary (the
//	  X-Talyvor-Feature header at serve time) ... This package reaches no ledger/mint path."
//
// ⚠ WHAT *IS* SIGNED IS SOMETHING ELSE. talyvor-track's internal/lensintegration/webhook.go HMACs
// the WEBHOOK PAYLOAD, and its own comment says the issue is matched by the X-Talyvor-Feature value
// "SCOPED to the workspace the signed payload names". The signature authenticates the WORKSPACE.
// The work-item id inside it is still the caller's string.
//
// ⚠ WHY THAT BLOCKS W6.1.4 AS WRITTEN. The metering record it specifies carries `delta_cost_usd`
// against `work_item_id`, and reports/tare-design-v1.html says that figure is avoided-COGS feeding
// the same economy that pays cache and distill royalties — justified precisely because the signal
// is "gateway-observed, not participant-asserted". The SAVING is gateway-observed. The
// ATTRIBUTION is participant-asserted. Attributing a money figure to a caller-declared string is
// exactly what the two files above already refuse to do.
//
// This file makes that verification executable so it is re-checked rather than re-argued.
// docs/tare-phase1d-brief.md carries the decision.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const attributionHeader = "X-Talyvor-Feature"

// TestPremise_TheWorkItemHeaderIsNeitherSignedNorVerified is TWO-SIDED. The half that stops it
// reporting its own blindness is the second: the header must actually BE READ somewhere. A census
// that finds no verification because it can find nothing at all is worthless.
func TestPremise_TheWorkItemHeaderIsNeitherSignedNorVerified(t *testing.T) {
	root := "../.."
	var readers, verifiers []string

	err := walkGoSource(root, func(path, src string) {
		rel := strings.TrimPrefix(filepath.ToSlash(path), filepath.ToSlash(root)+"/")
		if strings.HasSuffix(rel, "_test.go") || strings.Contains(rel, "/sdk/") {
			return
		}
		for _, line := range strings.Split(src, "\n") {
			if !strings.Contains(line, attributionHeader) {
				continue
			}
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue // a comment ABOUT the header is not a use of it
			}
			if strings.Contains(line, "Header.Get(") || strings.Contains(line, "Header.Set(") {
				readers = append(readers, rel)
				continue
			}
			// Anything else touching the header is a candidate verification path.
			verifiers = append(verifiers, rel+": "+trimmed)
		}
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(readers) == 0 {
		t.Fatalf("no code reads %s at all — this census has gone blind, and 'nothing verifies it' "+
			"would be a statement about the SEARCH rather than about the tree", attributionHeader)
	}
	if len(verifiers) > 0 {
		t.Fatalf("%s now has something other than a plain Get/Set — a verification path may have "+
			"appeared:\n  %s\n\n⚠ IF THAT IS A REAL SIGNATURE CHECK, W6.1.4's PREMISE HAS BECOME "+
			"TRUE: delete this test and the blocking half of docs/tare-phase1d-brief.md, and the "+
			"metering record may attribute money to the work item.",
			attributionHeader, strings.Join(verifiers, "\n  "))
	}
	t.Logf("MEASURED: %d files read %s with a plain Header.Get/Set and NONE verifies it. "+
		"The work-item id is caller-declared, so W6.1.4's 'gateway-signed header a client cannot "+
		"forge' does not exist.", len(readers), attributionHeader)
}

// ⚠ THE TWO REFUSALS ALREADY IN THE TREE. If either is ever deleted, whoever deletes it should have
// to argue with this test — because W6.1.4 would then be being built against a precedent that no
// longer holds.
func TestPremise_TheRepoAlreadyRefusesToLetTheDeclaredFieldScoreOrMint(t *testing.T) {
	for _, tc := range []struct{ file, needle, why string }{
		{"internal/mining/pattern_mining.go", "one caller-controlled field",
			"the rarity score deliberately excludes feature_category because a workspace could manufacture uniqueness by varying the header"},
		{"internal/cohort/cohort.go", "reaches no ledger/mint path",
			"the cohort package deliberately keeps the declared field away from any ledger"},
	} {
		b, err := os.ReadFile(filepath.Join("../..", tc.file))
		if err != nil {
			t.Fatalf("read %s: %v — a precedent this brief rests on has moved", tc.file, err)
		}
		if !strings.Contains(string(b), tc.needle) {
			t.Fatalf("%s no longer says %q.\nThat refusal is what makes W6.1.4's attribution a "+
				"DECISION rather than an oversight: %s.\nIf the refusal was withdrawn deliberately, "+
				"say so in docs/tare-phase1d-brief.md; if it was lost in an edit, restore it.",
				tc.file, tc.needle, tc.why)
		}
	}
}

// ⚠ THE OTHER PREMISE, WHICH IS TRUE. W6.1.4 says not to use token_events.savings_pct because it is
// writerless and that column family produced a wrong customer-facing number three times. Verified:
// still writerless, and the repo's own guard still pins it.
func TestPremise_SavingsPctIsStillWriterless(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("../..", "migrations/0114_writerless_column_comments.sql"))
	if err != nil {
		// The migration may be renamed; find it rather than fail on the name.
		matches, _ := filepath.Glob(filepath.Join("../..", "migrations/0114_*.sql"))
		if len(matches) == 0 {
			t.Skip("migration 0114 not found under that number — re-anchor this premise check")
		}
		b, err = os.ReadFile(matches[0])
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if !strings.Contains(string(b), "WRITERLESS") || !strings.Contains(string(b), "savings_pct") {
		t.Fatalf("migration 0114 no longer marks savings_pct WRITERLESS — if a writer was added, " +
			"W6.1.4's instruction to avoid that column needs re-reading")
	}
	if _, err := os.Stat(filepath.Join("../..", "internal/catalog/writerless_column_guard_test.go")); err != nil {
		t.Fatalf("the writerless-column guard is gone (%v) — the column could acquire a writer "+
			"silently, which is how it produced a wrong customer-facing number before", err)
	}
}
