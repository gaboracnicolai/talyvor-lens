package status

// One latency band was written for a Postgres ping on the same box and then
// applied, unchanged, to a HEAD against api.openai.com. That single fact made
// the page useless: the deployment is in Europe, three of the four providers are
// in the US, and a transatlantic round trip costs more than the 100 ms "healthy"
// ceiling on physics alone. So the page reported DEGRADED permanently, and a
// signal that is always amber carries no information — nobody believes it when
// something is actually wrong.
//
// MEASURED, not assumed (2026-07-26, from a European host — Chișinău, MD;
// full HEAD including DNS + TCP + TLS, 15 cold samples per provider):
//
//	provider          min    p50    p90    max
//	OpenAI            180    233    289    300
//	Anthropic          24     28     41     115   (answered at a nearby edge)
//	Google Gemini     147    179    230     231
//	AWS Bedrock       356    386    465     469   ← 31 ms from being called an OUTAGE
//
// And warm, on a reused connection: OpenAI 156-215, Google 56-59, Bedrock 125-150.
// The two regimes agree with the physics. Chișinău→us-east-1 is ~8,400 km great
// circle; fibre carries light at ~2/3 c, so ~84 ms RTT is the theoretical floor
// and real routing (~1.5× great circle) puts it near 125 ms — exactly the warm
// Bedrock measurement. A COLD connection costs three of those round trips (TCP,
// then TLS, then the request), ≈ 375-450 ms — exactly the cold measurement.
//
// Both regimes are normal and both must read operational: the cacher ticks every
// 60 s and Go's default transport drops idle connections after 90 s, so the check
// alternates between warm and cold in ordinary operation.
//
// The tests below assert the CLASSIFICATION, not the rendered pill.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

/* ── The core defect: one number, two very different distances ───────────── */

// TestSameLatencyClassifiesDifferentlyByDistance is the whole fix in one
// assertion. 250 ms from a database on the same host is a real problem; 250 ms
// from a server across an ocean is a healthy Tuesday.
func TestSameLatencyClassifiesDifferentlyByDistance(t *testing.T) {
	const ms = 250
	if got := classifyLocalLatency(ms, nil); got != StatusDegraded {
		t.Errorf("local %dms = %q, want degraded — a same-host round trip measures 1-4ms", ms, got)
	}
	if got := classifyProviderLatency(ms, nil); got != StatusOperational {
		t.Errorf("provider %dms = %q, want operational — it is below the transatlantic floor", ms, got)
	}
}

/* ── Providers: a realistic transatlantic round trip is HEALTHY ──────────── */

func TestProvider_RealisticTransatlanticLatencyIsOperational(t *testing.T) {
	// Every one of these is a value actually measured from a healthy provider.
	for _, ms := range []int64{24, 115, 147, 180, 233, 300, 386, 465, 469} {
		if got := classifyProviderLatency(ms, nil); got != StatusOperational {
			t.Errorf("provider %dms = %q, want operational — this is a MEASURED healthy reading", ms, got)
		}
	}
	// The headroom above the worst measured healthy reading (469 ms): a TLS 1.2
	// handshake costs a fourth round trip (~580 ms at 145 ms/RTT), and routes
	// change. Both must stay green.
	for _, ms := range []int64{580, 700, providerHealthyMs} {
		if got := classifyProviderLatency(ms, nil); got != StatusOperational {
			t.Errorf("provider %dms = %q, want operational — still explained by geography", ms, got)
		}
	}
}

func TestProvider_GenuinelySlowIsDegraded(t *testing.T) {
	// Past the point geography can explain, something is actually wrong.
	for _, ms := range []int64{providerHealthyMs + 1, 1000, 2500, 4900} {
		if got := classifyProviderLatency(ms, nil); got != StatusDegraded {
			t.Errorf("provider %dms = %q, want degraded", ms, got)
		}
	}
}

// TestProvider_SlowIsNeverOutage: the old rule called anything over 500 ms an
// outage, with the comment "the upstream is up but unusable". For a provider
// that is plainly wrong — 600 ms is slow, not down — and it is what made a
// healthy AWS Bedrock (measured up to 469 ms) one bad routing day from being
// reported as an outage. Outage means UNREACHABLE.
func TestProvider_SlowIsNeverOutage(t *testing.T) {
	for _, ms := range []int64{501, 600, 1000, 4999, 60000} {
		if got := classifyProviderLatency(ms, nil); got == StatusOutage {
			t.Errorf("provider %dms = outage — latency alone must never mean outage; "+
				"the request completed, so the provider was reachable", ms)
		}
	}
}

func TestProvider_UnreachableIsOutage(t *testing.T) {
	if got := classifyProviderLatency(12, errors.New("dial tcp: i/o timeout")); got != StatusOutage {
		t.Errorf("unreachable provider = %q, want outage", got)
	}
}

/* ── Local components: tightened, because 100ms was never their number ───── */

func TestLocal_SameHostLatencyIsOperational(t *testing.T) {
	// Measured on the live box: PostgreSQL 4ms, Redis 1ms, NATS 1ms.
	for _, ms := range []int64{0, 1, 4, 20, localHealthyMs} {
		if got := classifyLocalLatency(ms, nil); got != StatusOperational {
			t.Errorf("local %dms = %q, want operational", ms, got)
		}
	}
}

// TestLocal_SlowIsDegradedNotHidden: under the old shared band a Postgres ping
// at 90 ms — more than twenty times its normal — reported OPERATIONAL. An
// always-green check is the same defect as an always-amber one, wearing a
// different colour.
func TestLocal_SlowIsDegradedNotHidden(t *testing.T) {
	for _, ms := range []int64{localHealthyMs + 1, 90, 250, 900} {
		if got := classifyLocalLatency(ms, nil); got != StatusDegraded {
			t.Errorf("local %dms = %q, want degraded", ms, got)
		}
	}
}

func TestLocal_SlowIsNeverOutage(t *testing.T) {
	// A genuinely hung dependency is caught by its per-check TIMEOUT, which
	// surfaces as an error → outage. Latency itself never means outage.
	for _, ms := range []int64{501, 1500, 30000} {
		if got := classifyLocalLatency(ms, nil); got == StatusOutage {
			t.Errorf("local %dms = outage — latency alone must never mean outage", ms)
		}
	}
	if got := classifyLocalLatency(3, errors.New("connection refused")); got != StatusOutage {
		t.Errorf("unreachable local component = %q, want outage", got)
	}
}

/* ── A provider erroring is worse than a provider being slow ─────────────── */

// TestProvider_ServerErrorIsOutage: the old code called a 5xx DEGRADED while
// calling a 600 ms response an OUTAGE — exactly inverted. A provider returning
// 503 to a trivial HEAD is having an incident. (All four real endpoints answer
// 4xx to a HEAD today — 401/405/404/404 — so this branch only ever fires on a
// genuine server error and cannot cry wolf.)
func TestProvider_ServerErrorIsOutage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	sp := newStatusPage(nil, nil, nil, "t")
	got := sp.checkProvider(t.Context(), "OpenAI", srv.URL)
	if got.Status != StatusOutage {
		t.Errorf("provider returning 503 = %q, want outage — it is answering, and answering that it is broken", got.Status)
	}
}

func TestProvider_ClientErrorIsOperational(t *testing.T) {
	// 401/404/405 mean "reachable, we just didn't authenticate" — which is what
	// every real provider returns to an unauthenticated HEAD.
	for _, code := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusMethodNotAllowed} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		got := sp4xx(t, srv.URL)
		srv.Close()
		if got != StatusOperational {
			t.Errorf("provider returning %d = %q, want operational", code, got)
		}
	}
}

func sp4xx(t *testing.T, url string) ComponentStatus {
	t.Helper()
	return newStatusPage(nil, nil, nil, "t").checkProvider(t.Context(), "P", url).Status
}

/* ── Whose status is this page reporting? ────────────────────────────────── */

// TestOverall_ThirdPartyProviderDoesNotDefineOurStatus. The banner answers one
// question — "is Talyvor working?" — and a third party's latency is not an
// answer to it. On the live box every Lens component was operational
// (PostgreSQL 4ms, Redis 1ms, NATS 1ms) while the banner read DEGRADED because
// OpenAI answered in 257 ms. That told a visitor something false about us.
//
// The provider table still shows each provider's real status, which is the
// accurate and actionable information: "Talyvor operational, OpenAI outage" is
// exactly what a customer whose requests are failing needs to read.
func TestOverall_ThirdPartyProviderDoesNotDefineOurStatus(t *testing.T) {
	allOK := []ComponentStatus{StatusOperational, StatusOperational}

	if got := computeOverall(allOK, []ComponentStatus{StatusDegraded}); got != StatusOperational {
		t.Errorf("overall with a degraded PROVIDER = %q, want operational — we are fine", got)
	}
	if got := computeOverall(allOK, []ComponentStatus{StatusOutage}); got != StatusOperational {
		t.Errorf("overall with a provider OUTAGE = %q, want operational — their outage is not ours", got)
	}
	// Our own components still define it, exactly as before.
	if got := computeOverall([]ComponentStatus{StatusOperational, StatusDegraded}, nil); got != StatusDegraded {
		t.Errorf("overall with a degraded COMPONENT = %q, want degraded", got)
	}
	if got := computeOverall([]ComponentStatus{StatusOutage}, nil); got != StatusOutage {
		t.Errorf("overall with a component OUTAGE = %q, want outage", got)
	}
}

/* ── The always-green check ──────────────────────────────────────────────── */

// TestProxyCheck_DoesNotPretendToBeMeasured. checkProxy returns operational
// unconditionally: if the code is running, the process is up. That is true, but
// it rendered "Proxy | operational | 0ms" in the same table as three genuinely
// probed components, and the 0ms is not a measurement — it is a placeholder
// wearing the costume of one. A check that always passes is the same defect as
// a check that always warns.
func TestProxyCheck_DoesNotPretendToBeMeasured(t *testing.T) {
	c := (&StatusPage{}).checkProxy()
	if c.Status != StatusOperational {
		t.Errorf("proxy self-check = %q, want operational (it served this request)", c.Status)
	}
	if c.Measured {
		t.Error("the proxy self-check must not claim to be measured — it probes nothing")
	}
	if c.Message == "" {
		t.Error("an unmeasured check must say what it actually asserts")
	}
	// And the probed checks must claim the opposite, or the flag is meaningless.
	probed := newTestPage(t, &fakePinger{}).checkPostgres(t.Context())
	if !probed.Measured {
		t.Error("a real probe must be marked measured")
	}
}

// TestHTML_UnmeasuredLatencyIsNotRenderedAsZero: "0ms" reads as "we measured
// zero", which is a claim. An unmeasured row must show a dash.
func TestHTML_UnmeasuredLatencyIsNotRenderedAsZero(t *testing.T) {
	html := renderHTML(&StatusResponse{
		Status: StatusOperational,
		Components: []Component{
			{Name: "Proxy", Status: StatusOperational, Latency: 0, Measured: false},
			{Name: "Redis", Status: StatusOperational, Latency: 0, Measured: true},
		},
	})
	if strings.Count(html, "<td>0ms</td>") != 1 {
		t.Errorf("expected exactly one real 0ms cell (the measured Redis), got:\n%s", html)
	}
	if !strings.Contains(html, "<td>—</td>") {
		t.Error("the unmeasured row must render a dash, not a fabricated 0ms")
	}
}
