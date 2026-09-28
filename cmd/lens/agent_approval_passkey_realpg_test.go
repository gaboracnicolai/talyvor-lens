package main

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/webpush"
)

// B19.16 — once the owner has a passkey, an approve or deny without a valid assertion for that approval
// is refused and changes nothing, one with it succeeds once; and filing an approval posts one encrypted
// push per subscription, which decrypts to the agent, the amount and the reason.
func TestAgentRoutes_ApprovalsAreSignedWithAPasskeyAndPushedToTheOwner(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-agents", "app.talyvor.com", "https://app.talyvor.com"
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 20000000, 20000000)`, ws); err != nil {
		t.Fatal(err)
	}

	// A push service that records what it is sent.
	type push struct {
		path, authz string
		body        []byte
	}
	var mu sync.Mutex
	var pushes []push
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		pushes = append(pushes, push{r.URL.Path, r.Header.Get("Authorization"), body})
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(service.Close)
	vapidKey, vapidPublic, err := webpush.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	sender, err := webpush.NewSender(vapidKey, "mailto:ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	sender.AllowEndpoint = func(u *url.URL) bool { return strings.HasPrefix(u.String(), service.URL) }

	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, sender)
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(), owner))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	field := func(body, name string) string {
		var m map[string]any
		_ = json.Unmarshal([]byte(body), &m)
		s, _ := m[name].(string)
		return s
	}
	b64 := base64.RawURLEncoding.EncodeToString
	base := "/v1/workspaces/" + ws + "/agents"

	// Two of the owner's devices subscribe.
	if code, body := call(http.MethodGet, base+"/push/public-key", ""); code != http.StatusOK || field(body, "public_key") != vapidPublic {
		t.Fatalf("push public key = %d %s", code, body)
	}
	devices := map[string]*ecdh.PrivateKey{}
	secrets := map[string][]byte{}
	for _, dev := range []string{"/phone", "/laptop"} {
		k, _ := ecdh.P256().GenerateKey(rand.Reader)
		secret := make([]byte, 16)
		_, _ = rand.Read(secret)
		devices[dev], secrets[dev] = k, secret
		sub := `{"endpoint":"` + service.URL + dev + `","keys":{"p256dh":"` + b64(k.PublicKey().Bytes()) + `","auth":"` + b64(secret) + `"}}`
		if code, body := call(http.MethodPost, base+"/push/subscriptions", sub); code != http.StatusCreated {
			t.Fatalf("subscribe %s = %d %s", dev, code, body)
		}
	}

	// The owner registers a passkey: this test is the authenticator.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	credID := make([]byte, 16)
	_, _ = rand.Read(credID)
	rpHash := sha256.Sum256([]byte(rpID))
	authData := func(flags byte, count uint32, attested bool) []byte {
		d := append(append([]byte{}, rpHash[:]...), flags)
		d = binary.BigEndian.AppendUint32(d, count)
		if attested {
			d = append(d, make([]byte, 16)...) // AAGUID
			d = binary.BigEndian.AppendUint16(d, uint16(len(credID)))
			d = append(append(d, credID...), 0xa0) // the COSE key, which Lens does not read
		}
		return d
	}
	clientData := func(typ, challenge string) []byte {
		return []byte(`{"type":"` + typ + `","challenge":"` + challenge + `","origin":"` + origin + `","crossOrigin":false}`)
	}
	_, body := call(http.MethodPost, base+"/passkeys/challenge", "")
	reg := map[string]string{"credential_id": b64(credID), "name": "Nicolai's iPhone", "public_key": b64(spki),
		"client_data_json": b64(clientData("webauthn.create", field(body, "challenge"))), "authenticator_data": b64(authData(0x45, 0, true))}
	regBody, _ := json.Marshal(reg)
	if code, body := call(http.MethodPost, base+"/passkeys", string(regBody)); code != http.StatusCreated {
		t.Fatalf("register passkey = %d %s", code, body)
	}

	// An agent asks for two approvals. Each is pushed to both devices, once.
	agent, err := store.CreateAgent(ctx, ws, "researcher", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	payee, err := store.CreateAgent(ctx, ws, "vendor", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAgentRules(ctx, ws, agent.ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	first, err := store.RequestPaymentApproval(ctx, ws, agent.ID, payee.ID, 2_000_000, "invoice 7", "October hosting")
	if err != nil {
		t.Fatal(err)
	}
	waitPushes := func(n int) []push {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			mu.Lock()
			got := append([]push{}, pushes...)
			mu.Unlock()
			if len(got) >= n {
				return got
			}
		}
		t.Fatalf("fewer than %d pushes arrived", n)
		return nil
	}
	seen := map[string]bool{}
	for _, p := range waitPushes(2) {
		plain, err := webpush.Decrypt(p.body, devices[p.path], secrets[p.path])
		if err != nil {
			t.Fatalf("the push to %s does not decrypt: %v", p.path, err)
		}
		var msg map[string]any
		_ = json.Unmarshal(plain, &msg)
		if msg["agent_name"] != "researcher" || msg["amount_ulxc"] != float64(2_000_000) || msg["reason"] != "October hosting" || msg["approval_id"] != first.ID {
			t.Errorf("the push to %s says %s", p.path, plain)
		}
		if !strings.HasPrefix(p.authz, "vapid t=") || !strings.Contains(p.authz, "k="+vapidPublic) {
			t.Errorf("the push to %s is not VAPID-signed: %q", p.path, p.authz)
		}
		seen[p.path] = true
	}
	if !seen["/phone"] || !seen["/laptop"] {
		t.Errorf("the pushes went to %v, want one to each device", seen)
	}
	second, err := store.RequestPaymentApproval(ctx, ws, agent.ID, payee.ID, 3_000_000, "invoice 8", "November hosting")
	if err != nil {
		t.Fatal(err)
	}
	waitPushes(4)

	sign := func(challenge string, count uint32) string {
		cdj := clientData("webauthn.get", challenge)
		ad := authData(0x05, count, false)
		h := sha256.Sum256(cdj)
		signed := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, signed[:])
		a, _ := json.Marshal(map[string]any{"assertion": map[string]string{"credential_id": b64(credID),
			"client_data_json": b64(cdj), "authenticator_data": b64(ad), "signature": b64(sig)}})
		return string(a)
	}
	challengeFor := func(id string) string {
		t.Helper()
		code, body := call(http.MethodPost, base+"/approvals/"+id+"/challenge", "")
		if code != http.StatusOK || !strings.Contains(body, b64(credID)) {
			t.Fatalf("challenge for %s = %d %s", id, code, body)
		}
		return field(body, "challenge")
	}
	status := func(id string) string {
		t.Helper()
		list, err := store.ListAgentApprovals(ctx, ws)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range list {
			if a.ID == id {
				return a.Status
			}
		}
		return ""
	}
	approve := base + "/approvals/" + first.ID + "/approve"

	// Refused, and nothing changes: no assertion; an assertion over the other approval's challenge; one
	// signed by a key that is not the workspace's.
	if code, body := call(http.MethodPost, approve, `{}`); code != http.StatusForbidden || !strings.Contains(body, "passkey") {
		t.Errorf("approve with no assertion = %d %s, want 403", code, body)
	}
	if code, body := call(http.MethodPost, approve, sign(challengeFor(second.ID), 1)); code != http.StatusForbidden {
		t.Errorf("approve with the other approval's challenge = %d %s, want 403", code, body)
	}
	realKey := key
	key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if code, body := call(http.MethodPost, approve, sign(challengeFor(first.ID), 1)); code != http.StatusForbidden {
		t.Errorf("approve signed by another key = %d %s, want 403", code, body)
	}
	key = realKey
	if s := status(first.ID); s != "pending" {
		t.Fatalf("after three refused decisions the approval is %q, want pending", s)
	}

	// With a valid assertion it is approved — once: the same assertion cannot decide again.
	valid := sign(challengeFor(first.ID), 1)
	if code, body := call(http.MethodPost, approve, valid); code != http.StatusOK || status(first.ID) != "approved" {
		t.Fatalf("approve with a valid assertion = %d %s", code, body)
	}
	if code, body := call(http.MethodPost, base+"/approvals/"+second.ID+"/deny", valid); code != http.StatusForbidden || status(second.ID) != "pending" {
		t.Errorf("the same assertion replayed to deny another = %d %s, want 403 and it still pending", code, body)
	}
	if code, body := call(http.MethodPost, base+"/approvals/"+second.ID+"/deny", sign(challengeFor(second.ID), 2)); code != http.StatusOK || status(second.ID) != "denied" {
		t.Errorf("deny with a valid assertion = %d %s", code, body)
	}
}
