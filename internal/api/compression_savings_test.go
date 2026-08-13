package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/compressmeasure"
)

// stubSavingsStore records the window it was asked for and returns a fixed
// summary, so the clamp can be asserted on the ARGUMENT rather than on a
// description of the clamp.
type stubSavingsStore struct {
	since time.Time
	sum   compressmeasure.Summary
	err   error
}

func (s *stubSavingsStore) Summarise(_ context.Context, _ string, since time.Time) (compressmeasure.Summary, error) {
	s.since = since
	return s.sum, s.err
}

// A nil store is "not wired", and the route turns this into 503. It must never
// become a 200 with zeroes: an operator looking at an empty screen has to be able
// to tell a broken deployment from a quiet one.
func TestSavings_ANilStoreIsNotWiredRatherThanZero(t *testing.T) {
	_, err := ReadCompressionSavings(context.Background(), nil, "ws", 30, time.Now())
	if !errors.Is(err, ErrNoCompressionSavingsStore) {
		t.Fatalf("err = %v, want ErrNoCompressionSavingsStore", err)
	}
}

// THE TYPED-NIL HAZARD, asserted rather than commented. A (*compressmeasure.Store)(nil)
// placed in the CompressionSavingsStore interface is NOT == nil, so the guard
// above does not fire for it. Without ErrUnwired this call would return a
// confident 200 carrying "0 requests, 0 bytes removed" from a store with no
// database behind it — a zero from an instrument that read nothing.
// isNilInterface reports whether the interface VALUE is nil. It exists as a
// function rather than an inline `store == nil` because staticcheck folds that
// comparison away when it can see the concrete type at the comparison site
// (SA4023) — and folding it away would delete the premise this test rests on.
func isNilInterface(v CompressionSavingsStore) bool { return v == nil }

func TestSavings_ATypedNilStoreIsAlsoNotWired(t *testing.T) {
	var store CompressionSavingsStore = (*compressmeasure.Store)(nil)
	if isNilInterface(store) {
		t.Fatal("premise: a typed-nil in an interface must not compare equal to nil, or this test is pointless")
	}
	if isNilInterface(nil) != true {
		t.Fatal("control: isNilInterface must report a genuinely nil interface as nil, " +
			"or the premise check above is vacuous")
	}
	_, err := ReadCompressionSavings(context.Background(), store, "ws", 30, time.Now())
	if !errors.Is(err, ErrNoCompressionSavingsStore) {
		t.Fatalf("err = %v, want ErrNoCompressionSavingsStore — a store with no pool served a summary", err)
	}
}

// An unwired store built the ordinary way (NewStore(nil)) is the same answer.
func TestSavings_AStoreBuiltOnANilPoolIsNotWired(t *testing.T) {
	_, err := ReadCompressionSavings(context.Background(), compressmeasure.NewStore(nil), "ws", 30, time.Now())
	if !errors.Is(err, ErrNoCompressionSavingsStore) {
		t.Fatalf("err = %v, want ErrNoCompressionSavingsStore", err)
	}
}

// The clamp is asserted on the timestamp the store actually received, both ends
// and the default, so a clamp that silently stopped applying cannot pass.
func TestSavings_TheWindowIsClamped(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ in, want int }{
		{in: 0, want: 30},   // unset → default
		{in: -5, want: 30},  // negative → default
		{in: 7, want: 7},    // inside the range → honoured
		{in: 90, want: 90},  // upper bound → honoured
		{in: 365, want: 90}, // above → clamped
	} {
		st := &stubSavingsStore{}
		got, err := ReadCompressionSavings(context.Background(), st, "ws", tc.in, now)
		if err != nil {
			t.Fatalf("days=%d: %v", tc.in, err)
		}
		if got.Days != tc.want {
			t.Errorf("days=%d: reported window %d, want %d", tc.in, got.Days, tc.want)
		}
		if want := now.AddDate(0, 0, -tc.want); !st.since.Equal(want) {
			t.Errorf("days=%d: store queried since %s, want %s", tc.in, st.since, want)
		}
	}
}

// THE THREE STATES MUST RENDER DIFFERENTLY. "not wired", "wired but nothing
// measured" and "measured, and it saved nothing" are three different answers, and
// the last is the one this rewriter is most likely to give (0 of 308 committed
// corpus prompts modified). A screen that cannot tell them apart is how a
// structural zero gets reported as a measurement.
func TestSavings_MeasuredNothingIsNotTheSameAsMeasuredZeroSaving(t *testing.T) {
	now := time.Now()

	quiet := &stubSavingsStore{sum: compressmeasure.Summary{}}
	gotQuiet, err := ReadCompressionSavings(context.Background(), quiet, "ws", 30, now)
	if err != nil {
		t.Fatalf("quiet: %v", err)
	}
	if gotQuiet.Requests != 0 {
		t.Fatalf("premise: the quiet case must report 0 requests, got %d", gotQuiet.Requests)
	}

	ran := &stubSavingsStore{sum: compressmeasure.Summary{
		Requests: 308, Modified: 0, OriginalBytes: 10868, SentBytes: 10868, BytesRemoved: 0,
	}}
	gotRan, err := ReadCompressionSavings(context.Background(), ran, "ws", 30, now)
	if err != nil {
		t.Fatalf("ran: %v", err)
	}
	if gotRan.Requests != 308 {
		t.Errorf("requests = %d, want 308", gotRan.Requests)
	}
	if gotRan.BytesRemoved != 0 {
		t.Errorf("bytes_removed = %d, want 0", gotRan.BytesRemoved)
	}

	qj, _ := json.Marshal(gotQuiet)
	rj, _ := json.Marshal(gotRan)
	if string(qj) == string(rj) {
		t.Errorf("a workspace that measured NOTHING and one that measured 308 saving-free requests "+
			"serialise identically: %s", qj)
	}
}

// NO PERCENTAGE, asserted on the wire format. Three wrong customer-facing numbers
// in this repo came out of one savings-percentage column family; the JSON this
// endpoint emits is the place that mistake would come back.
func TestSavings_TheJSONCarriesNoPercentage(t *testing.T) {
	b, err := json.Marshal(CompressionSavings{
		Summary: compressmeasure.Summary{Requests: 5, Modified: 2, OriginalBytes: 400, SentBytes: 380, BytesRemoved: 20},
		Days:    30,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(b)
	for _, banned := range []string{"pct", "percent", "ratio", "savings_usd", "saved_usd"} {
		if strings.Contains(body, banned) {
			t.Errorf("the summary JSON contains %q: %s", banned, body)
		}
	}
	// And the fields that carry the honesty must actually be present — a
	// banned-substring sweep is green on an empty document.
	for _, required := range []string{`"requests"`, `"bytes_removed"`, `"estimated_path_requests"`, `"days"`} {
		if !strings.Contains(body, required) {
			t.Errorf("the summary JSON is missing %s: %s", required, body)
		}
	}
}

// A store error is surfaced, not swallowed into a zero.
func TestSavings_AStoreErrorIsNotRenderedAsZero(t *testing.T) {
	boom := errors.New("connection refused")
	_, err := ReadCompressionSavings(context.Background(), &stubSavingsStore{err: boom}, "ws", 30, time.Now())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the store's error", err)
	}
}
