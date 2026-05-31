package tenant

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// helper to wire a Store onto a pgxmock connection (mock impl
// satisfies our pgxDB interface).
func newMockStore(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("create mock pool: %v", err)
	}
	return newStore(mock), mock
}

// ─── 1) key generation ──────────────────────────────

func TestGenerateKey_HasPrefixAndLength(t *testing.T) {
	raw, prefix, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if !strings.HasPrefix(raw, KeyPrefix) {
		t.Fatalf("expected raw to start with %q, got %q", KeyPrefix, raw)
	}
	if len(raw) != len(KeyPrefix)+keyHexLen {
		t.Fatalf("unexpected raw length: %d", len(raw))
	}
	if len(prefix) != PrefixLookupLen {
		t.Fatalf("unexpected prefix length: %d", len(prefix))
	}
	if !strings.HasPrefix(prefix, KeyPrefix) {
		t.Fatalf("expected prefix to start with %q, got %q", KeyPrefix, prefix)
	}
}

func TestGenerateKey_IsRandom(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		raw, _, err := GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		if seen[raw] {
			t.Fatalf("duplicate key generated: %q", raw)
		}
		seen[raw] = true
	}
}

// ─── 2) scope validation ────────────────────────────

func TestValidateScopes(t *testing.T) {
	cases := []struct {
		name    string
		scopes  []string
		wantErr bool
	}{
		{"valid_proxy", []string{"proxy"}, false},
		{"valid_all", []string{"proxy", "analytics", "admin"}, false},
		{"empty", []string{}, false},
		{"invalid_single", []string{"root"}, true},
		{"invalid_mixed", []string{"proxy", "billing"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateScopes(tc.scopes)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrInvalidScope) {
				t.Fatalf("expected ErrInvalidScope, got %v", err)
			}
		})
	}
}

// ─── 3) create + validate round-trip (mock-DB) ──────

func TestCreateAndValidateAPIKey_RoundTrip(t *testing.T) {
	store, mock := newMockStore(t)
	defer mock.Close()

	ctx := context.Background()
	workspaceID := "ws_round_trip"

	mock.ExpectQuery("INSERT INTO workspace_api_keys").
		WithArgs(workspaceID, pgxmock.AnyArg(), pgxmock.AnyArg(),
			"primary", []string{"proxy"}, pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).
			AddRow("key_123", time.Now()))

	raw, meta, err := store.CreateAPIKey(ctx, workspaceID, "primary", []string{"proxy"}, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if meta.ID != "key_123" {
		t.Fatalf("expected id key_123, got %q", meta.ID)
	}
	if !strings.HasPrefix(raw, KeyPrefix) {
		t.Fatalf("raw missing prefix: %q", raw)
	}

	// Now validate. We have to seed pgxmock with the row the
	// real DB would return — including the bcrypt hash that was
	// computed inside CreateAPIKey. The hash is on `meta.KeyHash`.
	mock.ExpectQuery("SELECT id, workspace_id, key_hash").
		WithArgs(raw[:PrefixLookupLen]).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "key_hash", "key_prefix",
			"name", "scopes", "last_used_at", "expires_at", "created_at",
		}).AddRow(
			meta.ID, workspaceID, meta.KeyHash, meta.KeyPrefix,
			meta.Name, meta.Scopes, (*time.Time)(nil), (*time.Time)(nil), meta.CreatedAt,
		))
	mock.ExpectExec("UPDATE workspace_api_keys SET last_used_at").
		WithArgs(meta.ID).WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	got, err := store.ValidateAPIKey(ctx, raw)
	if err != nil {
		t.Fatalf("ValidateAPIKey: %v", err)
	}
	if got.WorkspaceID != workspaceID {
		t.Fatalf("expected workspace %q, got %q", workspaceID, got.WorkspaceID)
	}
	if got.LastUsedAt == nil {
		t.Fatal("expected LastUsedAt to be set after touch")
	}
}

// ─── 4) validate: wrong key rejected ────────────────

func TestValidateAPIKey_WrongKeyRejected(t *testing.T) {
	store, mock := newMockStore(t)
	defer mock.Close()

	ctx := context.Background()
	// craft a key + matching prefix
	raw, _, _ := GenerateKey()
	// pretend the DB has a row whose hash was computed from a
	// *different* raw key
	other, _, _ := GenerateKey()
	store2, _ := newMockStore(t)
	_ = store2 // unused: we only need a hash, generate via Store
	_, otherMeta, err := store.CreateAPIKey(ctx, "ws_x", "n", []string{"proxy"}, nil)
	// We expect the CreateAPIKey call above to have used the mock —
	// drain it. Re-init the mock to keep things tidy.
	if err != nil {
		// We swallow the error because pgxmock will report
		// missing INSERT expectations; let's redo with proper exp.
	}
	// Re-init clean.
	store, mock = newMockStore(t)
	defer mock.Close()

	mock.ExpectQuery("INSERT INTO workspace_api_keys").
		WithArgs("ws_x", pgxmock.AnyArg(), pgxmock.AnyArg(),
			"n", []string{"proxy"}, pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).
			AddRow("k1", time.Now()))
	_, otherMeta, err = store.CreateAPIKey(ctx, "ws_x", "n", []string{"proxy"}, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	// lookup by the *raw* prefix returns the otherMeta hash
	mock.ExpectQuery("SELECT id, workspace_id, key_hash").
		WithArgs(raw[:PrefixLookupLen]).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "key_hash", "key_prefix",
			"name", "scopes", "last_used_at", "expires_at", "created_at",
		}).AddRow(
			otherMeta.ID, "ws_x", otherMeta.KeyHash, raw[:PrefixLookupLen],
			"n", []string{"proxy"}, (*time.Time)(nil), (*time.Time)(nil), time.Now(),
		))

	_, err = store.ValidateAPIKey(ctx, raw)
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("expected ErrInvalidKey, got %v", err)
	}
	_ = other
}

// ─── 5) validate: expired key ───────────────────────

func TestValidateAPIKey_Expired(t *testing.T) {
	store, mock := newMockStore(t)
	defer mock.Close()

	ctx := context.Background()
	past := time.Now().Add(-time.Hour)

	mock.ExpectQuery("INSERT INTO workspace_api_keys").
		WithArgs("ws_y", pgxmock.AnyArg(), pgxmock.AnyArg(),
			"old", []string{"proxy"}, pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).AddRow("k_old", time.Now()))

	raw, meta, err := store.CreateAPIKey(ctx, "ws_y", "old", []string{"proxy"}, &past)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	mock.ExpectQuery("SELECT id, workspace_id, key_hash").
		WithArgs(raw[:PrefixLookupLen]).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "key_hash", "key_prefix",
			"name", "scopes", "last_used_at", "expires_at", "created_at",
		}).AddRow(
			meta.ID, "ws_y", meta.KeyHash, meta.KeyPrefix,
			"old", []string{"proxy"}, (*time.Time)(nil), &past, time.Now(),
		))

	_, err = store.ValidateAPIKey(ctx, raw)
	if !errors.Is(err, ErrKeyExpired) {
		t.Fatalf("expected ErrKeyExpired, got %v", err)
	}
}

// ─── 6) validate: malformed input ───────────────────

func TestValidateAPIKey_Malformed(t *testing.T) {
	store, mock := newMockStore(t)
	defer mock.Close()
	_, err := store.ValidateAPIKey(context.Background(), "nope")
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("expected ErrInvalidKey, got %v", err)
	}
}

// ─── 7) CheckAllowed: model + provider matrix ───────

func TestCheckAllowed(t *testing.T) {
	type tc struct {
		name     string
		cfg      *WorkspaceConfig
		model    string
		provider string
		wantErr  error
	}
	cases := []tc{
		{"nil_cfg", nil, "x", "y", nil},
		{"both_empty", &WorkspaceConfig{}, "x", "y", nil},
		{
			"model_allowed",
			&WorkspaceConfig{AllowedModels: []string{"claude-haiku-4-5"}},
			"claude-haiku-4-5", "anthropic", nil,
		},
		{
			"model_blocked",
			&WorkspaceConfig{AllowedModels: []string{"claude-haiku-4-5"}},
			"gpt-4o", "openai", ErrModelNotAllowed,
		},
		{
			"provider_blocked",
			&WorkspaceConfig{AllowedProviders: []string{"anthropic"}},
			"any", "openai", ErrProviderNotAllowed,
		},
		{
			"both_filters_pass",
			&WorkspaceConfig{
				AllowedProviders: []string{"anthropic"},
				AllowedModels:    []string{"claude-opus-4-7"},
			},
			"claude-opus-4-7", "anthropic", nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckAllowed(c.cfg, c.model, c.provider)
			if c.wantErr == nil && err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Fatalf("expected %v, got %v", c.wantErr, err)
			}
		})
	}
}

// ─── SpendTracker: cached current-month spend (cache TTL) ───
// (The cap-enforcement method CheckCap was removed as dead code —
// the live spend gate is budgets.Service.CheckBudget. This keeps the
// cache-TTL coverage via the live CurrentSpend path.)

func TestSpendTracker_CurrentSpend_CachesWithinTTL(t *testing.T) {
	store, mock := newMockStore(t)
	defer mock.Close()

	ctx := context.Background()
	tracker := NewSpendTracker(store)

	// CurrentSpend does NOT read config; two calls within the TTL must
	// hit Postgres exactly ONCE (the second is served from cache).
	mock.ExpectQuery("SELECT COALESCE\\(SUM\\(cost_usd\\), 0\\)").
		WithArgs("ws_cache").
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(5.0))

	for i := 0; i < 2; i++ {
		got, err := tracker.CurrentSpend(ctx, "ws_cache")
		if err != nil {
			t.Fatalf("CurrentSpend[%d]: %v", i, err)
		}
		if got != 5.0 {
			t.Fatalf("CurrentSpend[%d] = %v, want 5.0", i, got)
		}
	}
	// Exactly one SUM query expected — proves the second call was cached.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations (cache should serve the 2nd call): %v", err)
	}
}

// ─── 10) UpsertConfig validation ────────────────────

func TestUpsertConfig_Validation(t *testing.T) {
	store, mock := newMockStore(t)
	defer mock.Close()

	ctx := context.Background()
	if err := store.UpsertConfig(ctx, WorkspaceConfig{}); err == nil {
		t.Fatal("expected error for missing ID")
	}
	if err := store.UpsertConfig(ctx, WorkspaceConfig{ID: "ws", RetentionDays: -1}); err == nil {
		t.Fatal("expected error for negative retention")
	}
	if err := store.UpsertConfig(ctx, WorkspaceConfig{ID: "ws", LogLevel: "verbose"}); err == nil {
		t.Fatal("expected error for invalid log level")
	}
}

// ─── 11) UpsertConfig happy path ────────────────────

func TestUpsertConfig_HappyPath(t *testing.T) {
	store, mock := newMockStore(t)
	defer mock.Close()

	ctx := context.Background()
	mock.ExpectExec("INSERT INTO workspace_configs").
		WithArgs(
			"ws_h", "WS-Happy", 25.0, 50.0,
			100, 1000,
			[]string{"claude-haiku-4-5"}, []string{"anthropic"},
			"all", 30, pgxmock.AnyArg(),
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	err := store.UpsertConfig(ctx, WorkspaceConfig{
		ID:               "ws_h",
		Name:             "WS-Happy",
		SpendingCapUSD:   25.0,
		MonthlyBudget:    50.0,
		RateLimitRPM:     100,
		RateLimitTPM:     1000,
		AllowedModels:    []string{"claude-haiku-4-5"},
		AllowedProviders: []string{"anthropic"},
		LogLevel:         "all",
		RetentionDays:    30,
	})
	if err != nil {
		t.Fatalf("UpsertConfig: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ─── 12) RevokeAPIKey is idempotent ─────────────────

func TestRevokeAPIKey(t *testing.T) {
	store, mock := newMockStore(t)
	defer mock.Close()

	mock.ExpectExec("DELETE FROM workspace_api_keys").
		WithArgs("key_to_kill").
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	if err := store.RevokeAPIKey(context.Background(), "key_to_kill"); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
}

// ─── 13) CreateAPIKey rejects invalid scope ─────────

func TestCreateAPIKey_RejectsInvalidScope(t *testing.T) {
	store, mock := newMockStore(t)
	defer mock.Close()
	_, _, err := store.CreateAPIKey(context.Background(),
		"ws_z", "n", []string{"billing"}, nil)
	if !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("expected ErrInvalidScope, got %v", err)
	}
}
