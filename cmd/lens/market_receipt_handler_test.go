package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/market"
)

type fakeReceipts struct{ r market.Receipt }

func (f fakeReceipts) Receipt(_ context.Context, ws, id string) (market.Receipt, error) {
	if ws != f.r.BuyerWorkspaceID || id != f.r.ID {
		return market.Receipt{}, market.ErrNoReceipt
	}
	return f.r, nil
}

func (f fakeReceipts) ReceiptsOf(context.Context, string) ([]market.ReceiptSummary, error) {
	return []market.ReceiptSummary{{ID: f.r.ID, Number: f.r.Number}}, nil
}

// B32.40 — a receipt is read as JSON, as its page (?format=html) and as its PDF (Accept: application/pdf); another
// workspace's receipt, or one that does not exist, is 404.
func TestMarketReceiptRoutes_AnswerJSONHTMLAndPDF(t *testing.T) {
	r := chi.NewRouter()
	rc := market.Receipt{ID: "rcpt_1", Number: "2026-000001", BuyerWorkspaceID: "ws-1", InvoiceID: "in_1", IssuedAt: time.Now(), PaidAt: time.Now(),
		Supplier: market.Supplier{LegalName: "TALYVOR LTD"}, Lines: []market.ReceiptLine{{Description: "Adder — rental", NetUSDMicros: 10_000_000,
			RateBps: 2000, TaxUSDMicros: 2_000_000}}, NetUSDMicros: 10_000_000, TaxUSDMicros: 2_000_000, GrossUSDMicros: 12_000_000}
	mountMarketReceiptRoutes(r, fakeReceipts{rc})
	get := func(path, accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	const one = "/v1/workspaces/ws-1/marketplace/receipts/rcpt_1"
	if w := get(one, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"number":"2026-000001"`) {
		t.Fatalf("JSON = %d %s", w.Code, w.Body.String())
	}
	if w := get(one+"?format=html", ""); w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(w.Body.String(), "$12.00") {
		t.Fatalf("HTML = %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w := get(one, "application/pdf"); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/pdf" ||
		!bytes.HasPrefix(w.Body.Bytes(), []byte("%PDF-")) {
		t.Fatalf("PDF = %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w := get("/v1/workspaces/ws-2/marketplace/receipts/rcpt_1", ""); w.Code != http.StatusNotFound {
		t.Fatalf("another workspace's receipt = %d; want 404", w.Code)
	}
	if w := get("/v1/workspaces/ws-1/marketplace/receipts", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "rcpt_1") {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
}
