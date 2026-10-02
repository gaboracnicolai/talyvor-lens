package workspace

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// B26.1 — SYNTHETIC WORKSPACES DO NOT PILE UP.
//
// Every tester run creates hundreds of synthetic workspaces and none was ever removed. PurgeStaleSynthetic
// deletes the ones older than a cutoff with the rows they own, all of it test data, EXCEPT:
//
//   - a workspace that shares a row with a real workspace — a money row whose other side is real, from
//     before B25.1's wall — stays, with its rows, for the crossings report (B26.15);
//   - rows in an append-only table (an audit_no_mutation trigger: the ledgers, the audit trail) stay, as
//     U14 requires. They carry B25.1's test mark, so no real figure counts them.
//
// "The rows they own" is every row of every table in the schema with a text column named workspace_id or
// *_workspace_id naming one of them, found from the catalog at run time, so a table added later is
// covered without a list to keep. A table a delete cannot reach (a foreign key from a table without such a
// column) keeps its rows; the rest go.

// syntheticTable is a table with workspace columns, and whether it is append-only.
type syntheticTable struct {
	name       string
	cols       []string
	appendOnly bool
}

const syntheticTablesSQL = `
SELECT c.relname, array_agg(a.attname::text ORDER BY a.attnum),
       EXISTS (SELECT 1 FROM pg_trigger t WHERE t.tgrelid = c.oid AND t.tgname = 'audit_no_mutation' AND NOT t.tgisinternal)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p') AND NOT c.relispartition
  AND c.relname <> 'workspaces'
  AND (a.attname = 'workspace_id' OR a.attname LIKE '%\_workspace\_id')
  AND a.atttypid IN ('text'::regtype, 'varchar'::regtype)
GROUP BY c.oid, c.relname
ORDER BY c.relname`

// PurgeResult is what one PurgeStaleSynthetic run did.
type PurgeResult struct {
	Purged []string // the workspaces deleted
	Kept   []string // stale, but sharing a row with a real workspace
	// Unreached names the tables whose delete was refused to the end (a foreign key from a table without
	// a workspace column, a trigger): their rows of the purged workspaces stay.
	Unreached []string
}

// PurgeStaleSynthetic deletes the synthetic workspaces created before cutoff, with the rows they own, in
// one transaction; see the comment above for what it keeps. It only ever deletes synthetic
// workspaces: the candidates are read with "synthetic" and deleted with it.
func (m *Manager) PurgeStaleSynthetic(ctx context.Context, cutoff time.Time) (PurgeResult, error) {
	var res PurgeResult
	if m.pool == nil {
		return res, nil
	}
	var stale []string
	if err := m.pool.QueryRow(ctx, `SELECT COALESCE(array_agg(id ORDER BY id), '{}') FROM workspaces
		WHERE synthetic AND created_at < $1`, cutoff).Scan(&stale); err != nil {
		return res, fmt.Errorf("workspace: stale synthetic: %w", err)
	}
	if len(stale) == 0 {
		return res, nil
	}
	tables, err := m.syntheticTables(ctx)
	if err != nil {
		return res, err
	}

	// A stale workspace on a row whose other workspace column names a real workspace is kept.
	kept := map[string]bool{}
	for _, t := range tables {
		for _, own := range t.cols {
			for _, other := range t.cols {
				if own == other {
					continue
				}
				var ids []string
				if err := m.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COALESCE(array_agg(DISTINCT t.%[2]s), '{}') FROM %[1]s t
					JOIN workspaces w ON w.id = t.%[3]s AND NOT w.synthetic WHERE t.%[2]s = ANY($1::text[])`,
					pgx.Identifier{t.name}.Sanitize(), pgx.Identifier{own}.Sanitize(), pgx.Identifier{other}.Sanitize()),
					stale).Scan(&ids); err != nil {
					return res, fmt.Errorf("workspace: crossings in %s: %w", t.name, err)
				}
				for _, id := range ids {
					kept[id] = true
				}
			}
		}
	}
	var doomed []string
	for _, id := range stale {
		if kept[id] {
			res.Kept = append(res.Kept, id)
		} else {
			doomed = append(doomed, id)
		}
	}
	if len(doomed) == 0 {
		return res, nil
	}

	txer, ok := m.pool.(interface {
		Begin(ctx context.Context) (pgx.Tx, error)
	})
	if !ok {
		return res, fmt.Errorf("workspace: purge needs a database that begins transactions")
	}
	tx, err := txer.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("workspace: begin purge: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One set-based DELETE per table, each under a savepoint. A delete a foreign key refuses is tried again
	// after the others, until a whole pass deletes nothing new.
	var pending []syntheticTable
	for _, t := range tables {
		if !t.appendOnly {
			pending = append(pending, t)
		}
	}
	for progress := true; progress && len(pending) > 0; {
		progress = false
		var refused []syntheticTable
		for _, t := range pending {
			preds := make([]string, len(t.cols))
			for i, c := range t.cols {
				preds[i] = pgx.Identifier{c}.Sanitize() + ` = ANY($1::text[])`
			}
			sp, err := tx.Begin(ctx)
			if err != nil {
				return res, fmt.Errorf("workspace: savepoint: %w", err)
			}
			if _, err := sp.Exec(ctx, `DELETE FROM `+pgx.Identifier{t.name}.Sanitize()+` WHERE `+strings.Join(preds, " OR "), doomed); err != nil {
				_ = sp.Rollback(ctx)
				refused = append(refused, t)
				continue
			}
			if err := sp.Commit(ctx); err != nil {
				return res, fmt.Errorf("workspace: release savepoint: %w", err)
			}
			progress = true
		}
		pending = refused
	}
	for _, t := range pending {
		res.Unreached = append(res.Unreached, t.name)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspaces WHERE id = ANY($1::text[]) AND synthetic`, doomed); err != nil {
		return res, fmt.Errorf("workspace: delete synthetic: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("workspace: commit purge: %w", err)
	}
	m.mu.Lock()
	for _, id := range doomed {
		delete(m.workspaces, id)
	}
	m.mu.Unlock()
	res.Purged = doomed
	return res, nil
}

func (m *Manager) syntheticTables(ctx context.Context) ([]syntheticTable, error) {
	rows, err := m.pool.Query(ctx, syntheticTablesSQL)
	if err != nil {
		return nil, fmt.Errorf("workspace: workspace tables: %w", err)
	}
	defer rows.Close()
	var out []syntheticTable
	for rows.Next() {
		var t syntheticTable
		if err := rows.Scan(&t.name, &t.cols, &t.appendOnly); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
