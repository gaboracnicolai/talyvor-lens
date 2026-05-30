package stats

import (
	"math"
	"testing"
)

// These tests pin the statistical math to KNOWN, hand-computable / textbook
// values. The whole point of the eval pipeline's significance work is honesty,
// so the math itself must be verified against references, not just "looks
// plausible".

const eps = 1e-3

func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.6f, want %.6f (±%g)", name, got, want, tol)
	}
}

// NormalCDF against standard-normal table values.
func TestNormalCDF_KnownValues(t *testing.T) {
	approx(t, "Phi(0)", NormalCDF(0), 0.5, 1e-9)
	approx(t, "Phi(1.959964)", NormalCDF(1.959964), 0.975, eps)
	approx(t, "Phi(-1.959964)", NormalCDF(-1.959964), 0.025, eps)
	approx(t, "Phi(2.5067)", NormalCDF(2.5067), 0.99391, eps)
}

// Student's two-sided p-value via the regularized incomplete beta. The 97.5th
// percentile of t with df=8 is 2.306, so a |t| of 2.306 must give p≈0.05.
func TestStudentTwoSidedP_KnownValues(t *testing.T) {
	approx(t, "p(t=2.306, df=8)", StudentTwoSidedP(2.306, 8), 0.05, eps)
	approx(t, "p(t=0, df=8)", StudentTwoSidedP(0, 8), 1.0, 1e-9)
	approx(t, "p(t=1.0, df=8)", StudentTwoSidedP(1.0, 8), 0.3466, eps)
}

// Mann-Whitney U on two perfectly-separated groups of 5. Textbook: U1=0,
// U2=25, and with continuity-corrected normal approximation p≈0.0122.
func TestMannWhitneyU_SeparatedGroups(t *testing.T) {
	a := []float64{1, 2, 3, 4, 5}
	b := []float64{6, 7, 8, 9, 10}
	r := MannWhitneyU(a, b)

	if r.U1 != 0 || r.U2 != 25 {
		t.Fatalf("U1=%.1f U2=%.1f, want 0 and 25", r.U1, r.U2)
	}
	approx(t, "p", r.P, 0.0122, eps)
	// a is entirely below b → rank-biserial = -1.
	approx(t, "rank-biserial", r.RankBiserial, -1.0, 1e-9)
}

// Tie handling: identical samples → U at the mean, p=1 (no difference).
func TestMannWhitneyU_IdenticalSamples(t *testing.T) {
	a := []float64{0.5, 0.6, 0.7, 0.8, 0.9}
	b := []float64{0.5, 0.6, 0.7, 0.8, 0.9}
	r := MannWhitneyU(a, b)
	approx(t, "U1", r.U1, 12.5, 1e-9) // n1*n2/2
	approx(t, "p", r.P, 1.0, eps)
	approx(t, "rank-biserial", r.RankBiserial, 0.0, 1e-9)
}

// Welch's t on a=[1..5], b=[2..6]: means 3 and 4, equal sample variance 2.5,
// so t=-1.0 exactly and Welch–Satterthwaite df=8.
func TestWelchT_KnownValue(t *testing.T) {
	a := []float64{1, 2, 3, 4, 5}
	b := []float64{2, 3, 4, 5, 6}
	r := WelchT(a, b)
	approx(t, "t", r.T, -1.0, 1e-9)
	approx(t, "df", r.DF, 8.0, 1e-9)
	approx(t, "p", r.P, 0.3466, eps)
}

// Compare: a clear, large-effect difference with adequate n is SIGNIFICANT.
func TestCompare_LargeEffectIsSignificant(t *testing.T) {
	// 60 high-quality vs 60 low-quality samples — unambiguous.
	a := make([]float64, 60)
	b := make([]float64, 60)
	for i := range a {
		a[i] = 0.9
		b[i] = 0.3
	}
	v := Compare(a, b, 0.05)
	if !v.Significant {
		t.Fatalf("large effect must be significant, got %+v", v)
	}
	if v.Direction != 1 {
		t.Errorf("direction = %d, want +1 (a>b)", v.Direction)
	}
	if v.PValue >= 0.05 {
		t.Errorf("p = %.4f, want < 0.05", v.PValue)
	}
}

// Compare: a tiny sample must NEVER declare a winner — it's inconclusive even
// if the point difference looks large. This is the core honesty guarantee.
func TestCompare_TinySampleIsInconclusive(t *testing.T) {
	a := []float64{0.9, 0.95}
	b := []float64{0.3, 0.35}
	v := Compare(a, b, 0.05)
	if v.Significant {
		t.Fatalf("tiny sample (n=2) must be inconclusive, got significant: %+v", v)
	}
	if !containsFold(v.Summary, "not enough data") {
		t.Errorf("summary should explain insufficient data, got %q", v.Summary)
	}
}

// Compare: a real but small/noisy difference that isn't significant must report
// "not significant", NOT a winner — the n=40/p=0.21 case from the spec.
func TestCompare_NoSignificantDifference(t *testing.T) {
	// Two overlapping spreads, ~equal — should not reach significance.
	a := []float64{0.5, 0.6, 0.55, 0.52, 0.58, 0.51, 0.49, 0.57, 0.53, 0.54}
	b := []float64{0.51, 0.59, 0.54, 0.53, 0.57, 0.50, 0.52, 0.56, 0.55, 0.53}
	v := Compare(a, b, 0.05)
	if v.Significant {
		t.Fatalf("near-identical samples must not be significant: %+v", v)
	}
	if v.PValue < 0.05 {
		t.Errorf("p = %.4f, want >= 0.05", v.PValue)
	}
}

func containsFold(s, sub string) bool {
	return len(s) >= len(sub) && (indexFold(s, sub) >= 0)
}

func indexFold(s, sub string) int {
	ls, lsub := len(s), len(sub)
	for i := 0; i+lsub <= ls; i++ {
		match := true
		for j := 0; j < lsub; j++ {
			cs, cj := s[i+j], sub[j]
			if 'A' <= cs && cs <= 'Z' {
				cs += 'a' - 'A'
			}
			if 'A' <= cj && cj <= 'Z' {
				cj += 'a' - 'A'
			}
			if cs != cj {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
