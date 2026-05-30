package proxy

import (
	"net/http"
	"testing"
)

// TestIsAutoRoute is the gate that guarantees an explicit model is never
// overridden by routing intelligence: only the "auto" pseudo-model or an
// X-Talyvor-Auto-Route header cedes the choice. Anything else is pinned.
func TestIsAutoRoute(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		header string
		want   bool
	}{
		{"concrete model is pinned", "gpt-4o", "", false},
		{"concrete model with empty header is pinned", "claude-sonnet-4-6", "", false},
		{"auto pseudo-model cedes choice", "auto", "", true},
		{"AUTO is case-insensitive", "AUTO", "", true},
		{"header true cedes choice", "gpt-4o", "true", true},
		{"header 1 cedes choice", "gpt-4o", "1", true},
		{"header on cedes choice", "gpt-4o", "on", true},
		{"header false stays pinned", "gpt-4o", "false", false},
		{"header garbage stays pinned", "gpt-4o", "maybe", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if c.header != "" {
				r.Header.Set("X-Talyvor-Auto-Route", c.header)
			}
			if got := isAutoRoute(r, c.model); got != c.want {
				t.Fatalf("isAutoRoute(%q, header=%q) = %v, want %v", c.model, c.header, got, c.want)
			}
		})
	}
}
