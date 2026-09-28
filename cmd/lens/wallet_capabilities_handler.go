package main

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
)

// B22.1 — every wallet capability carries its class, and real money obeys it (economy/wallet_capabilities.go).
//
//	GET /v1/wallets/capabilities   each capability, its class (GREEN, AMBER, RED) and whether it takes real
//	                               money now — GREEN always, AMBER and RED only while the operator's clearance
//	                               is in force (`lens wallet-clearances`)
//
// Mounted in the authed group, for the screens; any credential reads it.

type walletCapabilityReader interface {
	WalletCapabilities(ctx context.Context) ([]economy.CapabilityStatus, error)
}

func mountWalletCapabilityRoutes(r chi.Router, store walletCapabilityReader) {
	r.Get("/v1/wallets/capabilities", func(w http.ResponseWriter, req *http.Request) {
		caps, err := store.WalletCapabilities(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"capabilities": caps})
	})
}
