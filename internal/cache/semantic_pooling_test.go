package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// SetPooled upserts a shared-pool row tagged with the contributor workspace and
// is_poolable=true (the latter a SQL literal, so not an arg).
func TestSemanticCache_SetPooled_TagsContributor(t *testing.T) {
	c, mock := newTestSemanticCache(t, stubEmbedder{}, 0.9)

	sum := sha256.Sum256([]byte("openai:gpt-4:pooledprompt"))
	wantHash := hex.EncodeToString(sum[:])

	mock.ExpectExec(`INSERT INTO prompt_embeddings`).
		WithArgs("openai", "gpt-4", wantHash, pgxmock.AnyArg(), "resp", "wsA").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := c.SetPooled(context.Background(), "openai", "gpt-4", "pooledprompt", "wsA", []byte("resp"), []float32{0.1, 0.2}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// GetPooled filters is_poolable=true and returns the response + contributor on a
// hit above threshold.
func TestSemanticCache_GetPooled_FiltersAndReturnsContributor(t *testing.T) {
	c, mock := newTestSemanticCache(t, stubEmbedder{vec: []float32{0.1, 0.2, 0.3}}, 0.9)

	const id = "11111111-1111-1111-1111-111111111111"
	mock.ExpectQuery(`is_poolable = true`).
		WithArgs(pgxmock.AnyArg(), "openai", "gpt-4").
		WillReturnRows(
			pgxmock.NewRows([]string{"id", "response", "contributor", "similarity"}).
				AddRow(id, "pooled_payload", "wsA", 0.95),
		)
	mock.ExpectExec(`UPDATE prompt_embeddings`).
		WithArgs(id).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	body, owner, err := c.GetPooled(context.Background(), "openai", "gpt-4", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "pooled_payload" || owner != "wsA" {
		t.Fatalf("got (%q, %q), want (pooled_payload, wsA)", body, owner)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// Below the similarity threshold → miss (nil, "", nil), no touch.
func TestSemanticCache_GetPooled_BelowThreshold(t *testing.T) {
	c, mock := newTestSemanticCache(t, stubEmbedder{vec: []float32{0.1}}, 0.9)
	mock.ExpectQuery(`is_poolable = true`).
		WithArgs(pgxmock.AnyArg(), "openai", "gpt-4").
		WillReturnRows(
			pgxmock.NewRows([]string{"id", "response", "contributor", "similarity"}).
				AddRow("id1", "x", "wsA", 0.5),
		)
	body, owner, err := c.GetPooled(context.Background(), "openai", "gpt-4", "hello")
	if err != nil || body != nil || owner != "" {
		t.Fatalf("below threshold must miss; got (%q,%q,%v)", body, owner, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// A pooled row with an empty/missing contributor (defensive) returns owner=""
// so the gate blocks it.
func TestSemanticCache_GetPooled_EmptyContributor(t *testing.T) {
	c, mock := newTestSemanticCache(t, stubEmbedder{vec: []float32{0.1}}, 0.9)
	mock.ExpectQuery(`is_poolable = true`).
		WithArgs(pgxmock.AnyArg(), "openai", "gpt-4").
		WillReturnRows(
			pgxmock.NewRows([]string{"id", "response", "contributor", "similarity"}).
				AddRow("id1", "x", "", 0.99),
		)
	mock.ExpectExec(`UPDATE prompt_embeddings`).WithArgs("id1").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	body, owner, err := c.GetPooled(context.Background(), "openai", "gpt-4", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "" {
		t.Fatalf("missing contributor must surface as empty owner (→ gate blocks); got %q", owner)
	}
	_ = body
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// The PRIVATE Get must exclude pooled rows (is_poolable = false in its WHERE),
// so a private lookup can never serve a pooled entry and bypass consent.
func TestSemanticCache_PrivateGet_ExcludesPoolable(t *testing.T) {
	c, mock := newTestSemanticCache(t, stubEmbedder{vec: []float32{0.1}}, 0.9)
	// The regex asserts the private SELECT carries the is_poolable=false filter.
	mock.ExpectQuery(`is_poolable = false`).
		WithArgs(pgxmock.AnyArg(), "openai", "gpt-4").
		WillReturnRows(pgxmock.NewRows([]string{"id", "response", "similarity"}))
	if _, err := c.Get(context.Background(), "openai", "gpt-4", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("private Get must filter is_poolable=false: %v", err)
	}
}
