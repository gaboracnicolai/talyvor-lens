package tare_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/tare"
)

const logLine = "2026-08-28T01:02:03Z ERROR db: connection refused dialing 10.0.0.7:5432 (attempt 14)"

func repeatedLog(n int) []byte {
	var b strings.Builder
	b.WriteString("2026-08-28T01:02:02Z INFO db: dialing primary\n")
	for i := 0; i < n; i++ {
		b.WriteString(logLine)
		b.WriteString("\n")
	}
	b.WriteString("2026-08-28T01:02:09Z INFO db: connected to 10.0.0.8:5432\n")
	return []byte(b.String())
}

func reduceLog(t *testing.T, in []byte) ([]byte, int, int) {
	t.Helper()
	out, tin, tout, err := tare.NewLogCollapse().Reduce(context.Background(), in, tare.KindLog)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	return out, tin, tout
}

func TestLog_CollapsesARunAndSaysHowManyItHid(t *testing.T) {
	in := repeatedLog(40)
	out, tin, tout := reduceLog(t, in)
	if len(out) >= len(in) {
		t.Fatalf("%d bytes in, %d out — not a reduction", len(in), len(out))
	}
	if tout > tin {
		t.Errorf("tokensOut %d > tokensIn %d", tout, tin)
	}
	if !bytes.Contains(out, []byte(tare.RepeatMarkerPrefix+"39")) {
		t.Errorf("40 occurrences should leave one line and hide 39; output:\n%s", out)
	}
	// The line itself must still be there ONCE — a collapse that dropped the content and kept only
	// a count would be smaller still and completely useless.
	if bytes.Count(out, []byte(logLine)) != 1 {
		t.Errorf("the collapsed line appears %d times, want exactly 1:\n%s",
			bytes.Count(out, []byte(logLine)), out)
	}
	// And the lines around the run are untouched.
	for _, keep := range []string{"dialing primary", "connected to 10.0.0.8:5432"} {
		if !bytes.Contains(out, []byte(keep)) {
			t.Errorf("output lost surrounding line %q:\n%s", keep, out)
		}
	}
}

// ⚠ THE PROPERTY THE DESIGN ASKS FOR, AND THE ONE THAT MAKES THE must-keep CLAUSE MOOT HERE.
// Byte-exact, over inputs that differ only in the ways a log actually varies.
func TestLog_RoundTripsByteForByte(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"trailing newline", repeatedLog(12)},
		{"NO trailing newline", bytes.TrimSuffix(repeatedLog(12), []byte("\n"))},
		{"CRLF line endings", bytes.ReplaceAll(repeatedLog(12), []byte("\n"), []byte("\r\n"))},
		{"run at the very start", []byte(logLine + "\n" + logLine + "\n" + logLine + "\ntail line here\n")},
		{"run at the very end", []byte("head line here\n" + strings.Repeat(logLine+"\n", 5))},
		{"two separate runs", []byte(strings.Repeat(logLine+"\n", 4) + "between\n" +
			strings.Repeat("2026-08-28T01:03:00Z WARN cache: evicting key user:42:profile (size 8192)\n", 4))},
		{"blank lines inside the run region", []byte(strings.Repeat(logLine+"\n", 6) + "\n\n\ntail\n")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, _, _ := reduceLog(t, c.in)
			if bytes.Equal(out, c.in) {
				t.Fatalf("sample came back unchanged — the round-trip below would be vacuous")
			}
			back, err := tare.ExpandLog(out)
			if err != nil {
				t.Fatalf("ExpandLog: %v", err)
			}
			if !bytes.Equal(back, c.in) {
				t.Errorf("not byte-exact.\n  in  (%d): %q\n  back(%d): %q", len(c.in), c.in, len(back), back)
			}
		})
	}
}

// ⚠ EVERY NUMBER, PATH AND ID SURVIVES — stated as a test rather than as a comment, because this is
// the claim the design's "hardcoded must-keep for numbers/paths/IDs" is about and this reducer
// satisfies it by discarding nothing rather than by having a keep-list.
func TestLog_NumbersPathsAndIDsAllSurviveTheRoundTrip(t *testing.T) {
	in := []byte(strings.Repeat(
		"2026-08-28T01:02:03Z ERROR /var/log/app/worker-7.log req=01J9ZK3P8QW4 user=42 bytes=9007199254740993\n", 8) +
		"2026-08-28T01:02:11Z INFO /srv/data/shard-03/index.bin sha=deadbeefcafe1234 rows=1048576\n")
	out, _, _ := reduceLog(t, in)
	back, err := tare.ExpandLog(out)
	if err != nil {
		t.Fatalf("ExpandLog: %v", err)
	}
	if !bytes.Equal(back, in) {
		t.Fatalf("round-trip changed the bytes")
	}
	for _, tok := range []string{
		"/var/log/app/worker-7.log", "01J9ZK3P8QW4", "user=42",
		"9007199254740993", // beyond 2^53 — the value the JSON path had to be careful about
		"/srv/data/shard-03/index.bin", "deadbeefcafe1234", "1048576",
	} {
		if !bytes.Contains(back, []byte(tok)) {
			t.Errorf("restored form lost %q", tok)
		}
		if strings.Count(string(back), tok) != strings.Count(string(in), tok) {
			t.Errorf("%q appears %d times, want %d", tok,
				strings.Count(string(back), tok), strings.Count(string(in), tok))
		}
	}
}

func TestLog_RefusesAndReturnsTheInputUnchanged(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		kind tare.Kind
	}{
		{"wrong kind", repeatedLog(20), tare.KindJSON},
		{"empty", []byte("   \n\n"), tare.KindLog},
		{"no repeated lines at all", []byte("alpha\nbeta\ngamma\ndelta\n"), tare.KindLog},
		{"repeats too short to pay for the marker", []byte("ok\nok\nok\nend\n"), tare.KindLog},
		{"content already carries the marker", []byte(strings.Repeat(logLine+"\n", 9) +
			tare.RepeatMarkerPrefix + "3" + "⟧\n"), tare.KindLog},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, tin, tout, err := tare.NewLogCollapse().Reduce(context.Background(), c.in, c.kind)
			if err != nil {
				t.Fatalf("refusal must be (content, nil), got err=%v", err)
			}
			if !bytes.Equal(out, c.in) {
				t.Errorf("refused but changed the content.\n  in:  %q\n  out: %q", c.in, out)
			}
			if tin != tout {
				t.Errorf("refused and booked a saving: tokensIn=%d tokensOut=%d", tin, tout)
			}
		})
	}
}

// ⚠ THE REFUSAL REASON REACHES THE OBSERVER, because a refusal nobody can count is indistinguishable
// from a reduction that happened to save nothing.
func TestLog_RefusalReasonsReachTheObserver(t *testing.T) {
	want := map[string]tare.Kind{
		tare.ReasonWrongKind:     tare.KindJSON,
		tare.ReasonNoRepeats:     tare.KindLog,
		tare.ReasonMarkerInInput: tare.KindLog,
		tare.ReasonEmpty:         tare.KindLog,
	}
	inputs := map[string][]byte{
		tare.ReasonWrongKind:     repeatedLog(20),
		tare.ReasonNoRepeats:     []byte("alpha\nbeta\ngamma\n"),
		tare.ReasonMarkerInInput: []byte(tare.RepeatMarkerPrefix + "2⟧\nalpha\n"),
		tare.ReasonEmpty:         []byte("  \n"),
	}
	for reason, kind := range want {
		var got []tare.Refusal
		r := tare.NewLogCollapse().WithObserver(func(f tare.Refusal) { got = append(got, f) })
		if _, _, _, err := r.Reduce(context.Background(), inputs[reason], kind); err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		if len(got) != 1 || got[0].Reason != reason {
			t.Errorf("want one refusal %q, got %+v", reason, got)
		}
	}
}

// ⚠ NON-ADJACENT DUPLICATES ARE NOT COLLAPSED, and that is a correctness property rather than a
// limitation to apologise for: recovering their positions would be needed to put them back.
func TestLog_LeavesNonAdjacentDuplicatesAlone(t *testing.T) {
	in := []byte(logLine + "\nsomething else entirely happened here\n" + logLine + "\n")
	out, _, _, err := tare.NewLogCollapse().Reduce(context.Background(), in, tare.KindLog)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Errorf("collapsed non-adjacent duplicates, which cannot be reversed:\n%s", out)
	}
}

func TestLog_IsDeterministic(t *testing.T) {
	in := repeatedLog(31)
	first, _, _ := reduceLog(t, in)
	for i := 0; i < 20; i++ {
		got, _, _ := reduceLog(t, in)
		if !bytes.Equal(got, first) {
			t.Fatalf("run %d differs from the first", i)
		}
	}
}

func TestLog_NeverReturnsSomethingLargerThanTheInput(t *testing.T) {
	for _, in := range [][]byte{
		repeatedLog(2), repeatedLog(3), repeatedLog(40),
		[]byte("a\na\na\na\na\n"), []byte("\n\n\n\n\n\n\n\n"),
		[]byte("x\n"), []byte(strings.Repeat("short\n", 100)),
	} {
		out, _, _, err := tare.NewLogCollapse().Reduce(context.Background(), in, tare.KindLog)
		if err != nil {
			t.Fatalf("Reduce: %v", err)
		}
		if len(out) > len(in) {
			t.Errorf("%d bytes in, %d out", len(in), len(out))
		}
	}
}

// ⚠ ExpandLog MUST REFUSE RATHER THAN INVENT. A marker it cannot make sense of means the input did
// not come from this reducer, and manufacturing log lines is the harm the whole package exists to
// prevent.
func TestLog_ExpandRefusesMalformedInputRatherThanGuessing(t *testing.T) {
	for _, c := range []struct{ name, in string }{
		{"marker with no line above it", tare.RepeatMarkerPrefix + "3⟧\nalpha\n"},
		{"count is not a number", "alpha\n" + tare.RepeatMarkerPrefix + "many⟧\n"},
		{"count is zero", "alpha\n" + tare.RepeatMarkerPrefix + "0⟧\n"},
		{"count is negative", "alpha\n" + tare.RepeatMarkerPrefix + "-2⟧\n"},
	} {
		if _, err := tare.ExpandLog([]byte(c.in)); err == nil {
			t.Errorf("%s: ExpandLog accepted it and produced an expansion", c.name)
		}
	}
	// And it must still accept what the reducer really produces.
	out, _, _ := reduceLog(t, repeatedLog(9))
	if _, err := tare.ExpandLog(out); err != nil {
		t.Errorf("ExpandLog rejected this reducer's own output: %v", err)
	}
}
