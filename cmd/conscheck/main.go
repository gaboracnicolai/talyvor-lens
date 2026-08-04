// conscheck — does the entity discriminator gate (#392) protect CONSUMER traffic?
//
// #392 was measured on ENGINEERING pairs, where the entity that differs is a version number, an
// exception name, or a command — all of which have a SHAPE, which is why a structural rule caught
// them. A public chat product's dangerous pair has no shape: one lowercase common noun changes and
// the correct answer inverts.
//
// This measures, on the same embedder and threshold production runs:
//  1. similarity for consumer danger pairs and consumer rephrasings
//  2. the LIVE gate's verdict on each (discriminator.Match — the exact test the pooled SELECT applies)
//  3. what the gate costs an honest user (genuine rephrasings it refuses)
//  4. a PROTOTYPE consumer entity tier, to price the fix rather than assert it
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/discriminator"
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/poolsafety"
)

type row struct {
	P          P
	Set        string
	Sim        float64
	GateAllow  bool // live gate: true => may pool if similarity clears
	ProtoAllow bool
}

type P = poolsafety.RephrasePair

func main() {
	key := os.Getenv("LENS_OPENAI_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "conscheck: no LENS_OPENAI_API_KEY")
		os.Exit(1)
	}
	model := envOr("LENS_EMBEDDING_MODEL", "text-embedding-3-small")
	th := 0.92 // config.DefaultSemanticThreshold — production

	emb := embedder.NewOpenAIEmbedder(key, model, os.Getenv("LENS_EMBEDDING_BASE_URL"))
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	// ── POSITIVE CONTROLS. A surprising number is only believable from a proven ruler.
	ctrl, err := poolsafety.ScorePairs(ctx, emb, poolsafety.PositiveControls())
	if err != nil {
		fmt.Fprintln(os.Stderr, "conscheck:", err)
		os.Exit(1)
	}
	fmt.Printf("POSITIVE CONTROLS — model %s, threshold %.2f\n", model, th)
	void := false
	for _, c := range ctrl {
		v := "ok"
		if c.Pair.Name == "identical" && c.Similarity < 0.999 {
			v, void = "⚠ INSTRUMENT BROKEN — VOID", true
		}
		// ⚠ The repo's own contract voids on `identical` alone. "capital"→"capitol" swaps in a
		// DIFFERENT REAL WORD, so a low score here is a finding about the embedder, not a fault
		// in the ruler; only a collapse (<0.80) would indicate a broken client.
		if c.Pair.Name == "typo" && c.Similarity < 0.80 {
			v, void = "⚠ INSTRUMENT SUSPECT", true
		}
		fmt.Printf("  %-11s %.4f  %s\n", c.Pair.Name, c.Similarity, v)
	}
	if void {
		os.Exit(2)
	}

	sets := []struct {
		name  string
		pairs []P
	}{
		{"danger/committed", poolsafety.ConsumerCommittedDangerPairs()},
		{"danger/extension", poolsafety.ConsumerDangerPairs()},
		{"rephrase/committed", poolsafety.RephrasePairs()},
		{"rephrase/extension", poolsafety.ConsumerRephrasePairs()},
	}

	var all []row
	for _, s := range sets {
		scores, err := poolsafety.ScorePairs(ctx, emb, s.pairs)
		if err != nil {
			fmt.Fprintln(os.Stderr, "conscheck:", err)
			os.Exit(1)
		}
		for _, sc := range scores {
			all = append(all, row{
				P: sc.Pair, Set: s.name, Sim: sc.Similarity,
				GateAllow:  discriminator.Match(sc.Pair.A, sc.Pair.B),
				ProtoAllow: protoMatch(sc.Pair.A, sc.Pair.B),
			})
		}
	}

	danger := filter(all, func(r row) bool { return strings.HasPrefix(r.Set, "danger") })
	reph := filter(all, func(r row) bool { return strings.HasPrefix(r.Set, "rephrase") })

	section("1. CONSUMER DANGER CORPUS — pairs whose correct answers differ", danger, th, true)
	section("2. CONSUMER REPHRASINGS — pairs that SHOULD pool", reph, th, false)

	// ── 3. THE GATE'S VERDICT, which is the question asked.
	fmt.Printf("\n3. THE LIVE ENTITY GATE ON CONSUMER TRAFFIC\n\n")
	dAllow := filter(danger, func(r row) bool { return r.GateAllow })
	rAllow := filter(reph, func(r row) bool { return r.GateAllow })
	fmt.Printf("   danger pairs the gate ALLOWS:    %2d of %2d  (%.0f%% pass straight through)\n",
		len(dAllow), len(danger), 100*float64(len(dAllow))/float64(len(danger)))
	fmt.Printf("   danger pairs the gate REFUSES:   %2d of %2d\n", len(danger)-len(dAllow), len(danger))
	fmt.Printf("   rephrasings the gate ALLOWS:     %2d of %2d\n", len(rAllow), len(reph))
	fmt.Printf("   rephrasings the gate REFUSES:    %2d of %2d  ← the honest-user cost\n\n",
		len(reph)-len(rAllow), len(reph))

	fmt.Printf("   DANGER PAIRS THE GATE DOES NOT SEE (sorted by similarity, worst first):\n")
	sort.Slice(dAllow, func(i, j int) bool { return dAllow[i].Sim > dAllow[j].Sim })
	for _, r := range dAllow {
		fmt.Printf("     %.4f  %-22s %q / %q\n", r.Sim, r.P.Name, r.P.A, r.P.B)
	}
	fmt.Printf("\n   DANGER PAIRS THE GATE CATCHES:\n")
	for _, r := range danger {
		if r.GateAllow {
			continue
		}
		fmt.Printf("     %.4f  %-22s  canon(A)=%q  canon(B)=%q\n", r.Sim, r.P.Name,
			discriminator.Canon(r.P.A), discriminator.Canon(r.P.B))
	}

	// ── 4. POST-GATE SEPARATION — the only table that decides whether pooling can be on.
	fmt.Print(poolsafety.SeparationTable(toScores(rAllow), toScores(dAllow)))

	// ── 5. THE PROTOTYPE. Measured, not asserted.
	fmt.Printf("\n5. PROTOTYPE CONSUMER ENTITY TIER (unmerged; measured to price the fix)\n\n")
	pdAllow := filter(danger, func(r row) bool { return r.ProtoAllow })
	prAllow := filter(reph, func(r row) bool { return r.ProtoAllow })
	fmt.Printf("   danger pairs still allowed:  %2d of %2d  (live gate: %d)\n", len(pdAllow), len(danger), len(dAllow))
	fmt.Printf("   rephrasings still allowed:   %2d of %2d  (live gate: %d)\n", len(prAllow), len(reph), len(rAllow))
	fmt.Printf("   rephrasings NEWLY refused by the prototype (pure cost to honest users):\n")
	n := 0
	for _, r := range reph {
		if r.GateAllow && !r.ProtoAllow {
			n++
			fmt.Printf("     %.4f  %-22s %q / %q\n", r.Sim, r.P.Name, r.P.A, r.P.B)
		}
	}
	if n == 0 {
		fmt.Printf("     (none)\n")
	}
	fmt.Printf("\n   danger pairs the prototype STILL misses:\n")
	sort.Slice(pdAllow, func(i, j int) bool { return pdAllow[i].Sim > pdAllow[j].Sim })
	for _, r := range pdAllow {
		fmt.Printf("     %.4f  %-22s %q / %q\n", r.Sim, r.P.Name, r.P.A, r.P.B)
	}
	fmt.Print(poolsafety.SeparationTable(toScores(prAllow), toScores(pdAllow)))
}

func section(title string, rs []row, th float64, danger bool) {
	asc := append([]row(nil), rs...)
	sort.Slice(asc, func(i, j int) bool { return asc[i].Sim < asc[j].Sim })
	pct := func(p float64) float64 { return asc[int(p*float64(len(asc)-1))].Sim }
	var over int
	for _, r := range rs {
		if r.Sim >= th {
			over++
		}
	}
	fmt.Printf("\n%s — %d pairs\n\n", title, len(rs))
	fmt.Printf("   at or above the production threshold %.2f: %d/%d\n", th, over, len(rs))
	fmt.Printf("   min %.4f · p25 %.4f · median %.4f · p75 %.4f · max %.4f\n\n",
		pct(0), pct(.25), pct(.50), pct(.75), pct(1))
	for i := len(asc) - 1; i >= 0; i-- {
		r := asc[i]
		g := "gate:ALLOW"
		if !r.GateAllow {
			g = "gate:refuse"
		}
		flag := ""
		if danger && r.Sim >= th && r.GateAllow {
			flag = "  ⚠⚠ WOULD POOL TODAY"
		}
		fmt.Printf("   %.4f  %-11s %-22s %-18s %q / %q%s\n", r.Sim, g, r.P.Name, r.Set, r.P.A, r.P.B, flag)
	}
}

func filter(rs []row, f func(row) bool) []row {
	var out []row
	for _, r := range rs {
		if f(r) {
			out = append(out, r)
		}
	}
	return out
}

func toScores(rs []row) []poolsafety.PairScore {
	out := make([]poolsafety.PairScore, 0, len(rs))
	for _, r := range rs {
		out = append(out, poolsafety.PairScore{Pair: r.P, Similarity: r.Sim})
	}
	return out
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
