package compat

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureHandler records the headers and path the inner handler saw
// after the middleware processed the request.
type captureHandler struct {
	headers http.Header
	path    string
}

func (c *captureHandler) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	c.headers = r.Header.Clone()
	c.path = r.URL.Path
}

func wrap(t *testing.T) (*captureHandler, http.Handler) {
	t.Helper()
	cap := &captureHandler{}
	h := NewHeliconeCompat(nil)
	return cap, h.Middleware()(cap)
}

func TestHelicone_AuthMappedToAuthorization(t *testing.T) {
	cap, mw := wrap(t)
	req := httptest.NewRequest(http.MethodPost, "/oai/v1/chat/completions", nil)
	req.Header.Set("Helicone-Auth", "Bearer tlv_test_key")
	mw.ServeHTTP(httptest.NewRecorder(), req)

	if got := cap.headers.Get("Authorization"); got != "Bearer tlv_test_key" {
		t.Errorf("Authorization = %q, want Bearer tlv_test_key", got)
	}
}

func TestHelicone_AuthRemovedFromForwardedRequest(t *testing.T) {
	cap, mw := wrap(t)
	req := httptest.NewRequest(http.MethodPost, "/oai/v1/chat/completions", nil)
	req.Header.Set("Helicone-Auth", "Bearer tlv_x")
	mw.ServeHTTP(httptest.NewRecorder(), req)

	if got := cap.headers.Get("Helicone-Auth"); got != "" {
		t.Errorf("Helicone-Auth = %q, want empty (must be stripped before upstream)", got)
	}
}

func TestHelicone_UserIdMappedToSessionHeader(t *testing.T) {
	cap, mw := wrap(t)
	req := httptest.NewRequest(http.MethodPost, "/oai/x", nil)
	req.Header.Set("Helicone-User-Id", "user-42")
	mw.ServeHTTP(httptest.NewRecorder(), req)

	if got := cap.headers.Get("X-Talyvor-Session"); got != "user-42" {
		t.Errorf("X-Talyvor-Session = %q, want user-42", got)
	}
	if got := cap.headers.Get("Helicone-User-Id"); got != "" {
		t.Errorf("Helicone-User-Id should be stripped after translation; got %q", got)
	}
}

func TestHelicone_PropertyMappedToFeatureHeader(t *testing.T) {
	cap, mw := wrap(t)
	req := httptest.NewRequest(http.MethodPost, "/oai/x", nil)
	req.Header.Set("Helicone-Property-Feature", "search")
	mw.ServeHTTP(httptest.NewRecorder(), req)

	if got := cap.headers.Get("X-Talyvor-Feature"); got != "search" {
		t.Errorf("X-Talyvor-Feature = %q, want search", got)
	}
}

func TestHelicone_OaiPathRewrittenToProxyOpenai(t *testing.T) {
	cap, mw := wrap(t)
	req := httptest.NewRequest(http.MethodPost, "/oai/v1/chat/completions", nil)
	mw.ServeHTTP(httptest.NewRecorder(), req)

	const want = "/v1/proxy/openai/v1/chat/completions"
	if cap.path != want {
		t.Errorf("path = %q, want %q", cap.path, want)
	}
}

func TestHelicone_AnthropicPathRewrittenToProxyAnthropic(t *testing.T) {
	cap, mw := wrap(t)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	mw.ServeHTTP(httptest.NewRecorder(), req)

	const want = "/v1/proxy/anthropic/v1/messages"
	if cap.path != want {
		t.Errorf("path = %q, want %q", cap.path, want)
	}
}

func TestHelicone_PassThroughWhenNoHeliconeHeaders(t *testing.T) {
	cap, mw := wrap(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer tlv_x")
	mw.ServeHTTP(httptest.NewRecorder(), req)

	// Path unchanged when there's no Helicone-shaped prefix.
	if cap.path != "/v1/proxy/openai/v1/chat/completions" {
		t.Errorf("non-Helicone path mutated: %q", cap.path)
	}
	// Existing Authorization preserved.
	if got := cap.headers.Get("Authorization"); got != "Bearer tlv_x" {
		t.Errorf("Authorization mutated: %q", got)
	}
	// No accidental X-Talyvor-* headers.
	for _, h := range []string{"X-Talyvor-Session", "X-Talyvor-Feature"} {
		if got := cap.headers.Get(h); got != "" {
			t.Errorf("%s = %q, expected empty when no Helicone headers were present", h, got)
		}
	}
}

func TestHelicone_CacheAndRetryHeadersStripped(t *testing.T) {
	cap, mw := wrap(t)
	req := httptest.NewRequest(http.MethodPost, "/oai/x", nil)
	req.Header.Set("Helicone-Cache-Enabled", "true")
	req.Header.Set("Helicone-Retry-Enabled", "true")
	mw.ServeHTTP(httptest.NewRecorder(), req)

	// Cache + retry are always-on in Lens; the headers must not be
	// forwarded to upstream where they'd confuse a real OpenAI client.
	if got := cap.headers.Get("Helicone-Cache-Enabled"); got != "" {
		t.Errorf("Helicone-Cache-Enabled should be stripped; got %q", got)
	}
	if got := cap.headers.Get("Helicone-Retry-Enabled"); got != "" {
		t.Errorf("Helicone-Retry-Enabled should be stripped; got %q", got)
	}
}
