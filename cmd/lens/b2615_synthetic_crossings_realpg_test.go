package main

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
)

// B26.15 — THE TEST MONEY THAT REACHED REAL AGENTS BEFORE THE WALL.
//
// Before B25.1, with nothing yet marked test: a test workspace's agent pays a real one, the real one pays it
// back a little, a third transfer is given back, an escrow is released and another is still held, and the test
// workspace buys a real seller's listing whose earning cleared. Beside them, a real-to-real and a test-to-test
// transfer. Then the two test workspaces are marked test (the state B25.1 found).
//
// `lens synthetic-crossings` lists exactly the seven crossing rows and changes nothing; --reverse returns every
// balance — each workspace's LXC and its test-funded part, each agent's, the seller's earnings — to what it was
// before the crossings, with a reversal row and a ledger entry on both sides naming each original; a second
// --reverse reverses nothing.
func TestB2615_SyntheticCrossingsListsTheCrossingsAndReversesThem(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	bank := economy.NewDualTokenStore(nil, pool, nil)
	bank.SetOwnerVerifier(earnverify.New(false))
	const lxc = int64(1_000_000)
	const testWS, realWS, realWS2, testWS2 = "b2615-test", "b2615-real", "b2615-real-2", "b2615-test-2"

	agent := func(ws string, credits int64) economy.Agent {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified) VALUES ($1, $1, $1, true)`, ws); err != nil {
			t.Fatal(err)
		}
		a, err := bank.CreateAgent(ctx, ws, ws+" agent", "owner-"+ws)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bank.CreditLXC(ctx, ws, credits, "stripe top-up", map[string]interface{}{"funding": economy.FundingTest}); err != nil {
			t.Fatal(err)
		}
		if _, err := bank.FundAgent(ctx, ws, a.ID, credits); err != nil {
			t.Fatal(err)
		}
		return a
	}
	tester, real, real2, tester2 := agent(testWS, 500*lxc), agent(realWS, 100*lxc), agent(realWS2, 100*lxc), agent(testWS2, 100*lxc)

	// The two that never cross.
	if _, err := bank.SendCredits(ctx, realWS, real.ID, real2.ID, 4*lxc, "real to real"); err != nil {
		t.Fatal(err)
	}
	if _, err := bank.SendCredits(ctx, testWS, tester.ID, tester2.ID, 3*lxc, "test to test"); err != nil {
		t.Fatal(err)
	}

	snapshot := func() map[string]int64 {
		t.Helper()
		got := map[string]int64{}
		for _, ws := range []string{testWS, realWS, realWS2, testWS2} {
			var bal, testFunded, earned int64
			if err := pool.QueryRow(ctx, `SELECT balance, test_funded_ulxc,
				COALESCE((SELECT sum(e.share_usd_micros) FROM market_earnings e WHERE e.seller_workspace_id = $1
					AND NOT EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = e.use_id)), 0)::bigint
				FROM lxc_balances WHERE workspace_id = $1`, ws).Scan(&bal, &testFunded, &earned); err != nil {
				t.Fatal(err)
			}
			got["lxc "+ws], got["test-funded "+ws], got["earned "+ws] = bal, testFunded, earned
		}
		for _, a := range []economy.Agent{tester, real, real2, tester2} {
			var held int64
			if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_postings WHERE account = 'agent:' || $1`,
				a.ID).Scan(&held); err != nil {
				t.Fatal(err)
			}
			got["agent "+a.ID] = held
		}
		return got
	}
	before := snapshot()

	// The crossings, as they were made before the wall.
	send := func(ws string, from economy.Agent, to economy.Agent, amount int64) economy.AgentTransfer {
		t.Helper()
		x, err := bank.SendCredits(ctx, ws, from.ID, to.ID, amount, "before the wall")
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	paid := send(testWS, tester, real, 50*lxc)
	paidBack := send(realWS, real, tester, 5*lxc)
	given := send(testWS, tester, real, 7*lxc)
	givenBack, err := bank.RefundTransfer(ctx, realWS, given.ID)
	if err != nil {
		t.Fatal(err)
	}
	released, err := bank.PayIntoEscrow(ctx, testWS, tester.ID, real.ID, 20*lxc, "delivered", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bank.ConfirmEscrow(ctx, testWS, released.ID); err != nil {
		t.Fatal(err)
	}
	held, err := bank.PayIntoEscrow(ctx, testWS, tester.ID, real.ID, 10*lxc, "not yet", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	const use = "use_b2615"
	if _, err := pool.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, agent_id, price_ulxc,
		charge, ran_at, metered_at, cleared_invoice_id, cleared_at) VALUES ($1, 'lst_b2615', 1, $2, $3, $4, $5, 'billed', now(), now(), 'in_b2615', now())`,
		use, realWS, testWS, tester.ID, 3*lxc); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO market_earnings (use_id, seller_workspace_id, gross_usd_micros, share_usd_micros, invoice_id, cleared_at,
		payable_at) VALUES ($1, $2, 300000, 300000, 'in_b2615', now(), now() + interval '14 days')`, use, realWS); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET synthetic = true WHERE id IN ($1, $2)`, testWS, testWS2); err != nil {
		t.Fatal(err)
	}
	crossed := snapshot()
	if maps.Equal(crossed, before) {
		t.Fatal("the crossings moved nothing: the test proves nothing")
	}

	run := func(args ...string) (string, error) {
		t.Helper()
		var out bytes.Buffer
		err := syntheticCrossingsCommand(ctx, bank, args, "operator-cli:b2615", &out)
		return out.String(), err
	}
	// listed returns the id and status of each row the command printed.
	listed := func(out string) map[string]string {
		rows := map[string]string{}
		for _, line := range strings.Split(out, "\n") {
			f := strings.Split(line, "\t")
			if len(f) == 8 {
				rows[f[1]] = f[7]
			}
		}
		return rows
	}

	// Read only: exactly the seven crossings, which of them are left, and nothing changed.
	out, err := run()
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + out)
	toReverse := []string{paid.ID, paidBack.ID, released.ID, held.ID, use}
	want := map[string]string{given.ID: "nothing to reverse: it was given back by " + givenBack.ID, givenBack.ID: "nothing to reverse: it gives back " + given.ID}
	for _, id := range toReverse {
		want[id] = "TO REVERSE"
	}
	if got := listed(out); !maps.Equal(got, want) {
		t.Fatalf("listed %v\nwant %v", got, want)
	}
	if !strings.Contains(out, "7 crossings, 5 left to reverse") {
		t.Fatalf("no summary in:\n%s", out)
	}
	if after := snapshot(); !maps.Equal(after, crossed) {
		t.Fatalf("listing changed balances: %v -> %v", crossed, after)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM test_crossing_reversals`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("listing wrote %d reversals (%v)", n, err)
	}

	// --reverse: every balance back to what it was before the crossings.
	if out, err = run("--reverse"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Log("\n" + out)
	if after := snapshot(); !maps.Equal(after, before) {
		for k := range before {
			if after[k] != before[k] {
				t.Errorf("%s = %d after the reversal, %d before the crossings (%d with them)", k, after[k], before[k], crossed[k])
			}
		}
		t.FailNow()
	}
	// One reversal row per crossing left, each naming its original…
	rows, err := pool.Query(ctx, `SELECT original_id FROM test_crossing_reversals ORDER BY original_id`)
	if err != nil {
		t.Fatal(err)
	}
	var reversed []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		reversed = append(reversed, id)
	}
	if slices.Sort(toReverse); !slices.Equal(reversed, toReverse) {
		t.Fatalf("reversal rows name %v, want %v", reversed, toReverse)
	}
	// …and, for each transfer and the released escrow, a ledger row on both sides and a posting on both agents
	// naming it.
	for _, id := range []string{paid.ID, paidBack.ID, released.ID} {
		var ledgerSides, postingSides int
		if err := pool.QueryRow(ctx, `SELECT
			(SELECT count(DISTINCT workspace_id) FROM lxc_ledger WHERE type = 'test_crossing_reversal' AND metadata::jsonb ->> 'original_id' = $1),
			(SELECT count(DISTINCT workspace_id) FROM agent_postings WHERE kind = 'reversal' AND ref = $1)`, id).Scan(&ledgerSides, &postingSides); err != nil {
			t.Fatal(err)
		}
		if ledgerSides != 2 || postingSides != 2 {
			t.Fatalf("%s: ledger rows in %d workspaces and postings in %d, want both sides of each", id, ledgerSides, postingSides)
		}
	}
	var heldStatus, refundCause string
	if err := pool.QueryRow(ctx, `SELECT (SELECT status FROM agent_escrows WHERE id = $1), (SELECT cause FROM market_refunds WHERE use_id = $2)`,
		held.ID, use).Scan(&heldStatus, &refundCause); err != nil || heldStatus != "returned" || refundCause != "test_crossing" {
		t.Fatalf("held escrow %q and use refund %q (%v); want returned and test_crossing", heldStatus, refundCause, err)
	}

	// Once: a second --reverse reverses nothing and moves nothing.
	if out, err = run("--reverse"); err != nil || !strings.Contains(out, "7 crossings, 0 reversed now") {
		t.Fatalf("second --reverse = %v\n%s", err, out)
	}
	if after := snapshot(); !maps.Equal(after, before) {
		t.Fatalf("a second --reverse moved money: %v", after)
	}
}
