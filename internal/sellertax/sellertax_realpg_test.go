package sellertax

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/envelope"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/migrations"
)

// B32.41 — seller tax details on a migrated Postgres (0226).

func sellerTaxPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_sellertax_%d", time.Now().UnixNano())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = ac.Close(ctx)
		t.Fatal(err)
	}
	_ = ac.Close(ctx)
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	mc, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbmigrate.Run(ctx, mc, migrations.FS); err != nil {
		_ = mc.Close(ctx)
		t.Fatal(err)
	}
	_ = mc.Close(ctx)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if c, err := pgx.Connect(context.Background(), admin); err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	return pool
}

func testRing(t *testing.T) *envelope.Keyring {
	t.Helper()
	k := make([]byte, envelope.KEKLen)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	ring, err := envelope.NewKeyring(k)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func ptr[T any](v T) *T { return &v }

// The stored TIN, date of birth and account identifier are ciphertext that opens only to the seller's own values, and
// every read returns them masked; saving again without them keeps them.
func TestTheStoredTINIsCiphertextAndReadsMasked(t *testing.T) {
	ctx := context.Background()
	pool := sellerTaxPool(t)
	ring := testRing(t)
	store := NewStore(pool, ring, partners.NewRegistry(nil))
	const ws, tin, dob, iban = "ws_seller_b3241", "1234567890", "1985-04-12", "GB33BUKB20201555555555"
	in := Input{SellerType: Individual, FirstName: "Ada", LastName: "Lovelace", Address: "1 Analytical Way, London",
		Country: "GB", TINs: &[]TIN{{Jurisdiction: "gb", Number: "12345 67890"}}, DateOfBirth: ptr(dob),
		AccountIdentifier: ptr("GB33 BUKB 2020 1555 5555 55"), AccountHolder: "Ada Lovelace", VATNumber: "GB 123 456 789"}
	d, err := store.Put(ctx, ws, in)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Complete || d.CompletedAt == nil || len(d.Missing) != 0 || !d.VATValid || d.VATNumber != "GB123456789" {
		t.Fatalf("saved %+v, want complete with a valid VAT number", d)
	}

	var row string
	if err := pool.QueryRow(ctx, `SELECT t::text FROM seller_tax_profiles t WHERE workspace_id = $1`, ws).Scan(&row); err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{tin, dob, iban} {
		if strings.Contains(row, plain) {
			t.Fatalf("the stored row holds %q in the clear: %s", plain, row)
		}
	}
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT tins_sealed FROM seller_tax_profiles WHERE workspace_id = $1`, ws).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var sealed envelope.Sealed
	if err := json.Unmarshal(raw, &sealed); err != nil {
		t.Fatal(err)
	}
	var opened []TIN
	if err := ring.Use(sealed, aad(ws, "tins"), func(p []byte) error { return json.Unmarshal(p, &opened) }); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 1 || opened[0] != (TIN{"GB", tin}) {
		t.Fatalf("the sealed TINs open to %+v, want GB %s", opened, tin)
	}
	if err := ring.Use(sealed, aad("ws_other", "tins"), func([]byte) error { return nil }); err == nil {
		t.Fatal("the sealed TINs opened as another seller's")
	}

	again := in
	again.TINs, again.DateOfBirth, again.AccountIdentifier, again.Address = nil, nil, nil, "2 Difference Engine Row, London"
	if _, err := store.Put(ctx, ws, again); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.TINs) != 1 || got.TINs[0] != (TIN{"GB", "••••7890"}) || got.AccountIdentifier != "••••5555" ||
		got.DateOfBirth != "••••-••-••" || !got.Complete || got.Address != again.Address {
		t.Fatalf("read back %+v, want the TIN and account masked to their last four, the date of birth masked, kept across the save", got)
	}
}

// A VAT number the tax partner finds not valid is kept, marked invalid, and leaves the details incomplete.
func TestAnInvalidVATNumberLeavesTheDetailsIncomplete(t *testing.T) {
	ctx := context.Background()
	store := NewStore(sellerTaxPool(t), testRing(t), partners.NewRegistry(nil))
	d, err := store.Put(ctx, "ws_entity", Input{SellerType: Entity, LegalName: "Never Issued GmbH", CompanyRegistrationNumber: "HRB 12345",
		Address: "Hauptstraße 1, Berlin", Country: "DE", TINs: &[]TIN{{"DE", "12345678901"}}, VATNumber: "DE999999999",
		AccountIdentifier: ptr("DE89370400440532013000"), AccountHolder: "Never Issued GmbH"})
	if err != nil {
		t.Fatal(err)
	}
	if d.VATValid || d.VATDetail == "" || d.Complete || len(d.Missing) != 1 || d.Missing[0] != "vat_number" {
		t.Fatalf("saved %+v, want the VAT number kept invalid and the only thing missing", d)
	}
}
