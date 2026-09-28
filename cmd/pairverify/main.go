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
//	names and numbers (B21.1) — poolsafety.EntityRephrasePairs/EntityDangerPairs: questions on the
//	                      entity lane, whose candidate bound is swept like the no-entity one.
//	conversations (B16.1) — the multi-turn traps (poolsafety.ConversationDanger/Rephrase) as both
//	                      semantic reads serve them: cache.LatestTurn splits each chat body, and
//	                      cache.ConversationCandidate (the same history, then the rule above on the
//	                      two latest questions) decides which reach the verifier.
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
	// PAIRVERIFY_CONVERSATIONS_ONLY=1 measures only the multi-turn sections (B16.1, B16.2).
	onlyConversations := os.Getenv("PAIRVERIFY_CONVERSATIONS_ONLY") == "1"
	lanes := append(poolsafety.ByTraffic(), poolsafety.TrafficLanes{Traffic: "NAMES AND NUMBERS (B21.1)",
		Rephrase: poolsafety.EntityRephrasePairs(), Danger: poolsafety.EntityDangerPairs()})
	for _, ln := range lanes {
		if onlyConversations {
			break
		}
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

	ds, un, err := conversations(ctx, v, emb, threshold)
	if err != nil {
		return err
	}
	dangerServed += ds
	unmeasured += un

	if !onlyConversations {
		economics(ctx, key, model)
	}

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
	return checkWith(ctx, emb, threshold, pairs, func(ctx context.Context, i int) (pairverify.Verdict, error) {
		return v.Verify(ctx, pairs[i].A, pairs[i].B)
	})
}

// checkWith is check with the verifier call supplied per pair — B16.2's in-context check needs each
// pair's own history.
func checkWith(ctx context.Context, emb poolsafety.Embedder, threshold float64, pairs []poolsafety.RephrasePair,
	verify func(ctx context.Context, i int) (pairverify.Verdict, error)) ([]result, error) {
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
				var got pairverify.Verdict
				var err error
				for a := 0; a < attempts; a++ {
					if got, err = verify(ctx, j.i); err == nil {
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

// bounds are the candidate bounds the wired gate is measured at, for each lane; cache.NoEntityLowerBound
// and cache.EntityLowerBound are the ones that serve.
var bounds = []float64{0.60, 0.65, 0.70, 0.75, 0.80, 0.85, 0.90, 0.95}

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

// wired reports the gate as it serves, at each candidate bound of one lane with the other lane at its
// live bound: how many pairs reach the verifier (each a paid check), how many rephrasings it serves
// (YES in every run) and how many danger pairs (YES in any run). served/checked is what the
// break-even hit rate is compared with.
func wired(reph, dang []result, threshold float64) {
	liveEntity := cache.EntityLowerBound
	if threshold < liveEntity {
		liveEntity = threshold
	}
	sweep := func(title string, at func(b float64) (entity, noEntity float64), live float64) {
		fmt.Printf("  wired gate — %s, verifier decides\n", title)
		fmt.Printf("    %-7s %-18s %-18s %-14s %s\n", "bound", "rephrase served", "danger served", "checks", "served/checks")
		for _, b := range bounds {
			e, n := at(b)
			rc, rs, dc, ds := 0, 0, 0, 0
			for _, r := range reph {
				if cache.PooledCandidateAt(r.pair.A, r.pair.B, r.sim, e, n) {
					rc++
					if r.measured == runs && r.yes == runs {
						rs++
					}
				}
			}
			for _, r := range dang {
				if cache.PooledCandidateAt(r.pair.A, r.pair.B, r.sim, e, n) {
					dc++
					if r.yes > 0 {
						ds++
					}
				}
			}
			mark := ""
			if b == live {
				mark = "  ← live"
			}
			share := "n/a"
			if rc+dc > 0 {
				share = fmt.Sprintf("%.0f%%", 100*float64(rs)/float64(rc+dc))
			}
			fmt.Printf("    %-7.2f %-18s %-18s %-14d %s%s\n", b, frac(rs, len(reph)), frac(ds, len(dang)), rc+dc, share, mark)
		}
	}
	sweep(fmt.Sprintf("no-entity lane at the bound, entity lane at %.2f (B9.7)", liveEntity),
		func(b float64) (float64, float64) { return liveEntity, b }, cache.NoEntityLowerBound)
	sweep(fmt.Sprintf("entity lane at the bound, no-entity lane at %.2f (B21.1)", cache.NoEntityLowerBound),
		func(b float64) (float64, float64) { return b, cache.NoEntityLowerBound }, cache.EntityLowerBound)
	var entity []string
	for _, r := range reph {
		if discriminator.Canon(r.pair.A).Verifiable() {
			entity = append(entity, fmt.Sprintf("%s %.4f", r.pair.Name, r.sim))
		}
	}
	sort.Strings(entity)
	fmt.Printf("    rephrasings that name something, and their similarity: %s\n", strings.Join(entity, ", "))
	var near []string
	for _, r := range reph {
		if !discriminator.Canon(r.pair.B).Verifiable() && !discriminator.Canon(r.pair.A).Verifiable() {
			near = append(near, fmt.Sprintf("%s %.4f", r.pair.Name, r.sim))
		}
	}
	sort.Strings(near)
	fmt.Printf("    entity-free rephrasings and their similarity: %s\n", strings.Join(near, ", "))
}

// conversations measures the multi-turn corpus. Each pair is split exactly as the proxy splits a
// request (cache.LatestTurn on the chat body); the verifier and the embedder see the two LATEST
// questions, as they do when serving. The private and the pooled read apply the same rule — they
// differ in whose rows they range over, not in the gate — so one measurement is both paths'.
func conversations(ctx context.Context, v pairverify.Verifier, emb poolsafety.Embedder, threshold float64) (dangerServed, unmeasured int, err error) {
	split := func(cs []poolsafety.ConversationPair) ([]poolsafety.RephrasePair, [][2]cache.Turn) {
		pairs := make([]poolsafety.RephrasePair, len(cs))
		turns := make([][2]cache.Turn, len(cs))
		for i, c := range cs {
			turns[i] = [2]cache.Turn{cache.LatestTurn(poolsafety.ChatBody(c.Stored)), cache.LatestTurn(poolsafety.ChatBody(c.Asked))}
			pairs[i] = poolsafety.RephrasePair{Name: c.Name, A: turns[i][0].Latest, B: turns[i][1].Latest}
		}
		return pairs, turns
	}
	dp, dt := split(poolsafety.ConversationDanger)
	rp, rt := split(poolsafety.ConversationRephrase)
	dang, err := check(ctx, v, emb, threshold, dp)
	if err != nil {
		return 0, 0, err
	}
	reph, err := check(ctx, v, emb, threshold, rp)
	if err != nil {
		return 0, 0, err
	}
	fmt.Printf("\n═══ CONVERSATIONS (B16.1) ═══  %d rephrase pairs (should serve) · %d danger pairs (must not)\n", len(reph), len(dang))
	fmt.Printf("  private and pooled reads: same history (prefix hash), then entity lane at %.2f / no-entity lane at %.2f, verifier decides\n",
		min(threshold, cache.EntityLowerBound), cache.NoEntityLowerBound)
	fmt.Printf("    %-34s %-8s %-7s %-9s %-8s %s\n", "pair", "history", "sim", "candidate", "YES", "served")
	row := func(r result, t [2]cache.Turn, danger bool) bool {
		if r.measured < runs {
			unmeasured++
		}
		cand := emb != nil && cache.ConversationCandidate(t[0], t[1], r.sim, threshold)
		served := cand && r.yes > 0
		if !danger {
			served = cand && r.measured == runs && r.yes == runs
		}
		history := "same"
		if t[0].Prefix != t[1].Prefix {
			history = "differs"
		}
		mark := ""
		if danger && served {
			mark = "  ⚠ SERVED"
		}
		fmt.Printf("    %-34s %-8s %-7.4f %-9v %d/%-6d %v%s\n", r.pair.Name, history, r.sim, cand, r.yes, r.measured, served, mark)
		return served
	}
	servedReph, aloneDanger := 0, 0
	fmt.Println("  danger:")
	for i, r := range dang {
		if row(r, dt[i], true) {
			dangerServed++
		}
		if r.yes > 0 {
			aloneDanger++
		}
	}
	fmt.Println("  rephrase:")
	for i, r := range reph {
		if row(r, rt[i], false) {
			servedReph++
		}
	}
	if emb == nil {
		fmt.Println("  NOT MEASURED: no LENS_OPENAI_API_KEY, so no similarity — the gate cannot be scored")
		unmeasured += len(dang)
	}
	fmt.Printf("  verifier alone on the latest questions: danger YES in some run %s\n", frac(aloneDanger, len(dang)))
	fmt.Printf("  as served: rephrasings served %s · danger served %s\n", frac(servedReph, len(reph)), frac(dangerServed, len(dang)))

	ds, un, err := standalone(ctx, v, emb, threshold)
	return dangerServed + ds, unmeasured + un, err
}

// standalone measures B16.2's lane: a stored single-turn question against the same or similar words
// asked mid-conversation, gated by cache.StandaloneCandidate and decided by the in-context verifier,
// which is shown the asked conversation. The danger set includes every B16.1 danger pair re-cast for
// this lane, so every context-dependent follow-up there is tried against a standalone answer.
func standalone(ctx context.Context, v pairverify.Verifier, emb poolsafety.Embedder, threshold float64) (dangerServed, unmeasured int, err error) {
	cv, ok := v.(pairverify.ContextVerifier)
	if !ok {
		return 0, 0, fmt.Errorf("the verifier has no in-context check")
	}
	measure := func(cs []poolsafety.ConversationPair) ([]result, [][2]cache.Turn, error) {
		pairs := make([]poolsafety.RephrasePair, len(cs))
		turns := make([][2]cache.Turn, len(cs))
		for i, c := range cs {
			turns[i] = [2]cache.Turn{cache.LatestTurn(poolsafety.ChatBody(c.Stored)), cache.LatestTurn(poolsafety.ChatBody(c.Asked))}
			pairs[i] = poolsafety.RephrasePair{Name: c.Name, A: turns[i][0].Latest, B: turns[i][1].Latest}
		}
		// One serve attempt is both calls, as cache.standsAlone makes them: the pair check, then the
		// stands-alone check on the asked conversation. Raw keeps both replies.
		res, err := checkWith(ctx, emb, threshold, pairs, func(ctx context.Context, i int) (pairverify.Verdict, error) {
			p, err := v.Verify(ctx, pairs[i].A, pairs[i].B)
			if err != nil {
				return p, err
			}
			a, err := cv.StandsAlone(ctx, turns[i][1].History, pairs[i].B)
			if err != nil {
				return a, err
			}
			return pairverify.Verdict{Same: p.Same && a.Same, Raw: strings.TrimSpace(p.Raw) + "/" + strings.TrimSpace(a.Raw),
				InTokens: p.InTokens + a.InTokens, OutTokens: p.OutTokens + a.OutTokens}, nil
		})
		return res, turns, err
	}
	dang, dt, err := measure(poolsafety.StandaloneDanger())
	if err != nil {
		return 0, 0, err
	}
	reph, rt, err := measure(poolsafety.StandaloneRephrase)
	if err != nil {
		return 0, 0, err
	}
	fmt.Printf("\n═══ STANDALONE ANSWER MID-CONVERSATION (B16.2) ═══  %d rephrase pairs (should serve) · %d danger pairs (must not)\n", len(reph), len(dang))
	fmt.Printf("  a single-turn row, the same rule on the two questions; served on the pair check's YES AND the stands-alone check's (pairverify.StandalonePrompt)\n")
	fmt.Printf("    %-44s %-7s %-9s %-8s %-8s %s\n", "pair", "sim", "candidate", "YES", "served", "replies (pair/stands-alone)")
	row := func(r result, t [2]cache.Turn, danger bool) bool {
		if r.measured < runs {
			unmeasured++
		}
		cand := emb != nil && cache.StandaloneCandidate(t[0], t[1], r.sim, threshold)
		served := cand && r.yes > 0
		if !danger {
			served = cand && r.measured == runs && r.yes == runs
		}
		mark := ""
		if danger && served {
			mark = "  ⚠ SERVED"
		}
		fmt.Printf("    %-44s %-7.4f %-9v %d/%-6d %-8v %s%s\n", r.pair.Name, r.sim, cand, r.yes, r.measured, served, strings.Join(r.raws, " "), mark)
		return served
	}
	servedReph := 0
	fmt.Println("  danger:")
	for i, r := range dang {
		if row(r, dt[i], true) {
			dangerServed++
		}
	}
	fmt.Println("  rephrase:")
	for i, r := range reph {
		if row(r, rt[i], false) {
			servedReph++
		}
	}
	if emb == nil {
		unmeasured += len(dang)
	}
	fmt.Printf("  as served: rephrasings served %s · danger served %s\n", frac(servedReph, len(reph)), frac(dangerServed, len(dang)))
	return dangerServed, unmeasured, nil
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
