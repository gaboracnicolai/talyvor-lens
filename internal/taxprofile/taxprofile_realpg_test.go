package taxprofile

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/migrations"
)

// B32.38 — buyer tax profiles on a migrated Postgres (0223), resolved against a fake Stripe read.

func profilePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_taxprofile_%d", time.Now().UnixNano())
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
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = c.Close(context.Background())
	})
	return pool
}

// fakeStripe is each Stripe customer's billing and card countries.
type fakeStripe map[string][2]string

func (f fakeStripe) CustomerCountries(_ context.Context, _, customerID string) (string, string, error) {
	c, ok := f[customerID]
	if !ok {
		return "", "", errors.New("no such customer")
	}
	return c[0], c[1], nil
}

func TestResolveWeighsTheDeclarationAgainstStripe(t *testing.T) {
	ctx := context.Background()
	pool := profilePool(t)
	store := NewStore(pool, partners.NewRegistry(nil))
	store.SetStripe(fakeStripe{
		"cus_gb":   {"", "GB"},
		"cus_fr":   {"", "DE"},
		"cus_bill": {"IE", ""},
	})
	for ws, cus := range map[string]string{"ws_gb": "cus_gb", "ws_fr": "cus_fr", "ws_noprofile": "cus_bill"} {
		if _, err := pool.Exec(ctx, `INSERT INTO billing_customers (workspace_id, stripe_customer_id) VALUES ($1, $2)`, ws, cus); err != nil {
			t.Fatal(err)
		}
	}
	put := func(ws string, in Input) Profile {
		t.Helper()
		p, err := store.Put(ctx, ws, in)
		if err != nil {
			t.Fatalf("put %s: %v", ws, err)
		}
		return p
	}
	resolve := func(ws string) Resolution {
		t.Helper()
		r, err := store.Resolve(ctx, ws)
		if err != nil {
			t.Fatalf("resolve %s: %v", ws, err)
		}
		return r
	}

	t.Run("declared GB with a GB card is a GB consumer", func(t *testing.T) {
		put("ws_gb", Input{LegalName: "Ada Lovelace", Address: "1 Test Street, London", Country: "gb", PostalCode: "N1 1AA"})
		r := resolve("ws_gb")
		if r.Country != "GB" || !r.Known || r.Business || r.DecidedBy != DecidedAgreed || r.Flagged {
			t.Fatalf("resolved %+v, want GB, a consumer, agreed, not flagged", r)
		}
		if r.PostalCode != "N1 1AA" || len(r.Evidence) != 2 {
			t.Fatalf("resolved %+v, want the declared postal code and two pieces of evidence", r)
		}
	})

	t.Run("a valid DE VAT number makes a DE business", func(t *testing.T) {
		p := put("ws_de", Input{LegalName: "Beispiel GmbH", Country: "DE", TaxID: "DE 123 456 789"})
		if !p.TaxIDValid || p.TaxID != "DE123456789" || p.TaxIDCheckedAt == nil || p.TaxIDPartner != "test" || !p.Business {
			t.Fatalf("saved %+v, want the number normalised, checked valid by the test partner, a business", p)
		}
		r := resolve("ws_de")
		if r.Country != "DE" || !r.Business || r.TaxID != "DE123456789" || r.DecidedBy != DecidedDeclared {
			t.Fatalf("resolved %+v, want a DE business by its VAT number, on the declaration alone", r)
		}
		if c := r.Customer(); !c.Business || !c.TaxIDValid || c.Country != "DE" {
			t.Fatalf("tax party %+v, want a DE business with a valid tax id", c)
		}
	})

	t.Run("declared FR with a DE card is FR, and flagged", func(t *testing.T) {
		put("ws_fr", Input{LegalName: "Jean Test", Country: "FR"})
		r := resolve("ws_fr")
		if r.Country != "FR" || r.DecidedBy != DecidedDeclaredConflict || !r.Flagged || r.FlagReason != "declared FR, but the card says DE" {
			t.Fatalf("resolved %+v, want FR on conflict, flagged with its reason", r)
		}
		p, err := store.Get(ctx, "ws_fr")
		if err != nil || p.FlaggedAt == nil || p.FlagReason != r.FlagReason {
			t.Fatalf("stored %+v (%v), want the profile flagged", p, err)
		}
		flagged, err := store.Flagged(ctx, 0)
		if err != nil || len(flagged) != 1 || flagged[0].WorkspaceID != "ws_fr" {
			t.Fatalf("operator's list %+v (%v), want ws_fr alone", flagged, err)
		}
		if again := put("ws_fr", Input{LegalName: "Jean Test", Address: "re-saved", Country: "FR"}); again.FlaggedAt == nil || !again.FlaggedAt.Equal(*p.FlaggedAt) {
			t.Fatalf("re-saving FR gave %+v, want the flag kept as it was", again)
		}
		put("ws_fr", Input{LegalName: "Jean Test", Country: "DE"})
		if r := resolve("ws_fr"); r.Country != "DE" || r.Flagged {
			t.Fatalf("after declaring DE, resolved %+v, want DE and the flag cleared", r)
		}
	})

	t.Run("an invalid VAT number is stored invalid and stays a consumer", func(t *testing.T) {
		p := put("ws_bad", Input{LegalName: "Never Issued GmbH", Country: "DE", TaxID: "DE999999999"})
		if p.TaxIDValid || p.TaxID != "DE999999999" || p.TaxIDCheckedAt == nil || p.TaxIDDetail == "" {
			t.Fatalf("saved %+v, want the number kept, checked and marked invalid with why", p)
		}
		if r := resolve("ws_bad"); r.Country != "DE" || r.Business || r.TaxID != "" {
			t.Fatalf("resolved %+v, want a DE consumer", r)
		}
	})

	t.Run("no profile resolves from the Stripe billing country; nothing at all is unknown", func(t *testing.T) {
		if r := resolve("ws_noprofile"); r.Country != "IE" || r.DecidedBy != DecidedStripeBilling || r.Business {
			t.Fatalf("resolved %+v, want IE from the Stripe billing address", r)
		}
		if r := resolve("ws_nothing"); r.Known || r.Country != "" || r.DecidedBy != DecidedUnknown {
			t.Fatalf("resolved %+v, want unknown", r)
		}
	})

	t.Run("a country that is not two letters is refused", func(t *testing.T) {
		if _, err := store.Put(ctx, "ws_x", Input{Country: "Germany"}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("err %v, want ErrInvalid", err)
		}
	})
}
