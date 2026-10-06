package market

// B32.26 — lineage royalties: each sale pays its originals, up the chain, capped.
//
// When a billed use clears (a use, a licence or a renewal alike), Talyvor's fee comes off first and the rest — the
// pool — flows up the sold version's family tree (lineage.go) breadth first: each parent receives its edge's share of
// what its child received, rounded down, and keeps it less what flows on to its own parents. The flow stops at
// LENS_LINEAGE_MAX_DEPTH generations or where an amount rounds to 0 µUSD. At every node the parents together receive
// at most LENS_LINEAGE_TOTAL_CAP_BPS of what that node received; above it each is scaled down, rounded down, and the
// rest stays with the node. An ancestor linked to the buyer or to the seller (their own, a shared card or owner, or
// the other side of the test-money wall) earns nothing, and its amount stays with the node below it.
//
// Every payee is one market_earnings row (migration 0208) in the sale's one clear entry, and waits the same holdback.

import (
	"context"
	"errors"
	"fmt"
	"math/bits"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/fees"
	"github.com/talyvor/lens/internal/workspace"
)

// The kinds of market_earnings row: one per payee of a sale.
const (
	EarningSale    = "sale"    // the seller's
	EarningSplit   = "split"   // a declared share's (B32.34, B30.82)
	EarningLineage = "lineage" // an ancestor's royalty
)

// DefaultLineageTotalCapBPS is the most a node's parents together receive of what it received
// (LENS_LINEAGE_TOTAL_CAP_BPS): 5000, 50% — a proposal for Nicolai.
const DefaultLineageTotalCapBPS = 5000

// SetLineageTotalCap sets the most a node's parents together receive of what it received (LENS_LINEAGE_TOTAL_CAP_BPS).
func (s *Store) SetLineageTotalCap(bps int) { s.lineageTotalCap = &bps }

func (s *Store) lineageCap() int64 {
	if s.lineageTotalCap == nil {
		return DefaultLineageTotalCapBPS
	}
	return int64(*s.lineageTotalCap)
}

// royalty is what one ancestor keeps of a sale: what the lineage edge paid it, less what flowed on to its parents.
type royalty struct {
	payee  string // the ancestor listing's owner
	edgeID string // the market_lineage edge that paid it
	depth  int    // 1 a parent, 2 a grandparent, …
	amount int64  // µUSD
}

// versionRef is one version of a listing.
type versionRef struct {
	listing string
	version int
}

// lineageEdge is one parent of a version: the edge, the parent version, its share and its listing's owner.
type lineageEdge struct {
	id       string
	parent   versionRef
	shareBPS int64
	owner    string
}

// mulDiv is a × b ÷ c rounded down, for 0 ≤ b ≤ c: never more than a, so it cannot overflow.
func mulDiv(a, b, c int64) int64 {
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	q, _ := bits.Div64(hi, lo, uint64(c))
	return int64(q)
}

// flowUp sends pool up root's family tree, breadth first, and answers what the seller keeps and what each ancestor
// does. parents reads a version's parents; pays says whether an ancestor's owner may earn from this sale.
func flowUp(pool int64, root versionRef, maxDepth int, capBPS int64,
	parents func(versionRef) ([]lineageEdge, error), pays func(owner string) (bool, error)) (int64, []royalty, error) {
	type node struct {
		at       versionRef
		received int64
		depth    int
		row      int // its royalty in out; -1 the seller
	}
	keep := pool
	var out []royalty
	queue := []node{{root, pool, 0, -1}}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if n.depth >= maxDepth || n.received == 0 {
			continue
		}
		edges, err := parents(n.at)
		if err != nil {
			return 0, nil, err
		}
		amounts := make([]int64, len(edges))
		var total int64
		for i, e := range edges {
			amounts[i] = mulDiv(n.received, e.shareBPS, fees.BPSDenominator)
			total += amounts[i]
		}
		if limit := mulDiv(n.received, capBPS, fees.BPSDenominator); total > limit {
			for i := range amounts {
				amounts[i] = mulDiv(amounts[i], limit, total)
			}
		}
		var given int64
		for i, e := range edges {
			if amounts[i] == 0 {
				continue
			}
			ok, err := pays(e.owner)
			if err != nil {
				return 0, nil, err
			}
			if !ok {
				continue
			}
			given += amounts[i]
			out = append(out, royalty{payee: e.owner, edgeID: e.id, depth: n.depth + 1, amount: amounts[i]})
			queue = append(queue, node{e.parent, amounts[i], n.depth + 1, len(out) - 1})
		}
		if n.row < 0 {
			keep -= given
		} else {
			out[n.row].amount -= given
		}
	}
	// An edge reached by two paths (a diamond in the tree) is one payee: its amounts add up, at its nearest depth.
	merged := out[:0]
	at := map[string]int{}
	for _, r := range out {
		if i, ok := at[r.edgeID]; ok {
			merged[i].amount += r.amount
			merged[i].depth = min(merged[i].depth, r.depth)
			continue
		}
		at[r.edgeID] = len(merged)
		merged = append(merged, r)
	}
	rows := merged[:0]
	for _, r := range merged {
		if r.amount > 0 {
			rows = append(rows, r)
		}
	}
	return keep, rows, nil
}

// lineageRoyaltiesTx flows pool, the seller's part of a sale of listing's version to buyer, up its family tree on
// tx, and answers what the seller keeps and each ancestor's royalty. live is whether the sale is real money: the
// lineage_royalties capability is asked whether it may pay it.
func (s *Store) lineageRoyaltiesTx(ctx context.Context, tx pgx.Tx, buyer, seller string, sold versionRef, pool int64, live bool) (int64, []royalty, error) {
	if c, ok := economy.CapabilityByKey(economy.CapabilityLineageRoyalties); !ok || (live && c.Class != economy.ClassGreen) {
		return pool, nil, nil
	}
	parents := func(v versionRef) ([]lineageEdge, error) {
		rows, err := tx.Query(ctx, `SELECT e.id, e.parent_listing_id, e.parent_version, e.share_bps, l.workspace_id
			FROM market_lineage e JOIN market_listings l ON l.id = e.parent_listing_id
			WHERE e.child_listing_id = $1 AND e.child_version = $2 ORDER BY e.created_at, e.id`, v.listing, v.version)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, func(row pgx.CollectableRow) (lineageEdge, error) {
			var e lineageEdge
			return e, row.Scan(&e.id, &e.parent.listing, &e.parent.version, &e.shareBPS, &e.owner)
		})
	}
	known := map[string]bool{}
	pays := func(owner string) (bool, error) {
		if ok, seen := known[owner]; seen {
			return ok, nil
		}
		ok := owner != buyer && owner != seller
		if ok {
			if err := workspace.CheckMoneyWall(ctx, tx, buyer, owner); errors.Is(err, workspace.ErrMoneyWall) {
				ok = false
			} else if err != nil {
				return false, err
			}
		}
		if ok {
			var linked bool
			if err := tx.QueryRow(ctx, `SELECT `+linkedSQL("$1", "$2")+` OR `+linkedSQL("$1", "$3"), owner, buyer, seller).Scan(&linked); err != nil {
				return false, fmt.Errorf("market: single-party check: %w", err)
			}
			ok = !linked
		}
		known[owner] = ok
		return ok, nil
	}
	keep, out, err := flowUp(pool, sold, s.maxLineageDepth(), s.lineageCap(), parents, pays)
	if err != nil {
		return 0, nil, fmt.Errorf("market: lineage royalties: %w", err)
	}
	return keep, out, nil
}
