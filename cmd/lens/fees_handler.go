package main

import (
	"net/http"

	"github.com/talyvor/lens/internal/fees"
)

// publicFeesHandler is GET /v1/public/fees (B32.8): every fee Talyvor charges, as lens.env sets it, for /pricing.
func publicFeesHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSONOK(w, http.StatusOK, fees.Current())
}
