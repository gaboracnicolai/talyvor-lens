package config

import (
	"reflect"
	"testing"
)

// B13.1 — LENS_BILLING_SUBSCRIPTION_PLANS: "name=price_id" pairs; malformed entries are skipped.
func TestParsePlans(t *testing.T) {
	got := parsePlans(" plus=price_a, pro = price_b ,max=,=price_c,junk,, max=price_d")
	want := map[string]string{"plus": "price_a", "pro": "price_b", "max": "price_d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsePlans = %v, want %v", got, want)
	}
}
