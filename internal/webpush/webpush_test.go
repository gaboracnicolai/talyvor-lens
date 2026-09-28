package webpush

import (
	"crypto/ecdh"
	"crypto/rand"
	"testing"
)

// RFC 8291 Appendix A: the specification's own message decrypts, so the key schedule is the one every
// browser's push service uses; and what Encrypt produces decrypts back.
func TestTheRFC8291ExampleDecrypts_AndEncryptRoundTrips(t *testing.T) {
	priv, _ := Decode64("q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94")
	auth, _ := Decode64("BTBZMqHH6r4Tts7J_aSIgg")
	body, _ := Decode64("DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN")
	ua, err := ecdh.P256().NewPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Decrypt(body, ua, auth); err != nil || string(got) != "When I grow up, I want to be a watermelon" {
		t.Fatalf("the RFC 8291 example decrypts to %q, %v", got, err)
	}

	mine, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	_, _ = rand.Read(secret)
	enc, err := Encrypt([]byte(`{"agent":"researcher"}`), mine.PublicKey().Bytes(), secret)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Decrypt(enc, mine, secret); err != nil || string(got) != `{"agent":"researcher"}` {
		t.Fatalf("round trip = %q, %v", got, err)
	}
}
