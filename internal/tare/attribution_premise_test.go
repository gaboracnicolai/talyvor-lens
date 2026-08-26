package tare_test

// attribution_premise_test.go — W6.1.4's OWN FIRST INSTRUCTION, executed.
//
// W6.1.4 says:
//
//	⚠ THE WORK-ITEM ID COMES FROM THE GATEWAY-SIGNED HEADER a client cannot forge (the T7
//	  attribution path Track already uses). ⚠ VERIFY THAT PATH EXISTS BEFORE DEPENDING ON IT.
//
// ⚠ IT DOES NOT EXIST. That conclusion is unchanged. What changed in #4xx is WHICH HEADER this
// file watches, and that correction matters more than it sounds.
//
// ⚠⚠ THE WORK-ITEM ID IS `X-Talyvor-Issue` (and `X-Talyvor-PR`). IT IS NOT `X-Talyvor-Feature`.
// reports/tare-design-v1.html books the saving to "the exact issue / PR / spec it came from", and
// on this repo's serve path those arrive as X-Talyvor-Issue / X-Talyvor-PR:
// attribution.ExtractFromRequest maps them to IssueID / Git.PRNumber, and recordSQL writes them to
// request_attribution.issue_id / pr_number — the row internal/api's spend-by-request endpoint joins
// to token_events for Track. `X-Talyvor-Feature` is the DECLARED CATEGORY: proxy.go feeds it to
// alert rules and token_events.feature, and ExtractFromRequest even strips a "code-" prefix from it
// "to keep the dashboard chips readable" — an IDE affordance, not an identifier.
//
// ⚠⚠⚠ THIS REPO ALREADY SHIPPED THAT CONFUSION AS A PRODUCT DEFECT AND WROTE THE POST-MORTEM INTO
// migrations/0116_request_attribution_request_id.sql:
//
//	"request_attribution already stores the issue the user was working on (issue_id, from the
//	 X-Talyvor-Issue header the Code extension sends) ... Track credits an issue by matching the
//	 spend record's FEATURE against an issue identifier, and the extension sends the feature as an
//	 IDE affordance ("code-chat"), so every request from the editor we ship attributed to nothing
//	 in the tracker we ship — even though the issue was known and stored the whole time."
//
// The first version of this file re-made that same substitution one layer up, and the consequence
// was measured rather than argued: with a real signature check added to X-Talyvor-Issue — the exact
// event this guard exists to detect, the event that would let the blocking half of
// docs/tare-phase1d-brief.md be deleted — the guard PASSED, and logged "the work-item id is
// caller-declared" over a tree in which it no longer was. It was also wrong the other way: a
// signature on X-Talyvor-Feature would have redded it with the message "W6.1.4's premise has become
// true", which authenticates a CATEGORY and no work item at all.
//
// ⚠ THE CONCLUSION SURVIVES THE CORRECTION. Measured over the whole tree: X-Talyvor-Issue and
// X-Talyvor-PR are read straight off the request with a plain Header.Get, are never signed and are
// never verified. No X-Talyvor-* attribution header has an HMAC or signature check anywhere in this
// repository. So W6.1.4's "gateway-signed header a client cannot forge" still does not exist, and
// the metering record still cannot honestly attribute a money figure to a work item.
//
// ⚠ WHY X-Talyvor-Feature STAYS IN THE CENSUS ANYWAY: the two refusals this brief rests on
// (internal/mining, internal/cohort) are refusals about THAT field, and talyvor-track's alert path
// still matches an issue on it. It is watched here as a DECLARED CATEGORY, and a verification
// appearing on it is reported as NOT unblocking W6.1.4.
//
// ⚠ WHAT *IS* SIGNED IS SOMETHING ELSE. talyvor-track's internal/lensintegration/webhook.go HMACs
// the WEBHOOK PAYLOAD. The signature authenticates the WORKSPACE; the identifier inside it is still
// the caller's string.
//
// ⚠ WHY THAT BLOCKS W6.1.4 AS WRITTEN. The metering record it specifies carries `delta_cost_usd`
// against `work_item_id`, and reports/tare-design-v1.html says that figure is avoided-COGS feeding
// the same economy that pays cache and distill royalties — justified precisely because the signal
// is "gateway-observed, not participant-asserted". The SAVING is gateway-observed. The
// ATTRIBUTION is participant-asserted. Attributing a money figure to a caller-declared string is
// exactly what internal/mining and internal/cohort already refuse to do.
//
// docs/tare-phase1d-brief.md carries the decision.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// attributionHeaderRole says what a header IS, because the whole point of this file is that the
// two roles were conflated once already.
type attributionHeaderRole int

const (
	// roleWorkItem — the identifier W6.1.4 wants to book money against. A verification appearing
	// on one of these is the event that would make W6.1.4's premise TRUE.
	roleWorkItem attributionHeaderRole = iota
	// roleDeclaredCategory — an operator/IDE label. Signing it authenticates no work item, so a
	// verification on one of these does NOT unblock W6.1.4.
	roleDeclaredCategory
)

// attributionHeaders is the census population, stated rather than implied.
//
// ⚠ EVERY ENTRY CARRIES ITS OWN FLOOR. The floor is PER HEADER and not over the union, because the
// union is exactly how this census could go blind without saying so: X-Talyvor-Feature has five
// reader lines and X-Talyvor-Issue has one, so a union floor of "at least one reader somewhere" is
// satisfied by Feature alone and would keep reporting a clean result after the work-item header
// stopped being read at all.
var attributionHeaders = []struct {
	name string
	role attributionHeaderRole
	why  string
}{
	{"X-Talyvor-Issue", roleWorkItem,
		"the issue the caller was working on; ExtractFromRequest -> AttributionContext.IssueID -> request_attribution.issue_id, the column Track joins spend to (migration 0116)"},
	{"X-Talyvor-PR", roleWorkItem,
		"the pull request; ExtractFromRequest -> GitContext.PRNumber -> request_attribution.pr_number. The design books the saving to \"issue / PR / spec\", so the PR is a work item too"},
	{"X-Talyvor-Feature", roleDeclaredCategory,
		"the declared feature category (proxy.go -> alert rules and token_events.feature). NOT the work item — the extension sends an IDE affordance like \"code-chat\", which is the substitution migration 0116 records as a shipped defect"},
}

// TestPremise_NoWorkItemHeaderIsSignedOrVerified is TWO-SIDED, PER HEADER. The half that stops it
// reporting its own blindness is the floor: each header must actually BE READ. A census that finds
// no verification because it can find nothing at all is worthless — and a census that finds a
// different header's readers is worse, because it looks healthy.
func TestPremise_NoWorkItemHeaderIsSignedOrVerified(t *testing.T) {
	root := "../.."

	readerFiles := map[string]map[string]bool{}
	verifiers := map[string][]string{}
	for _, h := range attributionHeaders {
		readerFiles[h.name] = map[string]bool{}
	}

	err := walkGoSource(root, func(path, src string) {
		rel := strings.TrimPrefix(filepath.ToSlash(path), filepath.ToSlash(root)+"/")
		if strings.HasSuffix(rel, "_test.go") || strings.Contains(rel, "/sdk/") {
			return
		}
		for _, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue // a comment ABOUT a header is not a use of it
			}
			for _, h := range attributionHeaders {
				if !strings.Contains(line, h.name) {
					continue
				}
				if strings.Contains(line, "Header.Get(") || strings.Contains(line, "Header.Set(") {
					readerFiles[h.name][rel] = true
					continue
				}
				// Anything else touching the header is a candidate verification path.
				verifiers[h.name] = append(verifiers[h.name], rel+": "+trimmed)
			}
		}
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	for _, h := range attributionHeaders {
		if len(readerFiles[h.name]) == 0 {
			t.Errorf("no non-test code reads %s at all — this census has gone BLIND for that "+
				"header, and \"nothing verifies it\" would be a statement about the SEARCH rather "+
				"than about the tree.\n%s: %s\n⚠ The other headers in this census still have "+
				"readers, which is exactly why this floor is per-header and not a total.",
				h.name, h.name, h.why)
		}
	}

	for _, h := range attributionHeaders {
		found := verifiers[h.name]
		if len(found) == 0 {
			continue
		}
		sort.Strings(found)
		switch h.role {
		case roleWorkItem:
			t.Errorf("%s — A WORK-ITEM HEADER — now has something other than a plain Get/Set:\n  %s\n\n"+
				"⚠ IF THAT IS A REAL SIGNATURE CHECK, W6.1.4's PREMISE HAS BECOME TRUE: delete this "+
				"test and the blocking half of docs/tare-phase1d-brief.md, and the metering record "+
				"may attribute money to the work item.\n%s: %s",
				h.name, strings.Join(found, "\n  "), h.name, h.why)
		case roleDeclaredCategory:
			t.Errorf("%s — a DECLARED CATEGORY, NOT the work item — now has something other than a "+
				"plain Get/Set:\n  %s\n\n⚠ THIS DOES NOT UNBLOCK W6.1.4. Signing a category "+
				"authenticates no work item; migration 0116 records what happens when the two are "+
				"substituted for one another. If this is a real verification, say what it is for in "+
				"docs/tare-phase1d-brief.md and move this header's role only if the work-item "+
				"identifier genuinely moved into it.\n%s: %s",
				h.name, strings.Join(found, "\n  "), h.name, h.why)
		}
	}

	if t.Failed() {
		return
	}
	for _, h := range attributionHeaders {
		role := "WORK ITEM"
		if h.role == roleDeclaredCategory {
			role = "declared category"
		}
		t.Logf("MEASURED: %s (%s) is read by %d non-test file(s) with a plain Header.Get/Set and "+
			"NOTHING verifies it.", h.name, role, len(readerFiles[h.name]))
	}
	t.Logf("So W6.1.4's \"gateway-signed header a client cannot forge\" does not exist for ANY " +
		"work-item header. The work-item id is caller-declared.")
}

// TestPremise_TheWorkItemHeaderIsTheIssueHeaderNotTheFeatureHeader anchors the ROLE column above in
// the product rather than in this file's opinion — because the role column is the thing that was
// wrong, and a table nobody checks is how it would go wrong again.
//
// ⚠ IT READS THE CODE, NOT A COMMENT. A prose claim that X-Talyvor-Issue is the work item would be
// satisfied by a comment somebody wrote; these two assertions are satisfied only by the extractor
// actually mapping that header onto the field the attribution row stores.
func TestPremise_TheWorkItemHeaderIsTheIssueHeaderNotTheFeatureHeader(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("../..", "internal/attribution/context.go"))
	if err != nil {
		t.Fatalf("read attribution/context.go: %v — the serve-path extractor this premise rests on has moved", err)
	}
	src := string(b)

	var issueLine, substitutedLine string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if !strings.Contains(line, "IssueID:") {
			continue
		}
		if strings.Contains(line, "X-Talyvor-Issue") {
			issueLine = trimmed
		}
		if strings.Contains(line, "X-Talyvor-Feature") {
			substitutedLine = trimmed
		}
	}

	// ⚠ THE SUBSTITUTION IS DIAGNOSED FIRST, DELIBERATELY. Repointing IssueID at X-Talyvor-Feature
	// ALSO removes the X-Talyvor-Issue mapping, so if the general "no longer maps" case were
	// checked first it would fire and this — the specific, already-shipped-once diagnosis — could
	// never be printed. Control X7 measured exactly that: the message was unreachable.
	if substitutedLine != "" {
		t.Fatalf("IssueID is now populated from X-Talyvor-Feature:\n  %s\n"+
			"⚠ THAT IS THE EXACT SUBSTITUTION migration 0116 RECORDS AS A SHIPPED DEFECT — the "+
			"extension sends the feature as an IDE affordance (\"code-chat\"), so every request "+
			"from the editor we ship attributed to nothing in the tracker we ship.", substitutedLine)
	}

	if issueLine == "" {
		t.Fatalf("ExtractFromRequest no longer maps X-Talyvor-Issue onto IssueID.\n" +
			"That mapping is WHY this file treats X-Talyvor-Issue as the work-item header. If the " +
			"work-item id moved to a different header, move it in attributionHeaders too — and if " +
			"it moved into X-Talyvor-Feature, re-read migration 0116 first, which records that " +
			"substitution as a shipped defect.")
	}

	// And the row that mapping feeds must actually carry issue_id — the column Track joins on.
	if !strings.Contains(src, "issue_id") {
		t.Fatalf("recordSQL no longer writes issue_id.\nThe work-item id would then be extracted " +
			"and dropped, and W6.1.4's record would have nothing gateway-side to attribute against " +
			"even if the header were signed.")
	}
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
