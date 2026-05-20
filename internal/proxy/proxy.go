package proxy

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/learner"
	"github.com/talyvor/lens/internal/router"
)

type Proxy struct {
	log        *slog.Logger
	exact      *cache.ExactCache
	semantic   *cache.SemanticCache
	modelRoute *router.ModelRouter
	compressor *compressor.Compressor
	learner    *learner.Learner
}

type Deps struct {
	Logger     *slog.Logger
	Exact      *cache.ExactCache
	Semantic   *cache.SemanticCache
	Router     *router.ModelRouter
	Compressor *compressor.Compressor
	Learner    *learner.Learner
}

func New(d Deps) *Proxy {
	return &Proxy{
		log:        d.Logger,
		exact:      d.Exact,
		semantic:   d.Semantic,
		modelRoute: d.Router,
		compressor: d.Compressor,
		learner:    d.Learner,
	}
}

func (p *Proxy) HandleOpenAI(w http.ResponseWriter, r *http.Request) {
	p.notImplemented(w, r, "openai")
}

func (p *Proxy) HandleAnthropic(w http.ResponseWriter, r *http.Request) {
	p.notImplemented(w, r, "anthropic")
}

func (p *Proxy) notImplemented(w http.ResponseWriter, r *http.Request, provider string) {
	p.log.Info("proxy request",
		slog.String("provider", provider),
		slog.String("path", r.URL.Path),
		slog.String("method", r.Method),
	)
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
