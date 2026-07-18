package outputverify_test

// content_capture_integration_test.go — migration 0098 + the capture seam: output_content_sha256 is recorded
// alongside response_sha256 (post-flush), NULL when the served body has no committable content. The identity
// columns are untouched — that invariant has its own pinned-value proof in internal/proxy.

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/outputverify"
)

// A record carrying a content hash lands it in the column; the identity columns hold exactly what was given.
func TestRecord_StoresOutputContentSHA256(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	w := outputverify.NewWriter(pool)

	r := rec("oid-content-1", "wsC", outputverify.VerdictUnverifiable, "", outputverify.KindNone)
	contentSHA, ok := outputverify.CanonicalContentSHA256("anthropic",
		[]byte(`{"content":[{"type":"text","text":"package main\nfunc main(){}"}]}`))
	if !ok {
		t.Fatal("vector must canonicalize")
	}
	r.OutputContentSHA256 = contentSHA
	if ins, err := w.Record(ctx, r); err != nil || !ins {
		t.Fatalf("Record ins=%v err=%v", ins, err)
	}

	var gotContent *string
	var gotResponse string
	if err := pool.QueryRow(ctx,
		`SELECT output_content_sha256, response_sha256 FROM k4_output_verdicts WHERE output_id=$1`, r.OutputID).
		Scan(&gotContent, &gotResponse); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotContent == nil || *gotContent != contentSHA {
		t.Fatalf("output_content_sha256 = %v, want %q", gotContent, contentSHA)
	}
	if gotResponse != r.ResponseSHA256 {
		t.Fatalf("response_sha256 = %q, want %q (identity column must hold the envelope hash untouched)", gotResponse, r.ResponseSHA256)
	}
}

// No committable content (extraction failed / streaming / empty) → the column is NULL, not "".
func TestRecord_NoContent_IsNULL(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	w := outputverify.NewWriter(pool)

	r := rec("oid-content-null-1", "wsC", outputverify.VerdictUnverifiable, "", outputverify.KindNone)
	// OutputContentSHA256 left "" — the not-committable representation.
	if ins, err := w.Record(ctx, r); err != nil || !ins {
		t.Fatalf("Record ins=%v err=%v", ins, err)
	}
	var gotContent *string
	if err := pool.QueryRow(ctx,
		`SELECT output_content_sha256 FROM k4_output_verdicts WHERE output_id=$1`, r.OutputID).Scan(&gotContent); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if gotContent != nil {
		t.Fatalf("output_content_sha256 must be NULL when no content binds; got %q", *gotContent)
	}
}
