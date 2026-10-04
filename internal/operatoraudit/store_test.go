package operatoraudit

import (
	"context"
	"errors"
	"testing"
	"time"
)

// B27.28 — an entry's own time cannot be set to make an action look earlier (or later) than it was.
func TestRecord_RefusesABackdatedOrFutureTime(t *testing.T) {
	s := NewStore(nil)
	for _, at := range []time.Time{time.Now().Add(-25 * time.Hour), time.Now().Add(time.Hour)} {
		_, err := s.Record(context.Background(), Entry{Actor: "a", Action: "b", OccurredAt: at})
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("at %s: err = %v; want ErrInvalid", at, err)
		}
	}
}
