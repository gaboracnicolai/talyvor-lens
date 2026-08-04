package api

import (
	"context"
	"testing"
)

// PER-ISSUE ATTRIBUTION — the editor we ship must attribute to the tracker we ship.
//
// Track credits an issue by matching the spend record's feature against an issue IDENTIFIER
// (RecordRequestSpend → WHERE identifier = $2). The Code extension sends the feature as an IDE
// AFFORDANCE ("code-chat", "code-completion"), so it matches nothing and every extension request
// lands unattributed. The issue the user was working on IS known — the extension sends
// X-Talyvor-Issue and Lens stores it — but it was stored in request_attribution, which has no
// request_id, so nothing could join it back to the spend row.
//
// ⚠ THIS TEST IS THE LEDGER-LEVEL CLAIM: a request carrying issue "ENG-42" and feature "code-chat"
// must surface ENG-42 on /v1/api/spend/by-request, which is the substrate Track's syncer reads.
// Asserted over real Postgres against the real query, not against a struct.

// The ledger-level claim, end to end over real rows: a spend row whose attribution carries issue
// ENG-42 and feature "code-chat" must surface ENG-42. Today it surfaced nothing.
func TestSpendByRequest_RowWithIssueSurfacesIt(t *testing.T) {
	s := spendHarness(t)
	pool := s.pool
	ctx := context.Background()
	const ws = "ws-issue-attr"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM token_events WHERE workspace_id=$1`, ws)
		_, _ = pool.Exec(context.Background(), `DELETE FROM request_attribution WHERE workspace_id=$1`, ws)
	})
	_, _ = pool.Exec(ctx, `DELETE FROM token_events WHERE workspace_id=$1`, ws)
	_, _ = pool.Exec(ctx, `DELETE FROM request_attribution WHERE workspace_id=$1`, ws)

	if _, err := pool.Exec(ctx, `INSERT INTO token_events
		(workspace_id, request_id, feature, cost_usd, input_tokens, output_tokens)
		VALUES ($1,'req-A','code-chat',0.01,10,20), ($1,'req-B','code-chat',0.02,10,20)`, ws); err != nil {
		t.Fatalf("seed token_events: %v", err)
	}
	// Only req-A has attribution — req-B stands in for an untagged request.
	if _, err := pool.Exec(ctx, `INSERT INTO request_attribution
		(workspace_id, feature, issue_id, request_id) VALUES ($1,'code-chat','ENG-42','req-A')`, ws); err != nil {
		t.Fatalf("seed attribution: %v", err)
	}

	rows, err := pool.Query(ctx, spendByRequestSQL, ws, 1, nil, 100)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, reqID, feature, issue, serveSource string
		var cost float64
		var inT, outT int64
		var ts any
		if err := rows.Scan(&id, &reqID, &feature, &issue, &cost, &inT, &outT, &ts, &serveSource); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[reqID] = issue
	}
	if got["req-A"] != "ENG-42" {
		t.Errorf("req-A issue = %q, want ENG-42 — the issue was stored and still did not reach the "+
			"substrate Track reads, so the editor's spend attributes to nothing", got["req-A"])
	}
	// ⚠ THE LEFT-JOIN CONTROL, and #66's rule: an untagged request must still be REPORTED, with an
	// empty issue. An inner join would drop it, turning unattributed spend into invisible spend.
	if _, present := got["req-B"]; !present {
		t.Error("req-B vanished from by-request — a request with no attribution row must still be " +
			"reported, or unattributed spend stops being recorded at all")
	}
	if got["req-B"] != "" {
		t.Errorf("req-B issue = %q, want empty — an issue was invented for an untagged request", got["req-B"])
	}
}
