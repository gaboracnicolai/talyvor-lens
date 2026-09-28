// Package webpush sends Web Push messages: the payload encrypted to the subscription (RFC 8291,
// aes128gcm content coding, RFC 8188) and the request signed with the server's VAPID key (RFC 8292).
//
// B19.16 uses it to tell a workspace's owner, on each device they subscribed, that an agent's request is
// waiting for their approval.
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// recordSize is the RFC 8188 record size: one record carries the whole message.
const recordSize = 4096

// ErrGone: the push service says the subscription no longer exists; forget it.
var ErrGone = errors.New("webpush: the subscription is gone")

var b64 = base64.RawURLEncoding

// Decode64 reads base64url with or without padding, as browsers hand subscriptions over.
func Decode64(s string) ([]byte, error) {
	return b64.DecodeString(strings.TrimRight(s, "="))
}

// Encrypt encrypts payload to a subscription's P-256 public key (65 bytes, uncompressed) and its 16-byte
// auth secret, as one aes128gcm record (RFC 8291 §3.4).
func Encrypt(payload, uaPublic, authSecret []byte) ([]byte, error) {
	if len(payload) > recordSize-103 { // the header (86) and the tag (16) and the delimiter (1) share the record
		return nil, errors.New("webpush: payload too large")
	}
	curve := ecdh.P256()
	ua, err := curve.NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("webpush: the subscription's key: %w", err)
	}
	as, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	asPublic := as.PublicKey().Bytes()
	cek, nonce, err := contentKeys(shared, authSecret, uaPublic, asPublic, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 0, 86)
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, recordSize)
	header = append(header, byte(len(asPublic)))
	header = append(header, asPublic...)
	return gcm.Seal(header, nonce, append(append([]byte{}, payload...), 0x02), nil), nil
}

// contentKeys derives the content-encryption key and nonce (RFC 8291 §3.3–3.4, RFC 8188 §2.2–2.3).
func contentKeys(shared, authSecret, uaPublic, asPublic, salt []byte) (cek, nonce []byte, err error) {
	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, nil, err
	}
	ikm, err := hkdf.Expand(sha256.New, prkKey, "WebPush: info\x00"+string(uaPublic)+string(asPublic), 32)
	if err != nil {
		return nil, nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, err
	}
	if cek, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16); err != nil {
		return nil, nil, err
	}
	nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	return cek, nonce, err
}

// Decrypt is Encrypt's inverse, for the receiving side: uaPrivate is the subscription's private key.
func Decrypt(body []byte, uaPrivate *ecdh.PrivateKey, authSecret []byte) ([]byte, error) {
	if len(body) < 21 {
		return nil, errors.New("webpush: too short")
	}
	salt, idLen := body[:16], int(body[20])
	if len(body) < 21+idLen {
		return nil, errors.New("webpush: too short")
	}
	asPublic, ciphertext := body[21:21+idLen], body[21+idLen:]
	as, err := ecdh.P256().NewPublicKey(asPublic)
	if err != nil {
		return nil, err
	}
	shared, err := uaPrivate.ECDH(as)
	if err != nil {
		return nil, err
	}
	cek, nonce, err := contentKeys(shared, authSecret, uaPrivate.PublicKey().Bytes(), asPublic, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndexByte(plain, 0x02)
	if i < 0 || len(bytes.Trim(plain[i+1:], "\x00")) != 0 {
		return nil, errors.New("webpush: no record delimiter")
	}
	return plain[:i], nil
}

// pushServices are the hosts a subscription may name: the browsers' push services. Lens sends to
// nothing else, so a subscription cannot point it at a host of the operator's own network.
var pushServices = []string{"fcm.googleapis.com", "updates.push.services.mozilla.com", "push.services.mozilla.com",
	".push.apple.com", ".notify.windows.com"}

// PushServiceEndpoint reports whether endpoint is an https URL of a known push service.
func PushServiceEndpoint(u *url.URL) bool {
	if u.Scheme != "https" || u.Port() != "" && u.Port() != "443" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, s := range pushServices {
		if host == s || strings.HasPrefix(s, ".") && strings.HasSuffix(host, s) {
			return true
		}
	}
	return false
}

// Sender signs and sends pushes with one VAPID key.
type Sender struct {
	key     *ecdsa.PrivateKey
	subject string
	client  *http.Client
	// AllowEndpoint decides which endpoints may be sent to; PushServiceEndpoint unless a test says otherwise.
	AllowEndpoint func(*url.URL) bool
}

// GenerateKey makes a VAPID key pair: the private key to keep (LENS_VAPID_PRIVATE_KEY) and the public key
// browsers subscribe with, both base64url.
func GenerateKey() (private, public string, err error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	raw, err := k.Bytes()
	if err != nil {
		return "", "", err
	}
	pub, err := k.PublicKey.Bytes()
	if err != nil {
		return "", "", err
	}
	return b64.EncodeToString(raw), b64.EncodeToString(pub), nil
}

// NewSender reads a base64url VAPID private key (the raw 32-byte P-256 scalar). subject is the contact
// the push services may use, a mailto: or https: URL.
func NewSender(privateKey, subject string) (*Sender, error) {
	raw, err := Decode64(privateKey)
	if err != nil {
		return nil, fmt.Errorf("webpush: the VAPID key is not base64url: %w", err)
	}
	k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	if err != nil {
		return nil, fmt.Errorf("webpush: the VAPID key: %w", err)
	}
	return &Sender{key: k, subject: subject, client: &http.Client{Timeout: 15 * time.Second}, AllowEndpoint: PushServiceEndpoint}, nil
}

// PublicKey is the VAPID public key, base64url, the applicationServerKey a browser subscribes with.
func (s *Sender) PublicKey() string {
	pub, _ := s.key.PublicKey.Bytes()
	return b64.EncodeToString(pub)
}

// vapid is the Authorization header for endpoint (RFC 8292 §2–3).
func (s *Sender) vapid(endpoint *url.URL) (string, error) {
	claims, err := json.Marshal(map[string]any{"aud": endpoint.Scheme + "://" + endpoint.Host,
		"exp": time.Now().Add(12 * time.Hour).Unix(), "sub": s.subject})
	if err != nil {
		return "", err
	}
	input := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + b64.EncodeToString(claims)
	h := sha256.Sum256([]byte(input))
	r, sig, err := ecdsa.Sign(rand.Reader, s.key, h[:])
	if err != nil {
		return "", err
	}
	raw := make([]byte, 64)
	r.FillBytes(raw[:32])
	sig.FillBytes(raw[32:])
	return "vapid t=" + input + "." + b64.EncodeToString(raw) + ", k=" + s.PublicKey(), nil
}

// Allows reports whether endpoint may be sent to.
func (s *Sender) Allows(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && s.AllowEndpoint(u)
}

// Send encrypts payload to the subscription and posts it. ErrGone means the subscription should be forgotten.
func (s *Sender) Send(ctx context.Context, endpoint, p256dh, auth string, payload []byte) error {
	u, err := url.Parse(endpoint)
	if err != nil || !s.AllowEndpoint(u) {
		return fmt.Errorf("webpush: %q is not a push service endpoint", endpoint)
	}
	ua, err := Decode64(p256dh)
	if err != nil {
		return fmt.Errorf("webpush: p256dh: %w", err)
	}
	secret, err := Decode64(auth)
	if err != nil {
		return fmt.Errorf("webpush: auth: %w", err)
	}
	body, err := Encrypt(payload, ua, secret)
	if err != nil {
		return err
	}
	authz, err := s.vapid(u)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "high")
	req.Header.Set("Authorization", authz)
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("webpush: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrGone
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("webpush: the push service answered %d", resp.StatusCode)
	}
	return nil
}
