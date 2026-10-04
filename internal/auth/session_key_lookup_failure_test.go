package auth

// B27.5 — a database hiccup is not a sign-out. Before this, AuthMiddleware answered 401 for ANY
// session-key Validate error, a failed lookup included, and the browser Chat then told a signed-in
// person "This session is no longer signed in".

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSessionKey_LookupFailureIs503AndTheWrongKeysStay401(t *testing.T) {
	pool := skAuthPool(t)
	m, store := skAuthManager(t, pool)
	ctx := context.Background()

	good, _, err := store.Mint(ctx, "ws-b275", "user-b275", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	expired, _, err := store.Mint(ctx, "ws-b275", "user-b275-exp", -time.Minute)
	if err != nil {
		t.Fatalf("Mint expired: %v", err)
	}
	revoked, _, err := store.Mint(ctx, "ws-b275", "user-b275-rev", time.Hour)
	if err != nil {
		t.Fatalf("Mint revoked: %v", err)
	}
	if _, err := store.RevokeAll(ctx, "ws-b275", "user-b275-rev"); err != nil {
		t.Fatalf("RevokeAll: %v", err)
	}
	unknown := "tlv_sk_" + "00112233445566778899aabbccddeeff0011223344556677"

	h := AuthMiddleware(New(pool), m)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	serve := func(raw string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, skRequest(raw))
		return rec
	}

	if rec := serve(good); rec.Code != http.StatusOK {
		t.Fatalf("the good key must work before the lookup breaks or this proves nothing: %d %s",
			rec.Code, rec.Body.String())
	}
	for name, raw := range map[string]string{"unknown": unknown, "expired": expired, "revoked": revoked} {
		if rec := serve(raw); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s session key answered %d, want 401 — only a failed lookup may become a 503",
				name, rec.Code)
		}
	}

	// The lookup fails: the table the query reads is gone, a real database error and not a miss.
	if _, err := pool.Exec(ctx, `ALTER TABLE session_keys RENAME TO session_keys_away`); err != nil {
		t.Fatalf("break the lookup: %v", err)
	}
	rec := serve(good)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a failed session lookup answered %d, want 503 — a 401 here signs the person out "+
			"of Chat for a database hiccup (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != AuthUnavailableRetryAfter {
		t.Fatalf("Retry-After = %q, want %q", got, AuthUnavailableRetryAfter)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["code"] != authUnavailableCode {
		t.Fatalf("body = %s, want code %q — the Chat keys its quiet retry on it", rec.Body.String(),
			authUnavailableCode)
	}

	// And it was a hiccup: once the database answers again, the same key works.
	if _, err := pool.Exec(ctx, `ALTER TABLE session_keys_away RENAME TO session_keys`); err != nil {
		t.Fatalf("restore the lookup: %v", err)
	}
	if rec := serve(good); rec.Code != http.StatusOK {
		t.Fatalf("after the hiccup the same key answered %d, want 200", rec.Code)
	}
}
