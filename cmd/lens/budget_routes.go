package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/budgets"
)

// The budget create / update / delete handlers. Each reloads the budget
// service's in-memory snapshot so the change takes effect on the next request.
// The row is already committed when the reload runs, so a reload that fails
// does not lose the change — the periodic refresh picks it up — but until then
// the OLD limits are the ones in force. That answers 503 and says so, with the
// saved budget, instead of a 2xx that claims the change is live.

const budgetReloadFailedMsg = "the budget change was saved, but the limits in force could not be reloaded; " +
	"it takes effect at the next refresh"

func writeBudgetReloadFailed(w http.ResponseWriter, err error, saved *budgets.Budget) {
	slog.Error("budget: reload after change failed", slog.String("err", err.Error()))
	body := map[string]any{"error": budgetReloadFailedMsg}
	if saved != nil {
		body["budget"] = saved
	}
	writeJSONOK(w, http.StatusServiceUnavailable, body)
}

func budgetCreateHandler(store *budgets.Store, reload func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		var in budgets.Budget
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		in.WorkspaceID = wsID
		created, err := store.Create(req.Context(), in)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := reload(req.Context()); err != nil {
			writeBudgetReloadFailed(w, err, created)
			return
		}
		writeJSONOK(w, http.StatusCreated, created)
	}
}

func budgetUpdateHandler(store *budgets.Store, reload func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		id := chi.URLParam(req, "id")
		var in budgets.Budget
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		updated, err := store.Update(req.Context(), wsID, id, in)
		if errors.Is(err, budgets.ErrNotFound) {
			writeJSONErr(w, http.StatusNotFound, "budget not found")
			return
		}
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := reload(req.Context()); err != nil {
			writeBudgetReloadFailed(w, err, updated)
			return
		}
		writeJSONOK(w, http.StatusOK, updated)
	}
}

func budgetDeleteHandler(store *budgets.Store, reload func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		id := chi.URLParam(req, "id")
		if err := store.Delete(req.Context(), wsID, id); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := reload(req.Context()); err != nil {
			writeBudgetReloadFailed(w, err, nil)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
