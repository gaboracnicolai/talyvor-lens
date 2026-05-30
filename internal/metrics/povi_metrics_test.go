package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The verified label is a bounded {true,false} set; a verify failure also bumps
// the dedicated failures counter.
func TestPOVIReceipt_BoundedLabelAndFailures(t *testing.T) {
	failBefore := testutil.ToFloat64(POVIReceiptVerifyFailuresTotal)

	POVIReceipt(true)
	POVIReceipt(false)

	if v := testutil.ToFloat64(POVIReceiptsTotal.WithLabelValues("true")); v < 1 {
		t.Errorf("verified=true counter = %v, want >=1", v)
	}
	if v := testutil.ToFloat64(POVIReceiptsTotal.WithLabelValues("false")); v < 1 {
		t.Errorf("verified=false counter = %v, want >=1", v)
	}
	if got := testutil.ToFloat64(POVIReceiptVerifyFailuresTotal); got != failBefore+1 {
		t.Errorf("verify-failures = %v, want %v (one failure)", got, failBefore+1)
	}
}

func TestPOVIProvisionalMint(t *testing.T) {
	before := testutil.ToFloat64(POVIProvisionalMintsTotal)
	POVIProvisionalMint()
	if got := testutil.ToFloat64(POVIProvisionalMintsTotal); got != before+1 {
		t.Errorf("provisional-mints = %v, want %v", got, before+1)
	}
}

// Staking metrics are unlabeled (trivially cardinality-safe); confirm the
// helpers set/increment them.
func TestPOVIStakingMetrics(t *testing.T) {
	SetPOVIStakeLocked(250.0)
	if v := testutil.ToFloat64(POVIStakeLockedTotal); v != 250.0 {
		t.Errorf("stake-locked gauge = %v, want 250", v)
	}
	SetPOVINodesStaked(3)
	if v := testutil.ToFloat64(POVINodesStakedGauge); v != 3 {
		t.Errorf("nodes-staked gauge = %v, want 3", v)
	}
	sBefore := testutil.ToFloat64(POVISlashesTotal)
	aBefore := testutil.ToFloat64(POVISlashAmountTotal)
	POVISlash(12.5)
	if got := testutil.ToFloat64(POVISlashesTotal); got != sBefore+1 {
		t.Errorf("slashes = %v, want %v", got, sBefore+1)
	}
	if got := testutil.ToFloat64(POVISlashAmountTotal); got != aBefore+12.5 {
		t.Errorf("slash-amount = %v, want %v", got, aBefore+12.5)
	}
}
