package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/poolsafety"
)

// poolFlagOverride is what stops /v1/admin/economy/flags from lying. The endpoint reports
// CachePoolableEnabled off cfg, and cfg stays TRUE while the gate holds pooling off — so
// without this, the one place built to observe live flag state asserts the opposite of the
// truth about the single flag that can serve one tenant's answer to another.

type stubRow struct {
	err  error
	vals []any
}

func (r stubRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d = r.vals[i].(string)
		case *float64:
			*d = r.vals[i].(float64)
		}
	}
	return nil
}

type stubReader struct{ row stubRow }

func (s *stubReader) QueryRow(context.Context, string, ...any) poolsafety.Row { return s.row }

func TestPoolFlagOverride_CarriesTheGatesReason(t *testing.T) {
	cfg := &config.Config{CachePoolableEnabled: true, EmbeddingModel: "text-embedding-3-small", SemanticThreshold: 0.92}
	gate := poolsafety.NewGate()
	db := &stubReader{row: stubRow{err: poolsafety.ErrNoAttestation}}
	gate.Refresh(context.Background(), db, cfg.EmbeddingModel, cfg.SemanticThreshold)

	ovs := poolFlagOverride(cfg, gate)()
	if len(ovs) != 1 {
		t.Fatalf("expected one override for CachePoolableEnabled, got %d", len(ovs))
	}
	if ovs[0].Name != "CachePoolableEnabled" {
		t.Errorf("override names %q; it must match the flag econflags reports", ovs[0].Name)
	}
	if ovs[0].Effective {
		t.Fatal("the override reports pooling as effective while the gate is holding it off")
	}
	if !strings.Contains(ovs[0].Reason, "poolcheck") {
		t.Errorf("reason %q does not tell the operator what to run; \"pooling off\" with no "+
			"remedy is indistinguishable from pooling never having been switched on", ovs[0].Reason)
	}

	// Step 6b: the attestation appears. The override must follow WITHOUT a restart — it is
	// re-evaluated per request precisely so it can show recovery.
	db.row = stubRow{vals: []any{"text-embedding-3-small", 0.92, "p", 0.6534}}
	gate.Refresh(context.Background(), db, cfg.EmbeddingModel, cfg.SemanticThreshold)
	ovs = poolFlagOverride(cfg, gate)()
	if !ovs[0].Effective {
		t.Fatal("poolcheck recorded an attestation and the gate opened, but the flags endpoint " +
			"would still report pooling off")
	}
	if ovs[0].Reason != "" {
		t.Errorf("an attested gate still reports reason %q, which reads as a problem", ovs[0].Reason)
	}
}

// When pooling is off in config, config and behaviour AGREE — reporting an override would
// dress an ordinary "off" up as a runtime force-off and send operators chasing a non-problem.
func TestPoolFlagOverride_NoOverrideWhenPoolingIsOffInConfig(t *testing.T) {
	cfg := &config.Config{CachePoolableEnabled: false}
	if ovs := poolFlagOverride(cfg, poolsafety.NewGate())(); ovs != nil {
		t.Fatalf("reported an override for a flag that is simply off in config: %+v", ovs)
	}
}

// The gate must not be polled when pooling is disabled in config: there is nothing to attest,
// and a background query against an unused feature is pure cost.
func TestPoolSafetyGate_PoolingOffInConfig_IsClosedAndUnpolled(t *testing.T) {
	cfg := &config.Config{CachePoolableEnabled: false}
	gate := poolSafetyGate(context.Background(), nil, cfg) // nil pool: polling would panic
	if gate == nil {
		t.Fatal("nil gate")
	}
	if gate.Attested() {
		t.Fatal("pooling reported as attested while disabled in config")
	}
}

// Sanity on the sentinel translation: poolsafety must be able to tell "never measured" from
// "could not read", and it does that without importing pgx, so the adapter has to translate.
func TestNoRowsAsMissing_TranslatesToErrNoAttestation(t *testing.T) {
	if !errors.Is(poolsafety.ErrNoAttestation, poolsafety.ErrNoAttestation) {
		t.Fatal("sentinel is not comparable")
	}
	r := noRowsAsMissing{stubRow{err: errors.New("some other failure")}}
	if errors.Is(r.Scan(), poolsafety.ErrNoAttestation) {
		t.Error("an unrelated read failure was translated into \"no attestation\", which would make " +
			"a transient outage look conclusive and pin pooling off")
	}
}
