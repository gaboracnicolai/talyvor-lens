package economy

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// B35.2 — a hold, debit, settle or release Postgres cancels as a deadlock or a serialization failure runs again,
// a bounded number of times; a refusal is never run again, and a cancellation on every attempt is
// ErrLockContention, never a shortfall.
func TestB352_ALockFailureRunsAgainAndARefusalDoesNot(t *testing.T) {
	ctx := context.Background()
	deadlock := &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
	serialization := &pgconn.PgError{Code: "40001", Message: "could not serialize access"}

	runs := 0
	err := retryLocks(ctx, func() error {
		if runs++; runs < 3 {
			return deadlock
		}
		return nil
	})
	if err != nil || runs != 3 {
		t.Fatalf("two deadlocks then success: %d runs, %v — want 3 runs and nil", runs, err)
	}

	runs = 0
	err = retryLocks(ctx, func() error { runs++; return serialization })
	if runs != lockRetries+1 || !errors.Is(err, ErrLockContention) || errors.Is(err, ErrInsufficientLXC) {
		t.Fatalf("a serialization failure every time: %d runs, %v — want %d runs and ErrLockContention", runs, err, lockRetries+1)
	}

	runs = 0
	err = retryLocks(ctx, func() error { runs++; return ErrInsufficientLXC })
	if runs != 1 || !errors.Is(err, ErrInsufficientLXC) || errors.Is(err, ErrLockContention) {
		t.Fatalf("a real shortfall: %d runs, %v — want 1 run and ErrInsufficientLXC as it is", runs, err)
	}
}
