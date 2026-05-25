package mining

// pgx_test_helpers.go — tiny test-only shims that let the test
// file reference pgx.ErrNoRows without importing pgx itself
// (keeps the test imports tight).

import "github.com/jackc/pgx/v5"

var errPgxNoRows = pgx.ErrNoRows
