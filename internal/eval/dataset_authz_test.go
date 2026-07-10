package eval

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/lens/internal/quality"
)

// B-Lens eval IDOR: the AddDatasetCase + eval/run handlers now gate on GetDataset(wsID, dsID) before
// touching a dataset by its bare id (a member of workspace A could otherwise inject cases into, or run,
// workspace B's dataset). This proves the gate's primitive: GetDataset scopes by workspace_id, so a
// foreign dataset resolves to not-found — the ownership check the handlers rely on.
func TestGetDataset_WorkspaceScoped_RefusesForeign(t *testing.T) {
	pool := newPool(t)
	// Caller authorized for ws-A asks for ds-B (owned by ws-B): the query must carry workspace_id=ws-A,
	// so ds-B does not match and no row comes back → error (not-found), NOT ds-B's content.
	pool.ExpectQuery(`FROM eval_datasets WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs("ds-B", "ws-A").
		WillReturnRows(pgxmock.NewRows([]string{"id", "workspace_id", "name", "description", "created_at"}))

	p := newPipeline(pool, quality.New(nil), "k", "k", "k")
	if _, err := p.GetDataset(context.Background(), "ws-A", "ds-B"); err == nil {
		t.Errorf("GetDataset(ws-A, ds-B) returned nil error for a foreign dataset — the handler ownership gate would pass and leak/poison ws-B's dataset")
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("GetDataset did not scope by workspace_id: %v", err)
	}
}
