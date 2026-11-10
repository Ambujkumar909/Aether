package notify

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/platform/database"
	"github.com/aether-rtc/aether/internal/platform/webpush"
)

// Subscriptions follow sessions; notices reach every live device and no
// signed-out one; a dead endpoint is forgotten; the queue is bounded.

var b64 = base64.RawURLEncoding

type fixture struct {
	db   *database.DB
	svc  *Service
	push *httptest.Server
	// what the fake push service received, by endpoint path
	mu       sync.Mutex
	received map[string][][]byte
	status   atomic.Int32
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := &fixture{db: db, received: map[string][][]byte{}}
	f.status.Store(http.StatusCreated)
	f.push = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
		f.mu.Lock()
		f.received[r.URL.Path] = append(f.received[r.URL.Path], body)
		f.mu.Unlock()
		w.WriteHeader(int(f.status.Load()))
	}))
	t.Cleanup(f.push.Close)
	key, _ := webpush.KeyFromSeed(bytes.Repeat([]byte{3}, 32))
	f.svc = NewService(NewSQLRepository(db), webpush.NewSender(key, "mailto:ops@example.test"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return f
}

// A user with sessions, straight into the tables identity owns.
func (f *fixture) user(t *testing.T, id string, sessions ...string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if _, err := f.db.ExecContext(ctx, `INSERT INTO identity_users (id, email, display_name, password_hash, role, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`, id, id+"@example.test", id, "x", "member", "active", now, now); err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if _, err := f.db.ExecContext(ctx, `INSERT INTO identity_sessions (id, token_hash, user_id, created_at, expires_at, last_seen)
			VALUES (?,?,?,?,?,?)`, s, "h-"+s, id, now, now+3600_000, now); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *fixture) keys(t *testing.T) (string, string, *ecdh.PrivateKey, []byte) {
	t.Helper()
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	io.ReadFull(rand.Reader, auth)
	return b64.EncodeToString(ua.PublicKey().Bytes()), b64.EncodeToString(auth), ua, auth
}

func (f *fixture) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received[path])
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestNoticesReachLiveDevicesAndNoSignedOutOne(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.user(t, "u1", "s-laptop", "s-phone", "s-old")
	p1, a1, _, _ := f.keys(t)
	p2, a2, _, _ := f.keys(t)
	p3, a3, _, _ := f.keys(t)
	for _, c := range [][4]string{{"s-laptop", "/laptop", p1, a1}, {"s-phone", "/phone", p2, a2}, {"s-old", "/old", p3, a3}} {
		if err := f.svc.Subscribe(ctx, "u1", c[0], f.push.URL+c[1], c[2], c[3]); err != nil {
			t.Fatal(err)
		}
	}
	// The old session is revoked: its device must hear nothing more.
	if _, err := f.db.ExecContext(ctx, `UPDATE identity_sessions SET revoked_at = ? WHERE id = 's-old'`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	var sent atomic.Int32
	f.svc.delivered = func(*Subscription, error) { sent.Add(1) }
	f.svc.Start(2)
	defer f.svc.Stop()
	f.svc.Notify("u1", "Priya", "are you there?", "/?chat=c1", "conv-c1", false)
	waitFor(t, "two deliveries", func() bool { return sent.Load() == 2 })
	if f.count("/laptop") != 1 || f.count("/phone") != 1 || f.count("/old") != 0 {
		t.Fatalf("deliveries: laptop %d phone %d old %d", f.count("/laptop"), f.count("/phone"), f.count("/old"))
	}
	// Nobody else's devices.
	f.svc.Notify("u2", "x", "y", "/", "", false)
	time.Sleep(50 * time.Millisecond)
	if f.count("/laptop") != 1 {
		t.Fatal("a notice for another user reached u1")
	}
	// The same endpoint subscribing again refreshes, it does not add.
	if err := f.svc.Subscribe(ctx, "u1", "s-laptop", f.push.URL+"/laptop", p1, a1); err != nil {
		t.Fatal(err)
	}
	n, _ := f.svc.repo.CountForUser(ctx, "u1")
	if n != 3 {
		t.Fatalf("%d rows after resubscribing", n)
	}
	// Unsubscribe is by the owner only: another user naming the endpoint
	// removes nothing.
	if err := f.svc.Unsubscribe(ctx, "u2", f.push.URL+"/laptop"); err != nil {
		t.Fatal(err)
	}
	if n, _ = f.svc.repo.CountForUser(ctx, "u1"); n != 3 {
		t.Fatal("someone else removed u1's subscription")
	}
	if err := f.svc.Unsubscribe(ctx, "u1", f.push.URL+"/laptop"); err != nil {
		t.Fatal(err)
	}
	if n, _ = f.svc.repo.CountForUser(ctx, "u1"); n != 2 {
		t.Fatalf("%d after the owner unsubscribed", n)
	}
}

func TestBoundsAndBadSubscriptionsAreRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.user(t, "u1", "s1")
	for i := 0; i < MaxPerUser; i++ {
		p, a, _, _ := f.keys(t)
		if err := f.svc.Subscribe(ctx, "u1", "s1", f.push.URL+"/d"+string(rune('a'+i)), p, a); err != nil {
			t.Fatal(err)
		}
	}
	p, a, _, _ := f.keys(t)
	if err := f.svc.Subscribe(ctx, "u1", "s1", f.push.URL+"/one-more", p, a); !errors.Is(err, ErrTooMany) {
		t.Fatalf("the eleventh device: %v", err)
	}
	// An existing one may still refresh.
	if err := f.svc.Subscribe(ctx, "u1", "s1", f.push.URL+"/da", p, a); err != nil {
		t.Fatalf("refreshing a known endpoint at the bound: %v", err)
	}
	for _, c := range [][3]string{
		{"http://push.example/x", p, a}, // plain http off the loopback
		{f.push.URL + "/x", p[:30], a},
		{f.push.URL + "/x", p, "short"},
		{"", p, a},
	} {
		if err := f.svc.Subscribe(ctx, "u1", "s1", c[0], c[1], c[2]); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("accepted %v: %v", c, err)
		}
	}
	if err := f.svc.Subscribe(ctx, "", "s1", f.push.URL+"/x", p, a); !errors.Is(err, ErrBadRequest) {
		t.Fatal("no user accepted")
	}
	off := NewService(f.svc.repo, nil, f.svc.log)
	if err := off.Subscribe(ctx, "u1", "s1", f.push.URL+"/x", p, a); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a server without push accepted a subscription: %v", err)
	}
	off.Notify("u1", "x", "y", "/", "", false) // a no-op, not a panic
}

func TestAGoneEndpointIsForgottenAndRefusalsAreCounted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.user(t, "u1", "s1")
	p, a, _, _ := f.keys(t)
	if err := f.svc.Subscribe(ctx, "u1", "s1", f.push.URL+"/dead", p, a); err != nil {
		t.Fatal(err)
	}
	var done atomic.Int32
	f.svc.delivered = func(*Subscription, error) { done.Add(1) }
	f.svc.Start(1)
	defer f.svc.Stop()
	// Refused MaxFails-1 times: still there.
	f.status.Store(http.StatusTooManyRequests)
	for i := 0; i < MaxFails-1; i++ {
		f.svc.Notify("u1", "x", "y", "/", "", false)
	}
	waitFor(t, "refusals", func() bool { return int(done.Load()) == MaxFails-1 })
	if n, _ := f.svc.repo.CountForUser(ctx, "u1"); n != 1 {
		t.Fatalf("dropped after %d refusals", MaxFails-1)
	}
	// One success resets the count.
	f.status.Store(http.StatusCreated)
	f.svc.Notify("u1", "x", "y", "/", "", false)
	waitFor(t, "a success", func() bool { return int(done.Load()) == MaxFails })
	f.status.Store(http.StatusTooManyRequests)
	for i := 0; i < MaxFails-1; i++ {
		f.svc.Notify("u1", "x", "y", "/", "", false)
	}
	waitFor(t, "more refusals", func() bool { return int(done.Load()) == 2*MaxFails-1 })
	if n, _ := f.svc.repo.CountForUser(ctx, "u1"); n != 1 {
		t.Fatal("the success did not reset the count")
	}
	// Gone: forgotten at once.
	f.status.Store(http.StatusGone)
	f.svc.Notify("u1", "x", "y", "/", "", false)
	waitFor(t, "gone", func() bool { return int(done.Load()) == 2*MaxFails })
	if n, _ := f.svc.repo.CountForUser(ctx, "u1"); n != 0 {
		t.Fatal("a gone subscription was kept")
	}
	if f.svc.Sent.Load() != 1 {
		t.Fatalf("sent %d, want 1", f.svc.Sent.Load())
	}
}

func TestTheQueueIsBoundedAndDropsAreCounted(t *testing.T) {
	f := newFixture(t)
	// Not started: nothing drains, so the queue fills and the rest drop.
	for i := 0; i < QueueDepth+50; i++ {
		f.svc.Notify("u1", "x", "y", "/", "", false)
	}
	if f.svc.Dropped.Load() != 50 || len(f.svc.queue) != QueueDepth {
		t.Fatalf("dropped %d, queued %d", f.svc.Dropped.Load(), len(f.svc.queue))
	}
	f.svc.Start(1)
	f.svc.Stop() // returns: no goroutine left behind
}
