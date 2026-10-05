package fees

import (
	"reflect"
	"testing"
)

// B32.8 — every fee defaults to Nicolai's value, and a setting in lens.env replaces it.
func TestLoad_DefaultsAreNicolaisValuesAndASettingReplacesOne(t *testing.T) {
	s, err := Load(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{
		MarketTakeBPS: 1500, ServicesTakeBPS: 500, ComputeTakeBPS: 500, LendingFeeBPS: 100,
		PlatformFeeBPS:      map[string]int64{"free": 550, "team": 300, "business": 100, "enterprise": 100},
		FXMarginBPS:         map[string]int64{"free": 0, "team": 50, "business": 25, "enterprise": 15},
		IntlPaymentFeeMinor: map[string]int64{"GBP": 500, "EUR": 600, "USD": 700},
		MerchantFeeBPS:      75, MerchantA2AFeeBPS: 100,
	}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("defaults = %+v, want %+v", s, want)
	}

	env := map[string]string{"LENS_MARKET_TAKE_BPS": "1200", "LENS_FX_MARGIN_BPS": `{"team":40}`}
	s, err = Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if s.MarketTakeBPS != 1200 || !reflect.DeepEqual(s.FXMarginBPS, map[string]int64{"team": 40}) || s.ServicesTakeBPS != 500 {
		t.Fatalf("with settings: %+v", s)
	}

	for k, v := range map[string]string{
		"LENS_MARKET_TAKE_BPS":        "15%",
		"LENS_SERVICES_TAKE_BPS":      "10001",
		"LENS_PLATFORM_FEE_BPS":       `{"free":-1}`,
		"LENS_INTL_PAYMENT_FEE_MINOR": `500`,
	} {
		if _, err := Load(func(n string) string {
			if n == k {
				return v
			}
			return ""
		}); err == nil {
			t.Errorf("%s=%s loaded; it must refuse to start", k, v)
		}
	}
}
