package mining

import (
	"context"
	"errors"
	"testing"
)

// Phase-0 Item C — the economy kill switch must stop the annotation mint (the SEC
// finding: the submit route is authed.Post/unconditional and neither it nor applyTx
// checked EconomyEnabled, so EconomyEnabled=false force-off'd every OTHER mint but
// MISSED annotation, the only live one).

// RED: with the economy gate OFF, SubmitAnnotation must refuse (ErrEconomyDisabled)
// BEFORE any DB access — so it cannot mint. The mock has NO expectations: a correct
// gate touches no DB. Before the fix, SubmitAnnotation proceeds toward the mint
// (Begin/query) and hits the mock as an unexpected call → not ErrEconomyDisabled.
func TestSubmitAnnotation_EconomyOff_RefusesBeforeMint(t *testing.T) {
	miner, mock := newMockAnnotator(t) // no expectations set
	miner.SetEconomyGate(func() bool { return false })

	err := miner.SubmitAnnotation(context.Background(), Annotation{
		TaskID: "t1", AnnotatorID: "ws_anno", Decision: "a_better", Confidence: 4,
	})
	if !errors.Is(err, ErrEconomyDisabled) {
		t.Fatalf("economy OFF must refuse the annotation submit with ErrEconomyDisabled BEFORE any DB/mint; got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("economy OFF must touch NO DB (no mint path reached): %v", err)
	}
}

// Control (regression): economy ON keeps the gate transparent — the submit proceeds
// to the DB exactly as before (it is NOT ErrEconomyDisabled). Passes before and after.
func TestSubmitAnnotation_EconomyOn_NotGated(t *testing.T) {
	miner, _ := newMockAnnotator(t) // no expectations → proceeding hits the mock
	miner.SetEconomyGate(func() bool { return true })

	err := miner.SubmitAnnotation(context.Background(), Annotation{
		TaskID: "t1", AnnotatorID: "ws_anno", Decision: "a_better", Confidence: 4,
	})
	if errors.Is(err, ErrEconomyDisabled) {
		t.Fatalf("economy ON must NOT gate the submit; got ErrEconomyDisabled")
	}
}
