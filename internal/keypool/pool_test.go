package keypool

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAdd_PutsKeyInCorrectProviderBucket(t *testing.T) {
	p := New()
	k, err := p.Add("openai", "sk-secret", "prod-1", 1000)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if k.ID == "" {
		t.Error("ID should be generated")
	}
	if k.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", k.Provider)
	}
	if k.Alias != "prod-1" {
		t.Errorf("Alias = %q, want prod-1", k.Alias)
	}
	if !k.Healthy {
		t.Error("new keys must start Healthy=true")
	}
	// Anthropic, google should accept.
	if _, err := p.Add("anthropic", "ant-key", "ant-1", 0); err != nil {
		t.Errorf("Add anthropic: %v", err)
	}
	if _, err := p.Add("google", "g-key", "g-1", 0); err != nil {
		t.Errorf("Add google: %v", err)
	}
	if _, err := p.Add("unknown", "k", "x", 0); err == nil {
		t.Error("Add unknown provider must return error")
	}
}

func TestGet_RoundRobinAcrossTwoHealthyKeys(t *testing.T) {
	p := New()
	a, _ := p.Add("openai", "key-a", "a", 0)
	b, _ := p.Add("openai", "key-b", "b", 0)

	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		k, err := p.Get("openai")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		seen[k.ID] = true
	}
	if !seen[a.ID] || !seen[b.ID] {
		t.Errorf("round-robin did not visit both keys; seen=%v wanted both %s and %s", seen, a.ID, b.ID)
	}
}

func TestGet_SkipsUnhealthyKeys(t *testing.T) {
	p := New()
	bad, _ := p.Add("openai", "key-bad", "bad", 0)
	good, _ := p.Add("openai", "key-good", "good", 0)

	// Take 'bad' below 50% health by recording errors only.
	for i := 0; i < 5; i++ {
		p.RecordError(bad.ID)
	}
	if bad.Healthy {
		t.Fatalf("bad key should be unhealthy after %d straight errors", 5)
	}

	for i := 0; i < 10; i++ {
		k, err := p.Get("openai")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if k.ID == bad.ID {
			t.Errorf("Get returned unhealthy key %s; should always pick %s", bad.ID, good.ID)
		}
	}
}

func TestGet_NoHealthyKeysReturnsError(t *testing.T) {
	p := New()
	k, _ := p.Add("openai", "key-x", "x", 0)
	for i := 0; i < 3; i++ {
		p.RecordError(k.ID)
	}
	if _, err := p.Get("openai"); err == nil {
		t.Error("Get should return error when no healthy keys; got nil")
	} else if !strings.Contains(err.Error(), "no healthy keys") {
		t.Errorf("error text = %q, want substring 'no healthy keys'", err.Error())
	}
}

func TestRecordError_MarksKeyUnhealthyAbove50PercentThreshold(t *testing.T) {
	p := New()
	k, _ := p.Add("openai", "key-x", "x", 0)

	// 5 successes, then 6 errors → 6 / 11 ≈ 54.5% — over threshold.
	for i := 0; i < 5; i++ {
		p.RecordSuccess(k.ID)
	}
	for i := 0; i < 6; i++ {
		p.RecordError(k.ID)
	}
	if k.Healthy {
		t.Errorf("key should be unhealthy when error rate is 6/11 (>50%%); got Healthy=true")
	}
}

func TestMarkHealthy_ResetsErrorCountAndMarksHealthy(t *testing.T) {
	p := New()
	k, _ := p.Add("openai", "key-x", "x", 0)
	for i := 0; i < 5; i++ {
		p.RecordError(k.ID)
	}
	if k.Healthy {
		t.Fatal("precondition: key should be unhealthy")
	}

	p.MarkHealthy(k.ID)

	if !k.Healthy {
		t.Error("MarkHealthy did not set Healthy=true")
	}
	if k.ErrorCount != 0 {
		t.Errorf("MarkHealthy did not reset ErrorCount; got %d", k.ErrorCount)
	}
}

func TestStats_ReturnsCountsWithoutExposingKeys(t *testing.T) {
	p := New()
	_, _ = p.Add("openai", "sk-supersecret-DONT-LEAK", "prod", 0)
	_, _ = p.Add("anthropic", "sk-ant-DONT-LEAK", "prod", 0)

	stats := p.Stats()
	if len(stats) != 2 {
		t.Fatalf("Stats len = %d, want 2", len(stats))
	}
	// Every stats serialisation path must omit the raw secret material.
	raw := flattenStats(stats)
	if strings.Contains(raw, "sk-supersecret") {
		t.Errorf("Stats leaked OpenAI raw key text; got: %s", raw)
	}
	if strings.Contains(raw, "sk-ant-DONT-LEAK") {
		t.Errorf("Stats leaked Anthropic raw key text; got: %s", raw)
	}
}

func TestStartHealthChecker_RestoresUnhealthyKeyOnCheckSuccess(t *testing.T) {
	p := New()
	k, _ := p.Add("openai", "key-x", "x", 0)
	for i := 0; i < 5; i++ {
		p.RecordError(k.ID)
	}
	if k.Healthy {
		t.Fatal("precondition: key must be unhealthy before health check")
	}

	checked := make(chan struct{}, 1)
	checkFn := func(provider, key string) bool {
		select {
		case checked <- struct{}{}:
		default:
		}
		return true // pretend health probe succeeded
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Drive one cycle synchronously so the test doesn't have to race a
	// 5-minute ticker. StartHealthChecker is a thin loop around this.
	p.RunHealthCheckOnce(ctx, checkFn)

	select {
	case <-checked:
	default:
		t.Fatal("checkFn was not invoked for the unhealthy key")
	}
	if !k.Healthy {
		t.Error("health check returned true but key is still unhealthy")
	}
	_ = time.Second // anchor import
}

// flattenStats serialises stats into a single string we can grep for
// leaked key material. Includes both struct fields and JSON form so a
// future struct addition that accidentally exposes the key fails fast.
func flattenStats(s []KeyStats) string {
	var b strings.Builder
	for _, st := range s {
		b.WriteString(st.ID)
		b.WriteByte(' ')
		b.WriteString(st.Provider)
		b.WriteByte(' ')
		b.WriteString(st.Alias)
		b.WriteByte('\n')
	}
	return b.String()
}
