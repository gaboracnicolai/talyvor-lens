package safehttp

import (
	"net"
	"testing"
)

// Phase-0 Item E — operator CIDR allowlist for the internal (private) node network.

func mustCIDRs(t *testing.T, csv string) []*net.IPNet {
	t.Helper()
	nets, err := ParseCIDRAllowlist(csv)
	if err != nil {
		t.Fatalf("ParseCIDRAllowlist(%q): %v", csv, err)
	}
	return nets
}

func mustParseNet(cidr string) *net.IPNet {
	_, n, _ := net.ParseCIDR(cidr)
	return n
}

// (1) Empty allowlist → today's behavior EXACTLY: a private IP is blocked.
func TestAllowlist_EmptyBlocksPrivate(t *testing.T) {
	if !blocked(net.ParseIP("10.0.0.5"), nil) {
		t.Error("empty allowlist must block a private IP (today's behavior preserved)")
	}
	if !blocked(net.ParseIP("192.168.1.10"), mustCIDRs(t, "")) {
		t.Error("empty-string allowlist must block a private IP")
	}
}

// (2) RED: an allowlisted private CIDR is reachable (NOT blocked). A private IP
// outside every allowlisted CIDR stays blocked.
func TestAllowlist_AllowedPrivateCIDRReachable(t *testing.T) {
	allow := mustCIDRs(t, "10.0.0.0/8, 192.168.0.0/16")
	if blocked(net.ParseIP("10.0.0.5"), allow) {
		t.Error("an allowlisted private IP (10.0.0.5 ∈ 10.0.0.0/8) must be REACHABLE")
	}
	if blocked(net.ParseIP("192.168.1.10"), allow) {
		t.Error("an allowlisted private IP (192.168.1.10 ∈ 192.168.0.0/16) must be REACHABLE")
	}
	if !blocked(net.ParseIP("172.16.0.1"), allow) {
		t.Error("a private IP in NO allowlisted CIDR must stay blocked")
	}
}

// (3) SECURITY INVARIANT: metadata / link-local / loopback are blocked in BOTH
// cases — never allowlistable (checked before the allowlist branch).
func TestAllowlist_MetadataAlwaysBlocked(t *testing.T) {
	meta := net.ParseIP("169.254.169.254")
	if !blocked(meta, nil) {
		t.Error("metadata must be blocked with no allowlist")
	}
	linkLocal := []*net.IPNet{mustParseNet("169.254.0.0/16")}
	if !blocked(meta, linkLocal) {
		t.Error("INVARIANT VIOLATED: metadata must stay blocked even if 169.254/16 is (wrongly) in the allowlist")
	}
	if !blocked(net.ParseIP("127.0.0.1"), linkLocal) {
		t.Error("loopback must never be allowlistable")
	}
	if !blocked(net.ParseIP("fe80::1"), linkLocal) {
		t.Error("fe80::/10 link-local must never be allowlistable")
	}
}

// (4) ParseCIDRAllowlist refuses a CIDR containing the metadata endpoint (fail-loud).
func TestParseCIDRAllowlist_RefusesMetadata(t *testing.T) {
	if _, err := ParseCIDRAllowlist("169.254.0.0/16"); err == nil {
		t.Error("a CIDR containing 169.254.169.254 must be REFUSED at parse time")
	}
	if _, err := ParseCIDRAllowlist("169.254.169.254/32"); err == nil {
		t.Error("the metadata /32 must be REFUSED")
	}
	if _, err := ParseCIDRAllowlist("10.0.0.0/8"); err != nil {
		t.Errorf("a legit private CIDR must parse: %v", err)
	}
	if _, err := ParseCIDRAllowlist("not-a-cidr"); err == nil {
		t.Error("an invalid CIDR must error")
	}
}
