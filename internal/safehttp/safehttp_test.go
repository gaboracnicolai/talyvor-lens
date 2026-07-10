package safehttp

import (
	"net"
	"testing"
)

// The classifier is the crux of the SSRF guard: every internal/sensitive range must be blocked
// (especially the cloud metadata endpoint 169.254.169.254), and public addresses allowed.
func TestBlockedClassifier(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"169.254.169.254", true}, // cloud metadata — the crown-jewel SSRF target
		{"127.0.0.1", true},       // loopback (the Lens server itself)
		{"::1", true},             // loopback v6
		{"10.0.0.5", true},        // RFC1918
		{"172.16.3.4", true},      // RFC1918
		{"192.168.1.1", true},     // RFC1918
		{"fd00::1", true},         // ULA
		{"fe80::1", true},         // link-local v6
		{"0.0.0.0", true},         // unspecified
		{"8.8.8.8", false},        // public — must be allowed
		{"1.1.1.1", false},        // public
		{"93.184.216.34", false},  // public (example.com)
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", c.ip)
		}
		if got := blocked(ip); got != c.want {
			t.Errorf("blocked(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}
