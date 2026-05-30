// Package stats provides pure, dependency-free statistical-significance
// functions for comparing two samples of quality scores. It has NO I/O, no
// state, and no internal dependencies — numbers in, verdict out — so it is
// directly unit-testable against textbook-known p-values and is safe to import
// from both internal/ab and internal/eval without import cycles.
//
// The PRIMARY test is the Mann-Whitney U (Wilcoxon rank-sum): a non-parametric
// rank test that makes no normality assumption. Quality scores are bounded to
// [0,1] and are often non-normal/bimodal (a cluster of good responses near 1.0
// and a tail of poor ones), which is exactly where a t-test's normality
// assumption is unreliable — so the rank test is the honest default. Welch's
// t-test is provided as a parametric secondary for cross-checking.
//
// HONESTY RULE: Compare never declares a winner from a tiny sample or a
// non-significant difference. "Inconclusive" is a first-class outcome.
package stats

import (
	"fmt"
	"math"
	"sort"
)

// MinSamplesPerGroup is the floor below which Compare refuses to render a
// verdict at all: the normal approximation to the U distribution is unreliable
// for tiny n, and — more importantly — declaring a winner from a handful of
// samples is exactly the lie this package exists to prevent.
const MinSamplesPerGroup = 5

// MWResult is the outcome of a Mann-Whitney U test.
type MWResult struct {
	NA, NB       int
	U1, U2       float64 // U for sample A and sample B (U1+U2 = NA*NB)
	P            float64 // two-sided p-value (continuity-corrected normal approx)
	RankBiserial float64 // effect size in [-1,1]; >0 means A tends higher than B
}

// TResult is the outcome of Welch's two-sample t-test (unequal variances).
type TResult struct {
	NA, NB  int
	T       float64 // t statistic (sign: >0 means mean(A) > mean(B))
	DF      float64 // Welch–Satterthwaite degrees of freedom
	P       float64 // two-sided p-value
	CohensD float64 // effect size (pooled-SD standardized mean difference)
}

// Verdict is the plain-language significance call for an A/B quality
// comparison. Significant is true ONLY when the primary (Mann-Whitney) test
// clears alpha AND both groups have at least MinSamplesPerGroup samples.
type Verdict struct {
	Primary     string  `json:"primary"` // "mann-whitney-u"
	NA          int     `json:"n_a"`
	NB          int     `json:"n_b"`
	UStatistic  float64 `json:"u_statistic"`
	PValue      float64 `json:"p_value"`
	EffectSize  float64 `json:"effect_size"` // rank-biserial
	Alpha       float64 `json:"alpha"`
	Significant bool    `json:"significant"`
	Direction   int     `json:"direction"` // +1 A>B, -1 A<B, 0 none/inconclusive
	Welch       TResult `json:"welch"`     // parametric secondary
	Summary     string  `json:"summary"`   // human-readable, names "inconclusive" when so
}

// NormalCDF is the standard-normal cumulative distribution Φ(z).
func NormalCDF(z float64) float64 { return 0.5 * math.Erfc(-z/math.Sqrt2) }

// mean and sampleVar are the usual unbiased estimators.
func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func sampleVar(xs []float64, m float64) float64 {
	n := len(xs)
	if n < 2 {
		return 0
	}
	var s float64
	for _, x := range xs {
		d := x - m
		s += d * d
	}
	return s / float64(n-1)
}

// MannWhitneyU computes the two-sided Mann-Whitney U test for samples a and b
// using average ranks for ties, a continuity-corrected normal approximation
// with tie correction for the p-value, and the rank-biserial effect size.
func MannWhitneyU(a, b []float64) MWResult {
	n1, n2 := len(a), len(b)
	res := MWResult{NA: n1, NB: n2}
	if n1 == 0 || n2 == 0 {
		res.P = 1
		return res
	}

	// Pool and assign average ranks (1-based), tracking tie-group sizes.
	type item struct {
		v float64
		g int // 0 = a, 1 = b
	}
	pooled := make([]item, 0, n1+n2)
	for _, v := range a {
		pooled = append(pooled, item{v, 0})
	}
	for _, v := range b {
		pooled = append(pooled, item{v, 1})
	}
	sort.SliceStable(pooled, func(i, j int) bool { return pooled[i].v < pooled[j].v })

	ranks := make([]float64, len(pooled))
	var tieSum float64 // Σ(t³ - t) over tie groups, for variance correction
	for i := 0; i < len(pooled); {
		j := i
		for j < len(pooled) && pooled[j].v == pooled[i].v {
			j++
		}
		// Tie group is [i, j): ranks i+1..j → average rank.
		avg := (float64(i+1) + float64(j)) / 2
		for k := i; k < j; k++ {
			ranks[k] = avg
		}
		tg := float64(j - i)
		tieSum += tg*tg*tg - tg
		i = j
	}

	var r1 float64
	for i, it := range pooled {
		if it.g == 0 {
			r1 += ranks[i]
		}
	}

	N1, N2 := float64(n1), float64(n2)
	u1 := r1 - N1*(N1+1)/2
	u2 := N1*N2 - u1
	res.U1, res.U2 = u1, u2
	// Rank-biserial: (U1 - U2)/(N1*N2) ∈ [-1,1]; >0 ⇒ A tends higher.
	res.RankBiserial = (u1 - u2) / (N1 * N2)

	N := N1 + N2
	muU := N1 * N2 / 2
	// Variance with tie correction.
	varU := (N1 * N2 / 12) * ((N + 1) - tieSum/(N*(N-1)))
	if varU <= 0 {
		res.P = 1
		return res
	}
	sd := math.Sqrt(varU)
	// Continuity correction: pull |U1-μ| toward the mean by 0.5.
	z := (math.Abs(u1-muU) - 0.5)
	if z < 0 {
		z = 0
	}
	z /= sd
	res.P = 2 * (1 - NormalCDF(z))
	if res.P > 1 {
		res.P = 1
	}
	return res
}

// WelchT computes Welch's two-sample t-test (unequal variances) with the
// Welch–Satterthwaite degrees of freedom and a two-sided p-value, plus Cohen's
// d as a parametric effect size.
func WelchT(a, b []float64) TResult {
	n1, n2 := len(a), len(b)
	res := TResult{NA: n1, NB: n2, P: 1}
	if n1 < 2 || n2 < 2 {
		return res
	}
	m1, m2 := mean(a), mean(b)
	v1, v2 := sampleVar(a, m1), sampleVar(b, m2)
	N1, N2 := float64(n1), float64(n2)

	se2 := v1/N1 + v2/N2
	if se2 <= 0 {
		// Zero variance: identical-within-group. Equal means ⇒ no difference.
		if m1 == m2 {
			res.T, res.P = 0, 1
		}
		return res
	}
	res.T = (m1 - m2) / math.Sqrt(se2)
	// Welch–Satterthwaite df.
	num := se2 * se2
	den := (v1/N1)*(v1/N1)/(N1-1) + (v2/N2)*(v2/N2)/(N2-1)
	res.DF = num / den
	res.P = StudentTwoSidedP(res.T, res.DF)

	// Cohen's d with pooled SD.
	pooledVar := ((N1-1)*v1 + (N2-1)*v2) / (N1 + N2 - 2)
	if pooledVar > 0 {
		res.CohensD = (m1 - m2) / math.Sqrt(pooledVar)
	}
	return res
}

// StudentTwoSidedP returns the two-sided p-value P(|T| > |t|) for a Student's
// t distribution with df degrees of freedom, via the regularized incomplete
// beta function.
func StudentTwoSidedP(t, df float64) float64 {
	if df <= 0 {
		return 1
	}
	x := df / (df + t*t)
	return betai(df/2, 0.5, x)
}

// Compare runs the primary (Mann-Whitney) and secondary (Welch) tests and
// renders an honest verdict. It declares significance ONLY when the primary
// p-value < alpha AND both groups have ≥ MinSamplesPerGroup samples. Below that
// floor the verdict is "inconclusive — not enough data", regardless of how
// large the point difference looks.
func Compare(a, b []float64, alpha float64) Verdict {
	mw := MannWhitneyU(a, b)
	welch := WelchT(a, b)

	v := Verdict{
		Primary:    "mann-whitney-u",
		NA:         mw.NA,
		NB:         mw.NB,
		UStatistic: math.Min(mw.U1, mw.U2),
		PValue:     mw.P,
		EffectSize: mw.RankBiserial,
		Alpha:      alpha,
		Welch:      welch,
	}

	if mw.RankBiserial > 0 {
		v.Direction = 1
	} else if mw.RankBiserial < 0 {
		v.Direction = -1
	}

	if mw.NA < MinSamplesPerGroup || mw.NB < MinSamplesPerGroup {
		v.Significant = false
		v.Direction = 0
		v.Summary = fmt.Sprintf(
			"inconclusive — not enough data (n=%d vs %d; need ≥%d per variant)",
			mw.NA, mw.NB, MinSamplesPerGroup)
		return v
	}

	v.Significant = mw.P < alpha
	conf := int(math.Round((1 - alpha) * 100))
	if v.Significant {
		dir := "A scores higher than B"
		if v.Direction < 0 {
			dir = "B scores higher than A"
		}
		v.Summary = fmt.Sprintf(
			"significant at %d%% — %s (Mann-Whitney U, n=%d vs %d, p=%.4f, effect=%.2f)",
			conf, dir, mw.NA, mw.NB, mw.P, mw.RankBiserial)
	} else {
		v.Direction = 0
		v.Summary = fmt.Sprintf(
			"not significant — inconclusive (Mann-Whitney U, n=%d vs %d, p=%.4f); need more data",
			mw.NA, mw.NB, mw.P)
	}
	return v
}

// ── regularized incomplete beta (Numerical Recipes betai/betacf) ──

func betai(aa, bb, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	lbeta := lgamma(aa+bb) - lgamma(aa) - lgamma(bb)
	front := math.Exp(lbeta + aa*math.Log(x) + bb*math.Log(1-x))
	if x < (aa+1)/(aa+bb+2) {
		return front * betacf(aa, bb, x) / aa
	}
	return 1 - front*betacf(bb, aa, 1-x)/bb
}

func betacf(aa, bb, x float64) float64 {
	const (
		maxIter = 200
		epsilon = 3e-14
		fpmin   = 1e-300
	)
	qab := aa + bb
	qap := aa + 1
	qam := aa - 1
	c := 1.0
	d := 1 - qab*x/qap
	if math.Abs(d) < fpmin {
		d = fpmin
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIter; m++ {
		mf := float64(m)
		m2 := 2 * mf
		aterm := mf * (bb - mf) * x / ((qam + m2) * (aa + m2))
		d = 1 + aterm*d
		if math.Abs(d) < fpmin {
			d = fpmin
		}
		c = 1 + aterm/c
		if math.Abs(c) < fpmin {
			c = fpmin
		}
		d = 1 / d
		h *= d * c
		aterm = -(aa + mf) * (qab + mf) * x / ((aa + m2) * (qap + m2))
		d = 1 + aterm*d
		if math.Abs(d) < fpmin {
			d = fpmin
		}
		c = 1 + aterm/c
		if math.Abs(c) < fpmin {
			c = fpmin
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < epsilon {
			break
		}
	}
	return h
}

func lgamma(x float64) float64 {
	v, _ := math.Lgamma(x)
	return v
}
