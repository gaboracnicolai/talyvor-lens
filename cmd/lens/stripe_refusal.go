package main

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/stripe/stripe-go/v81"
)

// B26.12 — when Stripe refuses what was asked of it (a card error, or an invalid request: a cardholder's last
// name with a digit in it, a country Connect does not serve), the answer is 400 with Stripe's own sentence, so
// the screen says why instead of "Nothing changed". Everything else Stripe answers stays 502 — Lens's own key
// (401/403), a clash (409), slow down (429), an object Lens named that is gone (404), Stripe down — since
// nothing the person typed would change it.
//
// B28.276 — Stripe's v2 API (a seller's account and onboarding link, B17.23) refuses with a code and a message
// and no v1 type, so stripe-go v81 reads it with an empty Type. That is the same refusal: before this, every
// Connect with Stripe Stripe turned down was a 502 and the seller read "Nothing happened".

// stripeToken is something in Stripe's sentence that may be an object id (ich_1Nv…) or a key (sk_test_…),
// which never reaches the screen; a param name (tos_acceptance) is told apart by having no digit or capital.
var stripeToken = regexp.MustCompile(`\b[a-z]{2,8}_[A-Za-z0-9_*]{10,}`)

// stripeRefusal is Stripe's reason, ids and keys taken out, when err is Stripe refusing the request for what
// it asked; "" when it is not.
func stripeRefusal(err error) string {
	var se *stripe.Error
	if !errors.As(err, &se) || se.Msg == "" {
		return ""
	}
	if se.HTTPStatusCode != http.StatusBadRequest && se.HTTPStatusCode != http.StatusPaymentRequired {
		return ""
	}
	if se.Type != stripe.ErrorTypeInvalidRequest && se.Type != stripe.ErrorTypeCard && se.Type != "" {
		return ""
	}
	return "Stripe says: " + stripeToken.ReplaceAllStringFunc(se.Msg, func(tok string) string {
		if strings.ContainsAny(tok, "0123456789*") || tok != strings.ToLower(tok) {
			return "…"
		}
		return tok
	})
}
