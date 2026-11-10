// Package webpush sends Web Push messages (RFC 8030) with the payload
// encrypted for the browser (RFC 8291, aes128gcm) and the server identified
// by a VAPID key (RFC 8292). Standard library only: ECDH on P-256, HKDF,
// AES-128-GCM, ECDSA for the token. No push-service account of any kind is
// involved: the browser hands the application an endpoint URL at its own
// push service, and the application posts to it.
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
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Subscription is what the browser's PushManager hands the page.
type Subscription struct {
	Endpoint string
	// P256dh is the browser's public key, 65 bytes uncompressed; Auth is its
	// 16-byte authentication secret. Both arrive base64url from the browser.
	P256dh []byte
	Auth   []byte
}

// MaxPayload is the most this sender will encrypt. Push services accept
// 4 KB; a notification needs far less, and a bound here is a bound on what
// a caller can be made to send.
const MaxPayload = 3072

var (
	ErrBadSubscription = errors.New("webpush: the subscription is not usable")
	// ErrGone is a push service saying the subscription no longer exists
	// (404 or 410): the caller should forget it.
	ErrGone = errors.New("webpush: the subscription is gone")
	// ErrRejected is any other refusal; the caller may retry later.
	ErrRejected = errors.New("webpush: the push service refused")
)

// ParseSubscription checks a browser's subscription: an https endpoint (or
// http on the loopback, for tests against a fake push service), a 65-byte
// uncompressed P-256 point, a 16-byte secret.
func ParseSubscription(endpoint, p256dh, auth string) (Subscription, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || len(endpoint) > 2048 {
		return Subscription{}, fmt.Errorf("%w: endpoint", ErrBadSubscription)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
	default:
		return Subscription{}, fmt.Errorf("%w: endpoint scheme", ErrBadSubscription)
	}
	pk, err := b64.DecodeString(strings.TrimRight(p256dh, "="))
	if err != nil || len(pk) != 65 || pk[0] != 4 {
		return Subscription{}, fmt.Errorf("%w: p256dh", ErrBadSubscription)
	}
	if _, err := ecdh.P256().NewPublicKey(pk); err != nil {
		return Subscription{}, fmt.Errorf("%w: p256dh is not on the curve", ErrBadSubscription)
	}
	a, err := b64.DecodeString(strings.TrimRight(auth, "="))
	if err != nil || len(a) != 16 {
		return Subscription{}, fmt.Errorf("%w: auth", ErrBadSubscription)
	}
	return Subscription{Endpoint: endpoint, P256dh: pk, Auth: a}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var b64 = base64.RawURLEncoding

// Encrypt produces the aes128gcm body for one message (RFC 8291 §3): a
// header of salt, record size and the sender's ephemeral public key, then
// one record of the plaintext, a 0x02 delimiter, and the GCM tag.
// Exposed, with the sender key and salt as parameters, so the RFC's own
// test vector can check it.
func Encrypt(plaintext []byte, sub Subscription, sender *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(plaintext) > MaxPayload {
		return nil, errors.New("webpush: payload too large")
	}
	if len(salt) != 16 {
		return nil, errors.New("webpush: salt must be 16 bytes")
	}
	uaPub, err := ecdh.P256().NewPublicKey(sub.P256dh)
	if err != nil {
		return nil, ErrBadSubscription
	}
	shared, err := sender.ECDH(uaPub)
	if err != nil {
		return nil, err
	}
	asPub := sender.PublicKey().Bytes()
	// IKM = HKDF(auth, shared, "WebPush: info" || 0x00 || ua_public || as_public)
	info := append(append([]byte("WebPush: info\x00"), sub.P256dh...), asPub...)
	ikm, err := hkdf.Key(sha256.New, shared, sub.Auth, string(info), 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
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
	record := append(append([]byte{}, plaintext...), 0x02) // last record
	const rs = 4096
	out := make([]byte, 0, 16+4+1+65+len(record)+16)
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, rs)
	out = append(out, byte(len(asPub)))
	out = append(out, asPub...)
	out = gcm.Seal(out, nonce, record, nil)
	return out, nil
}

// Key is the application's VAPID identity: one P-256 key for as long as
// the deployment lives. Browsers bind each subscription to the public key
// they were given, so losing the private key means every subscriber must
// subscribe again.
type Key struct {
	priv *ecdsa.PrivateKey
}

// KeyFromSeed derives the key from 32 secret bytes (the deployment's key
// file), deterministically, so the same file always yields the same key.
func KeyFromSeed(seed []byte) (*Key, error) {
	if len(seed) < 32 {
		return nil, errors.New("webpush: the key seed must be at least 32 bytes")
	}
	// A seed is a valid scalar unless it is zero or at least the group
	// order, which for 256 random bits happens about once in 2^32 draws:
	// hash forward until it is.
	d := sha256.Sum256(append([]byte("aether vapid v1:"), seed...))
	for range 64 {
		if priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d[:]); err == nil {
			return &Key{priv: priv}, nil
		}
		d = sha256.Sum256(d[:])
	}
	return nil, errors.New("webpush: could not derive a key")
}

// Public is the applicationServerKey the page passes to PushManager:
// the uncompressed point, base64url without padding.
func (k *Key) Public() string {
	b, err := k.priv.PublicKey.Bytes() // the uncompressed point, as Web Push specifies
	if err != nil {
		return ""
	}
	return b64.EncodeToString(b)
}

// token is the VAPID JWT (RFC 8292 §2): ES256 over the endpoint's origin,
// expiring within 24 hours, naming a contact.
func (k *Key) token(aud, contact string, now time.Time) (string, error) {
	header := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(map[string]any{"aud": aud, "exp": now.Add(12 * time.Hour).Unix(), "sub": contact})
	signing := header + "." + b64.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + b64.EncodeToString(sig), nil
}

// Verify checks a token this key issued (for tests and for a fake push
// service): the signature, the audience, and that it has not expired.
func (k *Key) Verify(token, aud string, now time.Time) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("webpush: malformed token")
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return errors.New("webpush: malformed signature")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&k.priv.PublicKey, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return errors.New("webpush: bad signature")
	}
	raw, err := b64.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var c struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return err
	}
	if c.Aud != aud {
		return errors.New("webpush: audience mismatch")
	}
	if time.Unix(c.Exp, 0).Before(now) {
		return errors.New("webpush: token expired")
	}
	return nil
}

// Sender posts encrypted messages to push services.
type Sender struct {
	Key     *Key
	Contact string // mailto: or https: the push service may use to reach the operator
	Client  *http.Client
	now     func() time.Time
}

func NewSender(key *Key, contact string) *Sender {
	return &Sender{Key: key, Contact: contact, Client: &http.Client{Timeout: 10 * time.Second}, now: time.Now}
}

// Send encrypts payload for sub and posts it with a time to live. Urgency
// tells the push service how hard to try to wake the device: "high" for a
// call, "normal" for a message.
func (s *Sender) Send(ctx context.Context, sub Subscription, payload []byte, ttl time.Duration, urgency string) error {
	sender, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return err
	}
	body, err := Encrypt(payload, sub, sender, salt)
	if err != nil {
		return err
	}
	u, err := url.Parse(sub.Endpoint)
	if err != nil {
		return ErrBadSubscription
	}
	tok, err := s.Key.token(u.Scheme+"://"+u.Host, s.Contact, s.now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", strconv.Itoa(int(ttl/time.Second)))
	if urgency != "" {
		req.Header.Set("Urgency", urgency)
	}
	req.Header.Set("Authorization", "vapid t="+tok+", k="+s.Key.Public())
	res, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	switch res.StatusCode {
	case http.StatusCreated, http.StatusOK, http.StatusAccepted:
		return nil
	case http.StatusNotFound, http.StatusGone:
		return ErrGone
	default:
		return fmt.Errorf("%w: %d", ErrRejected, res.StatusCode)
	}
}
