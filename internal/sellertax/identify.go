package sellertax

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/envelope"
)

// Identification is a seller's details with the sealed values opened: what the annual platform-reporting export
// (B32.44) writes into its file, and nothing else reads. Never log it, return it from an API or store it.
type Identification struct {
	Details
	TINs              []TIN  // in clear
	DateOfBirth       string // in clear, YYYY-MM-DD; "" when not given
	AccountIdentifier string // in clear; "" when not given
}

// Identify opens the details of each seller in workspaceIDs who has saved any, for the platform-reporting export;
// a seller with none is left out. The sealed values are opened with envelope.Use, each against its seller and field.
func (s *Store) Identify(ctx context.Context, workspaceIDs []string) (map[string]Identification, error) {
	if s.ring == nil {
		return nil, ErrNoCustody
	}
	rows, err := s.pool.Query(ctx, `SELECT `+detailsColumns+`, tins_sealed, date_of_birth_sealed, account_identifier_sealed
		FROM seller_tax_profiles WHERE workspace_id = ANY($1) ORDER BY workspace_id`, workspaceIDs)
	if err != nil {
		return nil, fmt.Errorf("sellertax: identify: %w", err)
	}
	type sealedRow struct {
		d                  Details
		tins, dob, account []byte
	}
	var read []sealedRow
	for rows.Next() {
		var r sealedRow
		d, err := s.scan(scanTail{rows, []any{&r.tins, &r.dob, &r.account}})
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("sellertax: identify: %w", err)
		}
		r.d = d
		read = append(read, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sellertax: identify: %w", err)
	}
	out := make(map[string]Identification, len(read))
	for _, r := range read {
		id := Identification{Details: r.d, TINs: []TIN{}}
		ws := r.d.WorkspaceID
		if err := s.open(ws, "tins", r.tins, func(p []byte) error { return json.Unmarshal(p, &id.TINs) }); err != nil {
			return nil, err
		}
		if err := s.open(ws, "date_of_birth", r.dob, func(p []byte) error { id.DateOfBirth = string(p); return nil }); err != nil {
			return nil, err
		}
		if err := s.open(ws, "account_identifier", r.account, func(p []byte) error { id.AccountIdentifier = string(p); return nil }); err != nil {
			return nil, err
		}
		out[ws] = id
	}
	return out, nil
}

// open lends a sealed column's plaintext to fn; a column never given (NULL) is left as it is.
func (s *Store) open(workspaceID, field string, column []byte, fn func(plaintext []byte) error) error {
	if column == nil {
		return nil
	}
	var sealed envelope.Sealed
	if err := json.Unmarshal(column, &sealed); err != nil {
		return fmt.Errorf("sellertax: read the sealed %s of %s: %w", field, workspaceID, err)
	}
	if err := s.ring.Use(sealed, aad(workspaceID, field), fn); err != nil {
		return fmt.Errorf("sellertax: open the %s of %s: %w", field, workspaceID, err)
	}
	return nil
}

// scanTail scans a row whose leading columns are detailsColumns into what scan reads, and the rest into tail.
type scanTail struct {
	row  pgx.Row
	tail []any
}

func (t scanTail) Scan(dest ...any) error { return t.row.Scan(append(dest, t.tail...)...) }
