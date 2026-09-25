package alerts

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/auth"
)

// B9.3 — every token_events row records the credential that made the request (migration 0129), on
// both writers: a live serve (RecordSpend) and a cache serve (RecordCacheServe). No credential = ”.
func TestTokenEvents_RecordTheCredentialThatMadeTheRequest(t *testing.T) {
	pool := cacheServePool(t)
	m := New(pool, nil, nil)
	chat := auth.WithAuthContext(context.Background(), &auth.AuthContext{WorkspaceID: "ws-am", AuthMethod: auth.MethodSessionKey})

	if err := m.RecordSpend(chat, "ws-am", "", "", "", "gpt-4o", 10, 5, "", "", "req-am-live", "text", false); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordCacheServe(chat, "ws-am", "", "", "", "gpt-4o", 10, 5, "", "req-am-pooled", "text", "cache_hit_pooled"); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordSpend(context.Background(), "ws-am", "", "", "", "gpt-4o", 10, 5, "", "", "req-am-none", "text", false); err != nil {
		t.Fatal(err)
	}
	for req, want := range map[string]string{"req-am-live": "session_key", "req-am-pooled": "session_key", "req-am-none": ""} {
		var got string
		if err := pool.QueryRow(context.Background(), `SELECT auth_method FROM token_events WHERE request_id = $1`, req).Scan(&got); err != nil {
			t.Fatalf("%s: %v", req, err)
		}
		if got != want {
			t.Errorf("%s: auth_method = %q, want %q", req, got, want)
		}
	}
}
