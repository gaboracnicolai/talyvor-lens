package pii

import (
	"slices"
	"strings"
	"testing"
)

func TestDetector_EmailRedacted(t *testing.T) {
	d := New()
	r := d.Detect("Contact me at john.doe+tag@example.com please.")

	if !r.WasRedacted {
		t.Fatalf("expected WasRedacted=true; got result: %+v", r)
	}
	if !strings.Contains(r.Redacted, "[REDACTED-EMAIL]") {
		t.Errorf("Redacted = %q, want it to contain [REDACTED-EMAIL]", r.Redacted)
	}
	if strings.Contains(r.Redacted, "john.doe+tag@example.com") {
		t.Errorf("Redacted still contains the email: %q", r.Redacted)
	}
	if !slices.Contains(r.Types, "email") {
		t.Errorf("Types = %v, want it to include 'email'", r.Types)
	}
}

func TestDetector_PhoneRedacted(t *testing.T) {
	d := New()

	cases := []string{
		"Call me at +1-555-555-5555 today",
		"Or at (555) 555-5555",
		"Or 555.555.5555",
		"International: +44 20 7946 0958",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			r := d.Detect(in)
			if !r.WasRedacted {
				t.Fatalf("expected WasRedacted=true for %q; got %+v", in, r)
			}
			if !strings.Contains(r.Redacted, "[REDACTED-PHONE]") {
				t.Errorf("Redacted = %q, want it to contain [REDACTED-PHONE]", r.Redacted)
			}
			if !slices.Contains(r.Types, "phone") {
				t.Errorf("Types = %v, want it to include 'phone'", r.Types)
			}
		})
	}
}

func TestDetector_CreditCardRedacted(t *testing.T) {
	d := New()
	for _, in := range []string{
		"My card is 4111-1111-1111-1111 ok",
		"Card: 4111 1111 1111 1111",
		"Card: 4111111111111111",
	} {
		t.Run(in, func(t *testing.T) {
			r := d.Detect(in)
			if !r.WasRedacted || !strings.Contains(r.Redacted, "[REDACTED-CARD]") {
				t.Errorf("Detect(%q) = %+v, want [REDACTED-CARD]", in, r)
			}
			if !slices.Contains(r.Types, "credit_card") {
				t.Errorf("Types = %v, want 'credit_card'", r.Types)
			}
		})
	}
}

func TestDetector_SSNRedacted(t *testing.T) {
	d := New()
	r := d.Detect("SSN is 123-45-6789 confirmed")

	if !r.WasRedacted || !strings.Contains(r.Redacted, "[REDACTED-SSN]") {
		t.Errorf("result = %+v, want [REDACTED-SSN]", r)
	}
	if !slices.Contains(r.Types, "ssn") {
		t.Errorf("Types = %v, want 'ssn'", r.Types)
	}
}

func TestDetector_IPAddressRedacted(t *testing.T) {
	d := New()
	r := d.Detect("Their server is at 192.168.1.42 right now")

	if !r.WasRedacted || !strings.Contains(r.Redacted, "[REDACTED-IP]") {
		t.Errorf("result = %+v, want [REDACTED-IP]", r)
	}
	if !slices.Contains(r.Types, "ip_address") {
		t.Errorf("Types = %v, want 'ip_address'", r.Types)
	}
}

func TestDetector_CleanTextNotRedacted(t *testing.T) {
	d := New()
	r := d.Detect("Hello world, this is fine.")

	if r.WasRedacted {
		t.Errorf("WasRedacted should be false on clean text; got result: %+v", r)
	}
	if r.Redacted != "Hello world, this is fine." {
		t.Errorf("Redacted = %q, want unchanged original", r.Redacted)
	}
	if len(r.Types) != 0 {
		t.Errorf("Types = %v, want empty slice", r.Types)
	}
}

func TestDetector_MultipleTypesAllDetected(t *testing.T) {
	d := New()
	r := d.Detect("Email john@example.com, phone 555-555-5555, SSN 123-45-6789, IP 10.0.0.1")

	if !r.WasRedacted {
		t.Fatal("expected WasRedacted=true")
	}
	for _, want := range []string{"email", "phone", "ssn", "ip_address"} {
		if !slices.Contains(r.Types, want) {
			t.Errorf("Types = %v, want it to include %q", r.Types, want)
		}
	}
	// Sanity: none of the originals leak through.
	for _, leaked := range []string{"john@example.com", "555-555-5555", "123-45-6789", "10.0.0.1"} {
		if strings.Contains(r.Redacted, leaked) {
			t.Errorf("Redacted = %q, still contains %q", r.Redacted, leaked)
		}
	}
}

func TestDetector_IsSafeToCacheFalseWhenPII(t *testing.T) {
	d := New()
	r := d.Detect("Email me at someone@example.com")
	if d.IsSafeToCache(r) {
		t.Error("IsSafeToCache should be false when PII is present")
	}
}

func TestDetector_IsSafeToCacheTrueWhenClean(t *testing.T) {
	d := New()
	r := d.Detect("Hello world")
	if !d.IsSafeToCache(r) {
		t.Error("IsSafeToCache should be true on clean text")
	}
}

func TestDetector_TypesDeduplicated(t *testing.T) {
	d := New()
	r := d.Detect("Emails: john@a.com and jane@b.com and bob@c.com")

	emailCount := 0
	for _, ty := range r.Types {
		if ty == "email" {
			emailCount++
		}
	}
	if emailCount != 1 {
		t.Errorf("Types %v contains 'email' %d times, want exactly 1", r.Types, emailCount)
	}
}
