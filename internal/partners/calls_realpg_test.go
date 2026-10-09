package partners

import (
	"context"
	"errors"
	"testing"
)

// B37.5 — a payment through the registry leaves its row in partner_calls (0238), and the row cannot be changed.
func TestCallStore_APaymentLeavesItsAuditRow(t *testing.T) {
	pool := taxPool(t)
	r := NewRegistry(nil)
	r.UseCallLog(NewCallStore(pool))
	ctx := WithWorkspace(context.Background(), "ws_b375")
	account := must(r.Account(ctx, "test"))
	gbp := must(account.OpenAccount(ctx, AccountRequest{ID: "acct-b375", Holder: "Ada Lovelace", Currency: "GBP"}))
	must(account.SendPayment(ctx, PaymentRequest{ID: "pay-b375", AccountRef: gbp.Ref, Amount: Money{10_00, "GBP"},
		Payee: Payee{Name: "Grace Hopper", AccountNumber: "12345678"}, Reference: "invoice 7"}))
	if _, err := account.SendPayment(ctx, PaymentRequest{ID: "pay-b375", AccountRef: gbp.Ref, Amount: Money{11_00, "GBP"}}); !errors.Is(err, ErrIDReused) {
		t.Fatalf("a reused id: %v", err)
	}

	var ws, method, id, outcome string
	var durationUS int64
	rows, err := pool.Query(ctx, `SELECT workspace_id, method, idempotency_id, outcome, duration_us FROM partner_calls
		WHERE service = 'account' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		if err := rows.Scan(&ws, &method, &id, &outcome, &durationUS); err != nil {
			t.Fatal(err)
		}
		if ws != "ws_b375" || durationUS < 0 {
			t.Errorf("%s: workspace %q, %dµs", method, ws, durationUS)
		}
		got = append(got, method+" "+id+" "+outcome)
	}
	want := []string{"OpenAccount acct-b375 ok", "SendPayment pay-b375 ok", "SendPayment pay-b375 refused"}
	if len(got) != len(want) {
		t.Fatalf("partner_calls = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("partner_calls[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for _, change := range []string{`DELETE FROM partner_calls`, `TRUNCATE partner_calls`} {
		if _, err := pool.Exec(ctx, change); err == nil {
			t.Errorf("%s changed the audit", change)
		}
	}
}
