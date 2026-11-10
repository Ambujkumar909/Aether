package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := b64.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RFC 8291 Appendix A: the published example, byte for byte.
func TestEncryptMatchesTheRFC8291Example(t *testing.T) {
	plaintext := []byte("When I grow up, I want to be a watermelon")
	uaPriv := mustB64(t, "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94")
	uaPub := mustB64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	asPriv := mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw")
	auth := mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg")
	salt := mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw")
	want := mustB64(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN")

	sender, err := ecdh.P256().NewPrivateKey(asPriv)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Encrypt(plaintext, Subscription{P256dh: uaPub, Auth: auth}, sender, salt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ciphertext differs from the RFC example\n got %x\nwant %x", got, want)
	}
	// And the browser's side of it decrypts back to the words.
	ua, _ := ecdh.P256().NewPrivateKey(uaPriv)
	back, err := decrypt(got, ua, auth)
	if err != nil || string(back) != string(plaintext) {
		t.Fatalf("decrypt: %q %v", back, err)
	}
}

// decrypt is the receiver's side (what a browser does), used to prove a
// freshly keyed message round-trips, and by the fake push service.
func decrypt(body []byte, ua *ecdh.PrivateKey, auth []byte) ([]byte, error) {
	if len(body) < 16+4+1+65+17 {
		return nil, errors.New("short")
	}
	salt := body[:16]
	rs := binary.BigEndian.Uint32(body[16:20])
	idlen := int(body[20])
	if rs != 4096 || idlen != 65 {
		return nil, errors.New("header")
	}
	asPubBytes := body[21 : 21+65]
	asPub, err := ecdh.P256().NewPublicKey(asPubBytes)
	if err != nil {
		return nil, err
	}
	shared, err := ua.ECDH(asPub)
	if err != nil {
		return nil, err
	}
	info := append(append([]byte("WebPush: info\x00"), ua.PublicKey().Bytes()...), asPubBytes...)
	ikm, _ := hkdf.Key(sha256.New, shared, auth, string(info), 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	rec, err := gcm.Open(nil, nonce, body[21+65:], nil)
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndexByte(rec, 0x02)
	if i < 0 {
		return nil, errors.New("no delimiter")
	}
	return rec[:i], nil
}

func TestAFreshMessageRoundTripsAndTheBoundsHold(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	io.ReadFull(rand.Reader, auth)
	sub, err := ParseSubscription("https://push.example/abc", b64.EncodeToString(ua.PublicKey().Bytes()), b64.EncodeToString(auth))
	if err != nil {
		t.Fatal(err)
	}
	sender, _ := ecdh.P256().GenerateKey(rand.Reader)
	salt := make([]byte, 16)
	io.ReadFull(rand.Reader, salt)
	msg := []byte(`{"title":"Priya","body":"are you there?"}`)
	body, err := Encrypt(msg, sub, sender, salt)
	if err != nil {
		t.Fatal(err)
	}
	back, err := decrypt(body, ua, auth)
	if err != nil || !bytes.Equal(back, msg) {
		t.Fatalf("round trip: %q %v", back, err)
	}
	if _, err := Encrypt(make([]byte, MaxPayload+1), sub, sender, salt); err == nil {
		t.Fatal("a payload over the bound was encrypted")
	}
	// Subscriptions that must be refused.
	good := b64.EncodeToString(ua.PublicKey().Bytes())
	for _, c := range [][3]string{
		{"http://push.example/abc", good, b64.EncodeToString(auth)}, // plain http off the loopback
		{"https://push.example/abc", good[:40], b64.EncodeToString(auth)},
		{"https://push.example/abc", good, b64.EncodeToString(auth[:8])},
		{"https://push.example/abc", b64.EncodeToString(append([]byte{4}, make([]byte, 64)...)), b64.EncodeToString(auth)}, // not on the curve
		{"https://" + strings.Repeat("a", 2050), good, b64.EncodeToString(auth)},
	} {
		if _, err := ParseSubscription(c[0], c[1], c[2]); !errors.Is(err, ErrBadSubscription) {
			t.Fatalf("accepted %v: %v", c, err)
		}
	}
	if _, err := ParseSubscription("http://127.0.0.1:9470/x", good, b64.EncodeToString(auth)); err != nil {
		t.Fatalf("loopback http is for the fake push service in tests: %v", err)
	}
}

func TestTheKeyIsStableAndTheTokenVerifies(t *testing.T) {
	seed := bytes.Repeat([]byte{7}, 32)
	k1, err := KeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := KeyFromSeed(seed)
	if k1.Public() != k2.Public() || len(mustB64(t, k1.Public())) != 65 {
		t.Fatalf("key not stable or not a point: %s / %s", k1.Public(), k2.Public())
	}
	other, _ := KeyFromSeed(bytes.Repeat([]byte{8}, 32))
	if other.Public() == k1.Public() {
		t.Fatal("different seeds gave one key")
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	tok, err := k1.token("https://push.example", "mailto:ops@example.com", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := k1.Verify(tok, "https://push.example", now); err != nil {
		t.Fatal(err)
	}
	if err := k1.Verify(tok, "https://other.example", now); err == nil {
		t.Fatal("wrong audience accepted")
	}
	if err := other.Verify(tok, "https://push.example", now); err == nil {
		t.Fatal("another key's token accepted")
	}
	if err := k1.Verify(tok, "https://push.example", now.Add(13*time.Hour)); err == nil {
		t.Fatal("expired token accepted")
	}
}

// A fake push service: the headers a real one checks, the body decrypted
// with the subscriber's key, and the status codes the sender must map.
func TestSendTalksToAPushServiceAndMapsItsAnswers(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	io.ReadFull(rand.Reader, auth)
	key, _ := KeyFromSeed(bytes.Repeat([]byte{9}, 32))
	var got struct {
		body    []byte
		headers http.Header
	}
	status := http.StatusCreated
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.body, _ = io.ReadAll(io.LimitReader(r.Body, 8192))
		got.headers = r.Header.Clone()
		w.WriteHeader(status)
	}))
	defer srv.Close()
	sub, err := ParseSubscription(srv.URL+"/sub/1", b64.EncodeToString(ua.PublicKey().Bytes()), b64.EncodeToString(auth))
	if err != nil {
		t.Fatal(err)
	}
	s := NewSender(key, "mailto:ops@example.com")
	s.now = func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) }
	if err := s.Send(context.Background(), sub, []byte(`{"title":"hi"}`), 2*time.Minute, "high"); err != nil {
		t.Fatal(err)
	}
	if got.headers.Get("Content-Encoding") != "aes128gcm" || got.headers.Get("TTL") != "120" || got.headers.Get("Urgency") != "high" {
		t.Fatalf("headers: %v", got.headers)
	}
	authz := got.headers.Get("Authorization")
	if !strings.HasPrefix(authz, "vapid t=") || !strings.Contains(authz, ", k="+key.Public()) {
		t.Fatalf("authorization: %q", authz)
	}
	tok := strings.TrimPrefix(strings.Split(authz, ",")[0], "vapid t=")
	if err := key.Verify(tok, srv.URL, s.now()); err != nil {
		t.Fatalf("token: %v", err)
	}
	back, err := decrypt(got.body, ua, auth)
	if err != nil || string(back) != `{"title":"hi"}` {
		t.Fatalf("the push service could not read the message: %q %v", back, err)
	}
	status = http.StatusGone
	if err := s.Send(context.Background(), sub, []byte("x"), time.Minute, ""); !errors.Is(err, ErrGone) {
		t.Fatalf("410: %v", err)
	}
	status = http.StatusTooManyRequests
	if err := s.Send(context.Background(), sub, []byte("x"), time.Minute, ""); !errors.Is(err, ErrRejected) {
		t.Fatalf("429: %v", err)
	}
}
