package partners

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
)

// testBook is a Test partner's memory: every operation it was asked to do, by reference, so a retry answers as
// the first call did and a later read sees how the operation ended. Its zero value is ready to use.
type testBook struct {
	mu  sync.Mutex
	ops map[string]*testOp
	now func() time.Time // time.Now when nil
}

func (b *testBook) clock() time.Time {
	if b.now == nil {
		return time.Now()
	}
	return b.now()
}

// record answers for operation id of kind: a retry with the same request gets the first answer back (fresh false);
// the same id with a different request is ErrIDReused; a new one is decided by decide and kept. The caller holds
// b.mu.
func (b *testBook) record(kind, id string, req any, decide func(ref string) (Result, Status)) (op *testOp, fresh bool, err error) {
	if err := checkID(id); err != nil {
		return nil, false, err
	}
	if b.ops == nil {
		b.ops = map[string]*testOp{}
	}
	ref := testRef(kind, id)
	if op, ok := b.ops[ref]; ok {
		if !reflect.DeepEqual(op.request, req) {
			return nil, false, fmt.Errorf("%w: %s %q", ErrIDReused, kind, id)
		}
		return op, false, nil
	}
	res, later := decide(ref)
	res.Ref = ref
	op = &testOp{request: req, result: res, later: later, at: b.clock()}
	b.ops[ref] = op
	return op, true, nil
}

// get is the operation ref names, if it is of kind. The caller holds b.mu.
func (b *testBook) get(kind, ref string) (*testOp, error) {
	op, ok := b.ops[ref]
	if !ok || !strings.HasPrefix(ref, "test_"+kind+"_") {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, ref)
	}
	return op, nil
}

// status is what a read of ref reports now.
func (b *testBook) status(kind, ref string) (Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	op, err := b.get(kind, ref)
	if err != nil {
		return Result{}, err
	}
	return op.status(), nil
}

// byAmount decides an operation on an amount by testOutcome.
func byAmount(m Money) func(string) (Result, Status) {
	return func(string) (Result, Status) {
		now, later, detail := testOutcome(m.Minor)
		return Result{Status: now, Detail: detail}, later
	}
}

// byName decides an operation with no amount by testNameOutcome.
func byName(name string) func(string) (Result, Status) {
	return func(string) (Result, Status) {
		now, later, detail := testNameOutcome(name)
		return Result{Status: now, Detail: detail}, later
	}
}
