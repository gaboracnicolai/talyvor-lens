package partners

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// BrokerPartner deals in shares, crypto and prediction markets for an owner, on the owner's mandate — execution
// only: the capabilities invest_and_trade, trade_equities, trade_crypto, trade_prediction and treasury_sweep.
type BrokerPartner interface {
	// Name identifies the implementation on every request it handles.
	Name() string
	// OpenAccount opens a dealing account in one currency for a holder. Idempotent on req.ID.
	OpenAccount(ctx context.Context, req AccountRequest) (Account, error)
	// Quote is the price of one whole unit of symbol now.
	Quote(ctx context.Context, symbol, currency string) (BrokerQuote, error)
	// Place sends an order. Idempotent on req.ID.
	Place(ctx context.Context, req Order) (Result, error)
	// OrderStatus says where an order stands now.
	OrderStatus(ctx context.Context, orderRef string) (Result, error)
	// Cancel withdraws an order that has not filled.
	Cancel(ctx context.Context, orderRef string) (Result, error)
	// Positions is what an account holds.
	Positions(ctx context.Context, accountRef string) ([]Position, error)
	// Fills is every fill on an account at or after since, oldest first.
	Fills(ctx context.Context, accountRef string, since time.Time) ([]Fill, error)
}

// The sides of an order.
const (
	SideBuy  = "buy"
	SideSell = "sell"
)

// StatusCancelled is an order withdrawn before it filled.
const StatusCancelled Status = "cancelled"

// BrokerQuote is the price of one whole unit of a symbol.
type BrokerQuote struct {
	Symbol string    `json:"symbol"`
	Price  Money     `json:"price"`
	At     time.Time `json:"at"`
}

// Order buys or sells Amount's worth of a symbol.
type Order struct {
	ID         string `json:"id"` // the caller's id for the order
	AccountRef string `json:"account_ref"`
	Symbol     string `json:"symbol"`
	Side       string `json:"side"`
	Amount     Money  `json:"amount"`
}

// Position is what an account holds of one symbol: millionths of a unit, and what it cost.
type Position struct {
	Symbol         string `json:"symbol"`
	QuantityMicros int64  `json:"quantity_micros"`
	Cost           Money  `json:"cost"`
}

// Fill is an order filling. A busted fill shows as the fill and its reversal, the quantity and amount negated.
type Fill struct {
	OrderRef       string    `json:"order_ref"`
	Symbol         string    `json:"symbol"`
	Side           string    `json:"side"`
	QuantityMicros int64     `json:"quantity_micros"`
	Price          Money     `json:"price"`
	Amount         Money     `json:"amount"`
	At             time.Time `json:"at"`
}

// TestBrokerPartner is test mode for dealing: it trades nothing. Every symbol has a fixed test price, accounts open
// by the holder's name, and orders answer by their amount (see the package comment): one that fills shows as a
// fill and a position, and one that comes back as the fill and its reversal. It refuses a sale of more than the
// account holds.
type TestBrokerPartner struct {
	book  testBook
	fills map[string][]Fill
}

// Name is "test".
func (*TestBrokerPartner) Name() string { return "test" }

// OpenAccount opens a dealing account by the holder's name.
func (p *TestBrokerPartner) OpenAccount(_ context.Context, req AccountRequest) (Account, error) {
	if _, ok := currencies[req.Currency]; !ok {
		return Account{}, fmt.Errorf("%w: a currency is GBP, EUR, USD or USDC, not %q", ErrInvalid, req.Currency)
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	op, _, err := p.book.record("bacct", req.ID, req, byName(req.Holder))
	if err != nil {
		return Account{}, err
	}
	return Account{Result: op.result, Currency: req.Currency}, nil
}

// Quote is the symbol's test price: between 10 and 1,000 whole units, fixed by its name. Never a market price.
func (p *TestBrokerPartner) Quote(_ context.Context, symbol, currency string) (BrokerQuote, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	digits, ok := currencies[currency]
	if symbol == "" || !ok {
		return BrokerQuote{}, fmt.Errorf("%w: a quote names a symbol and a currency", ErrInvalid)
	}
	sum := sha256.Sum256([]byte(symbol))
	units := 10 + int64(binary.BigEndian.Uint64(sum[:8])%991)
	return BrokerQuote{Symbol: symbol, Price: Money{units * pow10(digits).Int64(), currency}, At: p.book.clock()}, nil
}

// Place trades nothing: it answers by the order's amount, and fills at the test price.
func (p *TestBrokerPartner) Place(ctx context.Context, req Order) (Result, error) {
	if err := checkMoney(req.Amount); err != nil {
		return Result{}, err
	}
	if req.Side != SideBuy && req.Side != SideSell {
		return Result{}, fmt.Errorf("%w: an order is a buy or a sell, not %q", ErrInvalid, req.Side)
	}
	quote, err := p.Quote(ctx, req.Symbol, req.Amount.Currency)
	if err != nil {
		return Result{}, err
	}
	req.Symbol = quote.Symbol
	if req.Amount.Minor > math.MaxInt64/1_000_000 {
		return Result{}, fmt.Errorf("%w: %s is more than an order can be", ErrInvalid, req.Amount)
	}
	qty := req.Amount.Minor * 1_000_000 / quote.Price.Minor
	if qty == 0 {
		return Result{}, fmt.Errorf("%w: %s buys less than a millionth of %s", ErrInvalid, req.Amount, req.Symbol)
	}
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	acct, err := p.book.get("bacct", req.AccountRef)
	if err != nil {
		return Result{}, err
	}
	if st := acct.status().Status; st != StatusCompleted {
		return Result{}, fmt.Errorf("%w: account %s is %s, not open", ErrInvalid, req.AccountRef, st)
	}
	if cur := acct.request.(AccountRequest).Currency; cur != req.Amount.Currency {
		return Result{}, fmt.Errorf("%w: account %s deals in %s, not %s", ErrInvalid, req.AccountRef, cur, req.Amount.Currency)
	}
	decide := byAmount(req.Amount)
	if _, retry := p.book.ops[testRef("order", req.ID)]; !retry && req.Side == SideSell && p.held(req.AccountRef, req.Symbol) < qty {
		decide = func(string) (Result, Status) {
			return Result{Status: StatusFailed, Detail: "test mode: the account does not hold that much " + req.Symbol}, StatusFailed
		}
	}
	op, fresh, err := p.book.record("order", req.ID, req, decide)
	if err != nil {
		return Result{}, err
	}
	if fresh && op.result.Status == StatusCompleted {
		sign := int64(1)
		if req.Side == SideSell {
			sign = -1
		}
		f := Fill{OrderRef: op.result.Ref, Symbol: req.Symbol, Side: req.Side, QuantityMicros: sign * qty, Price: quote.Price,
			Amount: Money{sign * req.Amount.Minor, req.Amount.Currency}, At: op.at}
		if p.fills == nil {
			p.fills = map[string][]Fill{}
		}
		p.fills[req.AccountRef] = append(p.fills[req.AccountRef], f)
		if op.later == StatusReturned {
			f.QuantityMicros, f.Amount.Minor = -f.QuantityMicros, -f.Amount.Minor
			p.fills[req.AccountRef] = append(p.fills[req.AccountRef], f)
		}
	}
	return op.result, nil
}

// held is how much of symbol an account holds, in millionths. The caller holds p.book.mu.
func (p *TestBrokerPartner) held(accountRef, symbol string) int64 {
	var q int64
	for _, f := range p.fills[accountRef] {
		if f.Symbol == symbol {
			q += f.QuantityMicros
		}
	}
	return q
}

// OrderStatus is where an order stands now.
func (p *TestBrokerPartner) OrderStatus(_ context.Context, orderRef string) (Result, error) {
	return p.book.status("order", orderRef)
}

// Cancel withdraws a pending order; one that has filled or failed cannot be.
func (p *TestBrokerPartner) Cancel(_ context.Context, orderRef string) (Result, error) {
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	op, err := p.book.get("order", orderRef)
	if err != nil {
		return Result{}, err
	}
	if st := op.status().Status; st != StatusPending && st != StatusCancelled {
		return Result{}, fmt.Errorf("%w: order %s is %s and cannot be cancelled", ErrInvalid, orderRef, st)
	}
	op.result.Status, op.later, op.result.Detail = StatusCancelled, StatusCancelled, "cancelled"
	return op.result, nil
}

// Positions is what the account's fills add up to, by symbol.
func (p *TestBrokerPartner) Positions(_ context.Context, accountRef string) ([]Position, error) {
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	if _, err := p.book.get("bacct", accountRef); err != nil {
		return nil, err
	}
	by := map[string]*Position{}
	for _, f := range p.fills[accountRef] {
		pos, ok := by[f.Symbol]
		if !ok {
			pos = &Position{Symbol: f.Symbol, Cost: Money{0, f.Amount.Currency}}
			by[f.Symbol] = pos
		}
		pos.QuantityMicros += f.QuantityMicros
		pos.Cost.Minor += f.Amount.Minor
	}
	out := []Position{}
	for _, pos := range by {
		if pos.QuantityMicros != 0 {
			out = append(out, *pos)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out, nil
}

// Fills is the account's fills from since.
func (p *TestBrokerPartner) Fills(_ context.Context, accountRef string, since time.Time) ([]Fill, error) {
	p.book.mu.Lock()
	defer p.book.mu.Unlock()
	if _, err := p.book.get("bacct", accountRef); err != nil {
		return nil, err
	}
	out := []Fill{}
	for _, f := range p.fills[accountRef] {
		if !f.At.Before(since) {
			out = append(out, f)
		}
	}
	return out, nil
}
