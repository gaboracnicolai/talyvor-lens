package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/operatoraudit"
)

// operator_audit_handler.go — B27.28, the operator audit trail. The web app records each operator action
// here (who, what, target, when) and the Operator screen reads it back, filtered, or downloads it as CSV.
//
//   POST /v1/admin/operator-audit/record  — requireAdminOrModerator: the web app's moderator key, which
//                                           names the operator in X-Talyvor-Operator, or the global key.
//   GET  /v1/admin/operator-audit         — requireAdminOrOperatorRead: JSON, newest first.
//   GET  /v1/admin/operator-audit/export  — requireAdminOrOperatorRead: the same filters, as CSV.
//
// Filters (both reads): actor, action, target (exact), since, until (RFC 3339, or a YYYY-MM-DD date —
// an `until` date includes that whole day), limit.

// operatorAuditStore is the slice of *operatoraudit.Store the handlers need.
type operatorAuditStore interface {
	Record(ctx context.Context, e operatoraudit.Entry) (operatoraudit.Entry, error)
	List(ctx context.Context, f operatoraudit.Filter) ([]operatoraudit.Entry, error)
}

func writeOperatorAuditErr(w http.ResponseWriter, err error) {
	if errors.Is(err, operatoraudit.ErrInvalid) {
		writeJSONErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONErr(w, http.StatusInternalServerError, err.Error())
}

// newOperatorAuditRecordHandler answers POST /v1/admin/operator-audit/record. The actor is the operator
// X-Talyvor-Operator names — the one requireAdminOrModerator has already recorded the key's use under —
// or, for the global key, the body's actor. A body naming someone else than the header is refused. `at`
// is optional and must fall within the last 24 hours (operatoraudit.Store.Record).
func newOperatorAuditRecordHandler(store operatorAuditStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Actor  string `json:"actor"`
			Action string `json:"action"`
			Target string `json:"target"`
			Detail string `json:"detail"`
			At     string `json:"at"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		actor := strings.TrimSpace(in.Actor)
		if named := strings.TrimSpace(req.Header.Get(moderatorOperatorHeader)); named != "" {
			if actor != "" && actor != named {
				writeJSONErr(w, http.StatusBadRequest, "actor in the body is not the operator "+moderatorOperatorHeader+" names")
				return
			}
			actor = named
		}
		e := operatoraudit.Entry{Actor: actor, Action: in.Action, Target: in.Target, Detail: in.Detail}
		if at := strings.TrimSpace(in.At); at != "" {
			t, err := time.Parse(time.RFC3339, at)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, "at must be an RFC 3339 time, e.g. 2026-10-04T10:30:00Z")
				return
			}
			e.OccurredAt = t
		}
		rec, err := store.Record(req.Context(), e)
		if err != nil {
			writeOperatorAuditErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, rec)
	})
}

// parseOperatorAuditTime reads an RFC 3339 time or a YYYY-MM-DD date (UTC midnight); endOfDay moves a
// bare date to the following midnight, so an exclusive `until` still includes the day it names.
func parseOperatorAuditTime(name, v string, endOfDay bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	d, err := time.Parse("2006-01-02", v)
	if err != nil {
		return time.Time{}, errors.New(name + " must be an RFC 3339 time or a YYYY-MM-DD date")
	}
	if endOfDay {
		d = d.AddDate(0, 0, 1)
	}
	return d, nil
}

func parseOperatorAuditFilter(q url.Values) (operatoraudit.Filter, error) {
	f := operatoraudit.Filter{Actor: q.Get("actor"), Action: q.Get("action"), Target: q.Get("target")}
	var err error
	if v := strings.TrimSpace(q.Get("since")); v != "" {
		if f.Since, err = parseOperatorAuditTime("since", v, false); err != nil {
			return f, err
		}
	}
	if v := strings.TrimSpace(q.Get("until")); v != "" {
		if f.Until, err = parseOperatorAuditTime("until", v, true); err != nil {
			return f, err
		}
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		if f.Limit, err = strconv.Atoi(v); err != nil || f.Limit < 1 {
			return f, errors.New("limit must be a positive whole number")
		}
	}
	return f, nil
}

// newOperatorAuditListHandler answers GET /v1/admin/operator-audit.
func newOperatorAuditListHandler(store operatorAuditStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f, err := parseOperatorAuditFilter(req.URL.Query())
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		entries, err := store.List(req.Context(), f)
		if err != nil {
			writeOperatorAuditErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"entries": entries})
	})
}

// csvCell keeps a spreadsheet from running a cell as a formula: one starting =, +, -, @, tab or CR is
// prefixed with a quote, which every spreadsheet shows as text.
func csvCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// newOperatorAuditExportHandler answers GET /v1/admin/operator-audit/export — every entry the filters
// select (up to operatoraudit.MaxLimit), newest first, as CSV.
func newOperatorAuditExportHandler(store operatorAuditStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f, err := parseOperatorAuditFilter(req.URL.Query())
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if f.Limit == 0 {
			f.Limit = operatoraudit.MaxLimit
		}
		entries, err := store.List(req.Context(), f)
		if err != nil {
			writeOperatorAuditErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="operator-audit.csv"`)
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"id", "occurred_at", "actor", "action", "target", "detail", "recorded_at"})
		for _, e := range entries {
			_ = cw.Write([]string{
				strconv.FormatInt(e.ID, 10),
				e.OccurredAt.UTC().Format(time.RFC3339),
				csvCell(e.Actor), csvCell(e.Action), csvCell(e.Target), csvCell(e.Detail),
				e.RecordedAt.UTC().Format(time.RFC3339),
			})
		}
		cw.Flush()
	})
}
