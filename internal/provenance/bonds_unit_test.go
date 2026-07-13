package provenance

import "testing"

// INTEGER-EXACT slash math: floor(amount * bps / 10000). Rounding can NEVER burn more than the exact
// fraction, NEVER exceed the bond, and NEVER create value (mint). All integer µLENS — no float anywhere.
func TestSlashAmount_IntegerExact_NeverMintsNeverExceeds(t *testing.T) {
	cases := []struct {
		amount int64
		bps    int
		want   int64
	}{
		{1_000_000, 10000, 1_000_000}, // 100% → whole bond
		{1_000_000, 5000, 500_000},    // 50%
		{1_000_000, 1, 100},           // 0.01% → exact
		{3, 5000, 1},                  // floor(1.5)=1 — rounds DOWN, never up
		{1, 5000, 0},                  // floor(0.5)=0 — sub-µLENS remainder NEVER burned, never minted
		{7, 3333, 2},                  // floor(2.333)=2
		{9_223_372_036_854, 10000, 9_223_372_036_854}, // large bond, no overflow (big.Int)
	}
	for _, c := range cases {
		got := slashAmount(c.amount, c.bps)
		if got != c.want {
			t.Errorf("slashAmount(%d,%d)=%d want %d", c.amount, c.bps, got, c.want)
		}
		if got > c.amount {
			t.Errorf("slashAmount(%d,%d)=%d EXCEEDS the bond — would mint value", c.amount, c.bps, got)
		}
		// floor: the burn must never exceed the exact real fraction (no rounding up).
		if int64(c.bps)*c.amount/10000 < got {
			t.Errorf("slashAmount(%d,%d)=%d rounded UP — must floor", c.amount, c.bps, got)
		}
	}
}

// The server-derived keys contain every identity they protect (SEC-11) and are deterministic + distinct.
func TestKeys_ServerDerived_Distinct(t *testing.T) {
	b1 := BondID("ws-A", "oid-1")
	if b1 != BondID("ws-A", "oid-1") {
		t.Error("BondID must be deterministic")
	}
	if b1 == BondID("ws-B", "oid-1") || b1 == BondID("ws-A", "oid-2") {
		t.Error("BondID must vary with workspace AND output")
	}
	if len(b1) != 64 {
		t.Errorf("BondID must be a sha256 hex; got %d chars", len(b1))
	}
	k := slashKey(b1, "oid-1", "compile_failed", "self_reported")
	if k != slashKey(b1, "oid-1", "compile_failed", "self_reported") {
		t.Error("slashKey must be deterministic")
	}
	// Every identity it protects changes the key: bond, output, verdict, source.
	if k == slashKey("other-bond", "oid-1", "compile_failed", "self_reported") ||
		k == slashKey(b1, "oid-2", "compile_failed", "self_reported") ||
		k == slashKey(b1, "oid-1", "tests_failed", "self_reported") ||
		k == slashKey(b1, "oid-1", "compile_failed", "attested") {
		t.Error("slashKey must contain (and vary with) bond, output, verdict, source")
	}
}
