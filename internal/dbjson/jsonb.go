// Package dbjson centralizes the one correct way to send a JSON document to a
// Postgres jsonb column from Lens. It exists because of #133.
//
// THE BUG IT CLOSES: Lens connects through PgBouncer in transaction-pooling
// mode (LENS_DB_PGBOUNCER=true), which can't keep prepared statements, so pgx
// is configured for the SIMPLE query protocol. Under simple protocol there is
// no server Describe, so pgx infers a parameter's wire type from its Go type —
// and a []byte is inferred as bytea, rendered as a hex string literal that the
// jsonb column rejects with "invalid input syntax for type json" (SQLSTATE
// 22P02). A reproduction probe against the live pgbouncer topology established:
//
//	param form         pooled/simple    direct/extended
//	[]byte  $1             FAIL 22P02        OK
//	[]byte  $1::jsonb      FAIL 22P02        OK    ← the cast does NOT rescue it
//	string  $1             OK                OK    ← text-encoded works on both
//	JSONB   $1 (this type) OK                OK
//
// The lesson: never pass a raw []byte for a jsonb column. Pass a JSONB.
package dbjson

import (
	"database/sql/driver"
	"encoding/json"
)

// JSONB is a JSON document destined for a Postgres jsonb column.
//
// It implements driver.Valuer, returning the document as a string, so pgx
// text-encodes it on BOTH the simple and extended protocols (a string literal
// the jsonb column parses) instead of inferring bytea from the []byte.
type JSONB []byte

// Value implements driver.Valuer.
//
// Empty/nil/"null" collapse to "{}". The jsonb columns Lens writes are
// NOT NULL DEFAULT '{}', so an absent document must serialize to a valid empty
// object, never SQL NULL. The one nullable jsonb column
// (pool_royalty_adjudications.outcome) is left NULL by OMITTING it from the
// INSERT and later set to a real document by the UPDATE — it never relies on
// this type emitting SQL NULL.
func (j JSONB) Value() (driver.Value, error) {
	if len(j) == 0 || string(j) == "null" {
		return "{}", nil
	}
	return string(j), nil
}

// Marshal is the blessed path from any Go value to a jsonb wire value. It
// replaces the scattered `json.Marshal(...) []byte` + null-guard idiom that
// produced the #133 bug. nil/empty inputs round-trip through Value to "{}".
func Marshal(v any) (JSONB, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return JSONB(b), nil
}
