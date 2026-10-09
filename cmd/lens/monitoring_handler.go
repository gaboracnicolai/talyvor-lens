package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/monitoring"
)

// monitoring_handler.go — B30.7: TRANSACTION MONITORING, AS THE OPERATOR SEES IT.
//
//	GET /v1/admin/compliance/alerts   each alert the rules raised — the rule, the agent, the payment it judged, every
//	                                  payment the pattern is made of and why — newest first, only those on ?case_id=
//	                                  when it is given
//
// The cases the alerts are on are listed with screening's, at GET /v1/admin/screening (kind "monitoring").

// monitoringAlerts is what the operator reads of the alerts: *monitoring.Monitor.
type monitoringAlerts interface {
	Alerts(ctx context.Context, caseID string, limit int) ([]monitoring.Alert, error)
}

func newMonitoringAlertsHandler(alerts monitoringAlerts) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		as, err := alerts.Alerts(req.Context(), strings.TrimSpace(req.URL.Query().Get("case_id")), 500)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"alerts": as})
	})
}

// sweepTransactionMonitoring is the nightly run: once each UTC day — the first hour it runs on a day — the rules judge
// every payment of the last 25 hours, so a day's overlaps the last's. A payment its own run already judged raises
// nothing again.
func sweepTransactionMonitoring(ctx context.Context, m *monitoring.Monitor) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	done := ""
	for {
		now := time.Now()
		if day := now.UTC().Format(time.DateOnly); day != done {
			if n, err := m.Sweep(ctx, now.Add(-25*time.Hour), now); err != nil {
				slog.Warn("monitoring: the nightly run did not judge every payment", "raised", n, "err", err)
			} else {
				done = day
				slog.Info("monitoring: the nightly run judged the last day's payments", "raised", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
