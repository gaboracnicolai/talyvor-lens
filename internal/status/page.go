// Package status implements the public Talyvor Lens status page.
// Customers and uptime monitors poll /status (HTML) or /status.json
// before filing tickets. Checks run in parallel and the result is
// cached for 60s so the upstream provider services aren't hammered
// once per page-view.
package status

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
)

// pgxPinger is the subset of *pgxpool.Pool the status check uses.
// Decoupled so tests can drop in a fake without spinning up a real
// Postgres or pgxmock.
type pgxPinger interface {
	Ping(ctx context.Context) error
}

type ComponentStatus string

const (
	StatusOperational ComponentStatus = "operational"
	StatusDegraded    ComponentStatus = "degraded"
	StatusOutage      ComponentStatus = "outage"
	StatusUnknown     ComponentStatus = "unknown"

	// Latency bands. <=100ms healthy, <=500ms slow-but-up, >500ms outage.
	latencyHealthyMs  = 100
	latencyDegradedMs = 500

	// Per-check timeouts. The 5s provider budget caps the worst-case
	// Check() runtime at ~5s (everything runs in parallel) — well under
	// the spec's 10s budget.
	postgresTimeout  = 2 * time.Second
	redisTimeout     = 1 * time.Second
	natsTimeout      = 2 * time.Second
	providerTimeout  = 5 * time.Second
	defaultCacheTick = 60 * time.Second
)

type Component struct {
	Name      string          `json:"name"`
	Status    ComponentStatus `json:"status"`
	Latency   int64           `json:"latency_ms"`
	Message   string          `json:"message,omitempty"`
	CheckedAt time.Time       `json:"checked_at"`
}

type ProviderStatus struct {
	Name      string          `json:"name"`
	Status    ComponentStatus `json:"status"`
	Latency   int64           `json:"latency_ms"`
	CheckedAt time.Time       `json:"checked_at"`
}

type StatusResponse struct {
	Status      ComponentStatus  `json:"status"`
	Version     string           `json:"version"`
	UptimeHours float64          `json:"uptime_hours"`
	Components  []Component      `json:"components"`
	Providers   []ProviderStatus `json:"providers"`
	UpdatedAt   time.Time        `json:"updated_at"`
}

type StatusPage struct {
	pool        pgxPinger
	redisClient *redis.Client
	nc          *nats.Conn
	httpClient  *http.Client
	version     string
	startedAt   time.Time

	// Provider URLs — defaulted to real endpoints; tests swap to httptest.
	openaiURL    string
	anthropicURL string
	googleURL    string
	bedrockURL   string

	mu     sync.RWMutex
	cached *StatusResponse
}

func New(pool *pgxpool.Pool, redisClient *redis.Client, nc *nats.Conn, version string) *StatusPage {
	var p pgxPinger
	if pool != nil {
		p = pool
	}
	return newStatusPage(p, redisClient, nc, version)
}

func newStatusPage(pool pgxPinger, redisClient *redis.Client, nc *nats.Conn, version string) *StatusPage {
	return &StatusPage{
		pool:         pool,
		redisClient:  redisClient,
		nc:           nc,
		httpClient:   &http.Client{Timeout: providerTimeout},
		version:      version,
		startedAt:    time.Now().UTC(),
		openaiURL:    "https://api.openai.com/v1/models",
		anthropicURL: "https://api.anthropic.com/v1/messages",
		googleURL:    "https://generativelanguage.googleapis.com/",
		bedrockURL:   "https://bedrock-runtime.us-east-1.amazonaws.com",
	}
}

// classifyLatency maps a (latency, error) pair to a status. Transport
// errors are always outage regardless of latency. >500ms is also outage
// — the upstream is up but unusable; we want the page to surface it red.
func classifyLatency(latencyMs int64, err error) ComponentStatus {
	if err != nil {
		return StatusOutage
	}
	if latencyMs > latencyDegradedMs {
		return StatusOutage
	}
	if latencyMs > latencyHealthyMs {
		return StatusDegraded
	}
	return StatusOperational
}

// computeOverall returns the worst of every component/provider status.
// Empty input is operational — the system has nothing to fail on.
func computeOverall(components []ComponentStatus, providers []ComponentStatus) ComponentStatus {
	worst := StatusOperational
	upgrade := func(s ComponentStatus) {
		switch s {
		case StatusOutage:
			worst = StatusOutage
		case StatusDegraded:
			if worst != StatusOutage {
				worst = StatusDegraded
			}
		}
	}
	for _, s := range components {
		upgrade(s)
	}
	for _, s := range providers {
		upgrade(s)
	}
	return worst
}

// Check probes every component + provider in parallel and returns the
// aggregated StatusResponse. Per-check timeouts keep the total runtime
// bounded by the slowest single probe.
func (s *StatusPage) Check(ctx context.Context) StatusResponse {
	now := time.Now().UTC()

	// Run every probe in its own goroutine. Each returns its result via
	// a slot in a fixed-size slice so we can preserve display order.
	var (
		wg            sync.WaitGroup
		components    = make([]Component, 4)
		providers     = make([]ProviderStatus, 4)
	)

	wg.Add(8)
	go func() { defer wg.Done(); components[0] = s.checkPostgres(ctx) }()
	go func() { defer wg.Done(); components[1] = s.checkRedis(ctx) }()
	go func() { defer wg.Done(); components[2] = s.checkNATS(ctx) }()
	go func() { defer wg.Done(); components[3] = s.checkProxy() }()
	go func() { defer wg.Done(); providers[0] = s.checkProvider(ctx, "OpenAI", s.openaiURL) }()
	go func() { defer wg.Done(); providers[1] = s.checkProvider(ctx, "Anthropic", s.anthropicURL) }()
	go func() { defer wg.Done(); providers[2] = s.checkProvider(ctx, "Google Gemini", s.googleURL) }()
	go func() { defer wg.Done(); providers[3] = s.checkProvider(ctx, "AWS Bedrock", s.bedrockURL) }()
	wg.Wait()

	cs := make([]ComponentStatus, len(components))
	for i, c := range components {
		cs[i] = c.Status
	}
	ps := make([]ComponentStatus, len(providers))
	for i, p := range providers {
		ps[i] = p.Status
	}
	overall := computeOverall(cs, ps)

	// UptimeHours rounded to 2 decimals so the dashboard renders cleanly.
	hours := time.Since(s.startedAt).Hours()
	hours = float64(int(hours*100+0.5)) / 100

	return StatusResponse{
		Status:      overall,
		Version:     s.version,
		UptimeHours: hours,
		Components:  components,
		Providers:   providers,
		UpdatedAt:   now,
	}
}

func (s *StatusPage) checkPostgres(ctx context.Context) Component {
	c := Component{Name: "PostgreSQL", CheckedAt: time.Now().UTC()}
	if s.pool == nil {
		c.Status = StatusUnknown
		c.Message = "not configured"
		return c
	}
	probeCtx, cancel := context.WithTimeout(ctx, postgresTimeout)
	defer cancel()
	start := time.Now()
	err := s.pool.Ping(probeCtx)
	c.Latency = time.Since(start).Milliseconds()
	c.Status = classifyLatency(c.Latency, err)
	if err != nil {
		c.Message = err.Error()
	}
	return c
}

func (s *StatusPage) checkRedis(ctx context.Context) Component {
	c := Component{Name: "Redis", CheckedAt: time.Now().UTC()}
	if s.redisClient == nil {
		c.Status = StatusUnknown
		c.Message = "not configured"
		return c
	}
	probeCtx, cancel := context.WithTimeout(ctx, redisTimeout)
	defer cancel()
	start := time.Now()
	err := s.redisClient.Ping(probeCtx).Err()
	c.Latency = time.Since(start).Milliseconds()
	c.Status = classifyLatency(c.Latency, err)
	if err != nil {
		c.Message = err.Error()
	}
	return c
}

// checkNATS does a subscribe-then-publish round trip on _lens.health so
// the latency measurement reflects what a real publisher sees rather
// than just the TCP connection state. NATS delivers our own message
// back to us since we're subscribed; the loop is intentional.
func (s *StatusPage) checkNATS(ctx context.Context) Component {
	c := Component{Name: "NATS", CheckedAt: time.Now().UTC()}
	if s.nc == nil {
		c.Status = StatusUnknown
		c.Message = "not configured"
		return c
	}
	if s.nc.Status() != nats.CONNECTED {
		c.Status = StatusOutage
		c.Message = "not connected"
		return c
	}
	sub, err := s.nc.SubscribeSync("_lens.health")
	if err != nil {
		c.Status = StatusOutage
		c.Message = err.Error()
		return c
	}
	defer func() { _ = sub.Unsubscribe() }()
	_ = s.nc.Flush()

	start := time.Now()
	if err := s.nc.Publish("_lens.health", []byte("ping")); err != nil {
		c.Status = StatusOutage
		c.Message = err.Error()
		c.Latency = time.Since(start).Milliseconds()
		return c
	}
	_, err = sub.NextMsg(natsTimeout)
	c.Latency = time.Since(start).Milliseconds()
	c.Status = classifyLatency(c.Latency, err)
	if err != nil {
		c.Message = err.Error()
	}
	return c
}

// checkProxy is the self-check. If this code is running, the proxy
// process is up — there's nothing to ping. Latency is zero.
func (s *StatusPage) checkProxy() Component {
	return Component{
		Name:      "Proxy",
		Status:    StatusOperational,
		Latency:   0,
		CheckedAt: time.Now().UTC(),
	}
}

// checkProvider issues a HEAD against the provider's base URL. 2xx or
// 4xx both count as reachable — a 401 just means we didn't send a key,
// not that the provider is down. 5xx or transport error → outage.
func (s *StatusPage) checkProvider(ctx context.Context, name, url string) ProviderStatus {
	p := ProviderStatus{Name: name, CheckedAt: time.Now().UTC()}
	probeCtx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, url, nil)
	if err != nil {
		p.Status = StatusOutage
		return p
	}
	start := time.Now()
	resp, err := s.httpClient.Do(req)
	p.Latency = time.Since(start).Milliseconds()
	if err != nil {
		p.Status = StatusOutage
		return p
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		p.Status = StatusDegraded
		return p
	}
	p.Status = classifyLatency(p.Latency, nil)
	return p
}

// UpdateCache replaces the cached snapshot. Exposed for tests; the
// production cacher loop calls this internally via runCheckOnce.
func (s *StatusPage) UpdateCache(resp StatusResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := resp
	s.cached = &copy
}

func (s *StatusPage) snapshot() *StatusResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cached == nil {
		return nil
	}
	c := *s.cached
	return &c
}

func (s *StatusPage) runCheckOnce(ctx context.Context) {
	resp := s.Check(ctx)
	s.UpdateCache(resp)
}

// StartCacher runs Check() on a ticker and stores the result in memory.
// /status and /status.json serve from this cache so a 1000-rps poll of
// the status page doesn't translate into 1000-rps probes of every
// upstream provider.
func (s *StatusPage) StartCacher(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultCacheTick
	}
	// Prime the cache immediately so the first request after boot
	// doesn't see a nil snapshot.
	s.runCheckOnce(ctx)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runCheckOnce(ctx)
		}
	}
}

// ServeHTTP serves the status page. JSON if the Accept header asks for
// it, HTML otherwise. The HTML carries a meta-refresh so customers
// camping on the page see fresh data every 60s without manual reload.
func (s *StatusPage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		s.ServeJSON(w, r)
		return
	}
	snap := s.snapshot()
	if snap == nil {
		// Cache miss (very first request, before the cacher primed it).
		// Run an inline check so we never serve a blank page.
		inline := s.Check(r.Context())
		snap = &inline
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(renderHTML(snap)))
}

// ServeJSON always returns JSON, ignoring the Accept header. The
// /status.json route uses this directly.
func (s *StatusPage) ServeJSON(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshot()
	if snap == nil {
		inline := s.Check(r.Context())
		snap = &inline
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(snap)
}

// renderHTML emits the status page. Intentionally inlined — no
// templates package, no asset loading — so the page renders even if
// every other system is degraded.
func renderHTML(s *StatusResponse) string {
	bannerClass, bannerText := bannerFor(s.Status)
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="60">
<meta property="og:title" content="TALYVOR LENS STATUS">
<meta name="application-name" content="TALYVOR LENS STATUS">
<title>Talyvor Lens Status</title>
<style>
  :root { --bg:#0c0e12; --panel:#13161c; --text:#d4d8e2; --secondary:#8892a4; --accent:#f0a030;
          --good:#5ac17d; --warn:#f0a030; --bad:#e35d6a; --mono: 'IBM Plex Mono', ui-monospace, monospace; }
  * { box-sizing: border-box; }
  body { margin: 0; padding: 40px 24px; background: var(--bg); color: var(--text);
         font-family: var(--mono); }
  main { max-width: 880px; margin: 0 auto; }
  header { margin-bottom: 28px; }
  h1 { margin: 0 0 4px; font-size: 1.6rem; letter-spacing: 0.05em; }
  h1 .accent { color: var(--accent); }
  .subtitle { color: var(--secondary); font-size: 0.9rem; }
  .banner { padding: 24px 20px; border-radius: 12px; margin-bottom: 28px;
            font-size: 1.2rem; font-weight: 600; }
  .banner.good { background: rgba(90, 193, 125, 0.12); color: var(--good); }
  .banner.warn { background: rgba(240, 160, 48, 0.12); color: var(--warn); }
  .banner.bad  { background: rgba(227, 93, 106, 0.12); color: var(--bad); }
  section { background: var(--panel); border-radius: 10px; padding: 18px 20px; margin-bottom: 18px; }
  section h2 { margin: 0 0 12px; font-size: 1rem; color: var(--secondary); letter-spacing: 0.04em; }
  table { width: 100%%; border-collapse: collapse; font-size: 0.95rem; }
  th, td { text-align: left; padding: 8px 6px; border-bottom: 1px solid rgba(255,255,255,0.05); }
  th { color: var(--secondary); font-weight: 500; }
  .pill { display: inline-block; padding: 2px 10px; border-radius: 999px; font-size: 0.8rem; }
  .pill.good { background: rgba(90, 193, 125, 0.12); color: var(--good); }
  .pill.warn { background: rgba(240, 160, 48, 0.12); color: var(--warn); }
  .pill.bad  { background: rgba(227, 93, 106, 0.12); color: var(--bad); }
  .meta { color: var(--secondary); font-size: 0.85rem; margin-top: 24px; }
  footer { color: var(--secondary); font-size: 0.8rem; margin-top: 28px; text-align: center; }
</style>
</head>
<body>
<main>
<header>
  <h1>TALYVOR <span class="accent">LENS</span> STATUS</h1>
  <div class="subtitle">v%s · uptime %s</div>
</header>

<div class="banner %s">%s</div>

<section>
  <h2>Lens Components</h2>
  <table><thead><tr><th>Component</th><th>Status</th><th>Latency</th><th>Message</th></tr></thead><tbody>`,
		htmlEscape(s.Version),
		formatUptime(s.UptimeHours),
		bannerClass,
		bannerText,
	)

	for _, c := range s.Components {
		fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td>%dms</td><td>%s</td></tr>`,
			htmlEscape(c.Name), statusPill(c.Status), c.Latency, htmlEscape(c.Message))
	}
	b.WriteString(`</tbody></table></section>`)

	b.WriteString(`<section><h2>LLM Providers</h2>
<table><thead><tr><th>Provider</th><th>Status</th><th>Latency</th></tr></thead><tbody>`)
	for _, p := range s.Providers {
		fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td>%dms</td></tr>`,
			htmlEscape(p.Name), statusPill(p.Status), p.Latency)
	}
	b.WriteString(`</tbody></table></section>`)

	fmt.Fprintf(&b, `<div class="meta">Last updated: %s UTC</div>
<footer>Updated every 60 seconds automatically</footer>
</main>
</body>
</html>`, s.UpdatedAt.Format("2006-01-02 15:04:05"))
	return b.String()
}

func bannerFor(s ComponentStatus) (cls, text string) {
	switch s {
	case StatusOperational:
		return "good", "All Systems Operational"
	case StatusDegraded:
		return "warn", "Degraded Performance"
	case StatusOutage:
		return "bad", "Service Outage"
	}
	return "warn", "Status Unknown"
}

func statusPill(s ComponentStatus) string {
	switch s {
	case StatusOperational:
		return `<span class="pill good">operational</span>`
	case StatusDegraded:
		return `<span class="pill warn">degraded</span>`
	case StatusOutage:
		return `<span class="pill bad">outage</span>`
	}
	return `<span class="pill">unknown</span>`
}

func formatUptime(hours float64) string {
	days := int(hours / 24)
	rem := hours - float64(days)*24
	if days > 0 {
		return fmt.Sprintf("%d days, %.2f hours", days, rem)
	}
	return fmt.Sprintf("%.2f hours", hours)
}

// htmlEscape covers the typical XSS vectors. Status fields are sourced
// from internal probes (component names, latency strings) so the risk
// is low — but the Message field can carry upstream-error text, and a
// determined attacker controlling the upstream's error body could try
// to smuggle HTML in. Belt + suspenders.
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}
