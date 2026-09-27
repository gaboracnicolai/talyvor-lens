// Command pairverify measures B9.2's pair verifier over the committed poolsafety corpora.
//
// Every pair is checked RUNS times. A danger pair answered YES in ANY run counts as served — a
// verifier that is right on average still serves a wrong answer on the run it gets wrong. A
// rephrasing counts as recovered only when EVERY run says YES.
//
// Two populations are reported:
//
//	verifier alone      — every pair, as if the verifier were the only gate after retrieval.
//	after live gates    — only pairs production would already admit (similarity >= the live
//	                      threshold AND equal discriminators), i.e. the verifier as a SECOND gate.
//	                      Needs LENS_OPENAI_API_KEY for the embeddings; skipped without it.
//	wired gate (B9.7)   — the gate as it serves: cache.PooledCandidateAt decides which pairs reach
//	                      the verifier (entity gate at the threshold; with no entity on either side,
//	                      similarity >= a bound), the verifier decides. Swept over candidate bounds.
//
// And the economics: mean cost per check, the cost of the answer a hit saves (a real generation on
// the same model over a sample of the corpus), and the break-even hit rate — the share of checked
// candidates that must end in a serve for the checks to pay for themselves.
//
// It changes nothing and recommends nothing.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/canonq"
	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/discriminator"
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/pairverify"
	"github.com/talyvor/lens/internal/poolsafety"
)

const (
	runs     = 3
	workers  = 8
	attempts = 3
)

type result struct {
	pair     poolsafety.RephrasePair
	yes      int
	measured int
	raws     []string
	in, out  int
	admitted bool // passes the live threshold AND the entity gate (only when embeddings ran)
	sim      float64
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pairverify:", err)
		os.Exit(1)
	}
}

func run() error {
	key := os.Getenv("LENS_ANTHROPIC_API_KEY")
	if key == "" {
		return fmt.Errorf("no LENS_ANTHROPIC_API_KEY — nothing can be measured")
	}
	v := pairverify.NewAnthropicVerifier(key, os.Getenv("LENS_PAIRVERIFY_MODEL"))
	model := v.Model
	threshold := config.DefaultSemanticThreshold
	if v, err := strconv.ParseFloat(os.Getenv("LENS_SEMANTIC_THRESHOLD"), 64); err == nil {
		threshold = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	var emb poolsafety.Embedder
	if ok := os.Getenv("LENS_OPENAI_API_KEY"); ok != "" {
		em := os.Getenv("LENS_EMBEDDING_MODEL")
		if em == "" {
			em = "text-embedding-3-small"
		}
		emb = embedder.NewOpenAIEmbedder(ok, em, os.Getenv("LENS_EMBEDDING_BASE_URL"))
	}

	fmt.Printf("verifier model: %s  temperature 0  %d runs per pair  prompt: pairverify.Prompt\n", model, runs)
	fmt.Printf("live gates:     similarity >= %.2f AND discriminators equal", threshold)
	if emb == nil {
		fmt.Printf("  (NOT MEASURED: no LENS_OPENAI_API_KEY)")
	}
	fmt.Println()

	dangerServed := 0
	unmeasured := 0
	for _, ln := range poolsafety.ByTraffic() {
		reph, err := check(ctx, v, emb, threshold, ln.Rephrase)
		if err != nil {
			return err
		}
		dang, err := check(ctx, v, emb, threshold, ln.Danger)
		if err != nil {
			return err
		}
		ds, un := report(ln.Traffic, reph, dang, emb != nil, threshold)
		dangerServed += ds
		unmeasured += un
	}

	economics(ctx, key, model)

	fmt.Printf("\n══════════ VERDICT ══════════\n")
	switch {
	case dangerServed > 0:
		fmt.Printf("  SERVES %d DANGER PAIR(S) in at least one of %d runs — it does NOT go on the serve path.\n", dangerServed, runs)
	case unmeasured > 0:
		fmt.Printf("  %d pair check(s) never returned — the danger count is incomplete, so this is NOT a pass.\n", unmeasured)
	default:
		fmt.Printf("  0 danger pairs served across %d runs of every pair.\n", runs)
	}
	return nil
}

// check runs every pair `runs` times through the verifier, and scores similarity + entity gate.
func check(ctx context.Context, v pairverify.Verifier, emb poolsafety.Embedder, threshold float64, pairs []poolsafety.RephrasePair) ([]result, error) {
	out := make([]result, len(pairs))
	for i, p := range pairs {
		out[i].pair = p
	}
	if emb != nil {
		scored, err := poolsafety.ScorePairs(ctx, emb, pairs)
		if err != nil {
			return nil, err
		}
		byName := map[string]int{}
		for i, p := range pairs {
			byName[p.Name] = i
		}
		for _, s := range scored {
			i := byName[s.Pair.Name]
			out[i].sim = s.Similarity
			out[i].admitted = s.Similarity >= threshold && discriminator.Match(s.Pair.A, s.Pair.B)
		}
	}
	type job struct{ i, run int }
	jobs := make(chan job)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				p := out[j.i].pair
				var got pairverify.Verdict
				var err error
				for a := 0; a < attempts; a++ {
					if got, err = v.Verify(ctx, p.A, p.B); err == nil {
						break
					}
					time.Sleep(time.Duration(a+1) * 2 * time.Second)
				}
				mu.Lock()
				if err == nil {
					out[j.i].measured++
					out[j.i].in += got.InTokens
					out[j.i].out += got.OutTokens
					out[j.i].raws = append(out[j.i].raws, strings.TrimSpace(got.Raw))
					if got.Same {
						out[j.i].yes++
					}
				}
				mu.Unlock()
			}
		}()
	}
	for i := range pairs {
		for r := 0; r < runs; r++ {
			jobs <- job{i, r}
		}
	}
	close(jobs)
	wg.Wait()
	return out, nil
}

// bounds are the no-entity candidate bounds the wired gate is measured at; cache.NoEntityLowerBound
// is the one that serves.
var bounds = []float64{0.60, 0.65, 0.70, 0.75, 0.80, 0.85, 0.90}

func report(lane string, reph, dang []result, gated bool, threshold float64) (dangerServed, unmeasured int) {
	fmt.Printf("\n═══ %s ═══  %d rephrase pairs (should serve) · %d danger pairs (must not)\n", lane, len(reph), len(dang))
	recovered, unstable := 0, 0
	for _, r := range reph {
		switch {
		case r.measured == runs && r.yes == runs:
			recovered++
		case r.yes > 0:
			unstable++
		}
		if r.measured < runs {
			unmeasured++
		}
	}
	var served []result
	for _, r := range dang {
		if r.yes > 0 {
			served = append(served, r)
		}
		if r.measured < runs {
			unmeasured++
		}
	}
	fmt.Printf("  verifier alone:   rephrasings recovered (YES %d/%d runs) %s · YES in some runs only %d\n",
		runs, runs, frac(recovered, len(reph)), unstable)
	fmt.Printf("                    danger pairs served (YES in ANY run)  %s\n", frac(len(served), len(dang)))
	for _, r := range served {
		fmt.Printf("    ⚠ SERVED %-22s YES %d/%d  A=%q  B=%q  replies=%q\n", r.pair.Name, r.yes, r.measured, r.pair.A, r.pair.B, r.raws)
	}
	if gated {
		ar, ad, rr, dd := 0, 0, 0, 0
		for _, r := range reph {
			if r.admitted {
				ar++
				if r.measured == runs && r.yes == runs {
					rr++
				}
			}
		}
		for _, r := range dang {
			if r.admitted {
				ad++
				if r.yes > 0 {
					dd++
				}
			}
		}
		fmt.Printf("  after live gates: candidates admitted today — rephrase %d, danger %d; with the verifier as a second gate: rephrase served %d, danger served %d\n", ar, ad, rr, dd)
		wired(reph, dang, threshold)
	}
	sort.Slice(reph, func(i, j int) bool { return reph[i].pair.Name < reph[j].pair.Name })
	var refused []string
	for _, r := range reph {
		if r.yes == 0 {
			refused = append(refused, r.pair.Name)
		}
	}
	fmt.Printf("  rephrasings refused in every run: %s\n", strings.Join(refused, ", "))
	return len(served), unmeasured
}

// wired reports the B9.7 gate as it serves, at each candidate bound: how many pairs reach the
// verifier (each a paid check), how many rephrasings it serves (YES in every run) and how many
// danger pairs (YES in any run). served/checked is what the break-even hit rate is compared with.
func wired(reph, dang []result, threshold float64) {
	fmt.Printf("  wired gate (B9.7): entity lane at %.2f, no-entity lane at the bound, verifier decides\n", threshold)
	fmt.Printf("    %-7s %-18s %-18s %-14s %s\n", "bound", "rephrase served", "danger served", "checks", "served/checks")
	for _, b := range bounds {
		rc, rs, dc, ds := 0, 0, 0, 0
		for _, r := range reph {
			if cache.PooledCandidateAt(r.pair.A, r.pair.B, r.sim, threshold, b) {
				rc++
				if r.measured == runs && r.yes == runs {
					rs++
				}
			}
		}
		for _, r := range dang {
			if cache.PooledCandidateAt(r.pair.A, r.pair.B, r.sim, threshold, b) {
				dc++
				if r.yes > 0 {
					ds++
				}
			}
		}
		live := ""
		if b == cache.NoEntityLowerBound {
			live = "  ← live"
		}
		share := "n/a"
		if rc+dc > 0 {
			share = fmt.Sprintf("%.0f%%", 100*float64(rs)/float64(rc+dc))
		}
		fmt.Printf("    %-7.2f %-18s %-18s %-14d %s%s\n", b, frac(rs, len(reph)), frac(ds, len(dang)), rc+dc, share, live)
	}
	var near []string
	for _, r := range reph {
		if !discriminator.Canon(r.pair.B).Verifiable() && !discriminator.Canon(r.pair.A).Verifiable() {
			near = append(near, fmt.Sprintf("%s %.4f", r.pair.Name, r.sim))
		}
	}
	sort.Strings(near)
	fmt.Printf("    entity-free rephrasings and their similarity: %s\n", strings.Join(near, ", "))
}

// economics prices one check against the answer a hit saves, both on the same model.
func economics(ctx context.Context, key, model string) {
	fmt.Printf("\n══════════ COST ══════════\n")
	v := pairverify.NewAnthropicVerifier(key, model)
	gen := canonq.NewAnthropicCanonicaliser(key, model)
	var cin, cout, cn, gin, gout, gn int
	for _, ln := range poolsafety.ByTraffic() {
		sample := ln.Rephrase
		if len(sample) > 5 {
			sample = sample[:5]
		}
		for _, p := range sample {
			if r, err := v.Verify(ctx, p.A, p.B); err == nil {
				cin, cout, cn = cin+r.InTokens, cout+r.OutTokens, cn+1
			}
			if r, err := gen.Answer(ctx, p.A); err == nil {
				gin, gout, gn = gin+r.InTokens, gout+r.OutTokens, gn+1
			}
		}
	}
	if cn == 0 || gn == 0 {
		fmt.Println("  not measured: no successful check or generation")
		return
	}
	check := alerts.CostUSD(model, cin/cn, cout/cn)
	answer := alerts.CostUSD(model, gin/gn, gout/gn)
	fmt.Printf("  per check:  mean %d in + %d out tokens = $%.7f  (%d calls)\n", cin/cn, cout/cn, check, cn)
	fmt.Printf("  per answer: mean %d in + %d out tokens = $%.7f  (%d real generations on %s)\n", gin/gn, gout/gn, answer, gn, model)
	if check == 0 || answer == 0 {
		fmt.Printf("  ⚠ the catalog prices %s at $0 — the ratio below is meaningless\n", model)
		return
	}
	fmt.Printf("  break-even: %.1f%% of checked candidates must end in a serve for the checks to pay for themselves\n", 100*check/answer)
	fmt.Printf("  (the answer is priced on %s; a pool serving a larger model saves more per hit, so this is the conservative case)\n", model)
}

func frac(n, d int) string {
	if d == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d %.0f%%", n, d, 100*float64(n)/float64(d))
}
