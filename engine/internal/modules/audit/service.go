package audit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// GenesisHash anchors the chain. A fixed, well-known value rather than a
// random one, so that a verifier with no prior state can still check the
// first link.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Service owns the chain.
type Service struct {
	repo Repository
	log  *slog.Logger

	// Appends are serialised. Two concurrent events reading the same
	// previous hash would both chain to it, and one of them would be
	// unverifiable forever after. This is the one place in the system where
	// a global lock is correct rather than lazy: audit volume is bounded by
	// human actions, and correctness of the chain is the entire product.
	mu       sync.Mutex
	headHash string
	headSeq  int64
	loaded   bool

	// Async recording keeps the audit write off the request path for events
	// where a few milliseconds of latency matters more than immediate
	// durability. Security-critical events use Record and block.
	queue    chan Event
	wg       sync.WaitGroup
	stop     chan struct{}
	stopOnce sync.Once
	stopped  atomic.Bool
}

func NewService(repo Repository, log *slog.Logger) *Service {
	return &Service{
		repo:  repo,
		log:   log,
		queue: make(chan Event, 1024),
		stop:  make(chan struct{}),
	}
}

// Start begins the background writer.
func (s *Service) Start(ctx context.Context) error {
	if err := s.loadHead(ctx); err != nil {
		return err
	}
	s.wg.Add(1)
	go s.drain()
	return nil
}

// Stop flushes the queue. Safe to call more than once, and from more than
// one goroutine.
//
// The idempotence is not decoration. This used to be a bare close(s.stop),
// which panics with "close of closed channel" the second time. Shutdown is
// the worst place in a program to panic: it runs while the process is already
// going down, frequently on an error path, and it aborts the rest of the
// orderly teardown -- here, the flush of unwritten audit events, which is
// exactly the data whose loss must never be silent.
//
// The kernel does guard against stopping a module twice, so this was not
// reachable through the normal path. It was reachable from anything else: a
// failed Start cleaning up after itself, a supervisor retrying, or a test,
// and the test helper in this package already registers Stop as cleanup, so
// any test that also stopped the service explicitly took the whole package
// down with it.
func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		s.stopped.Store(true)
		close(s.stop)
		s.wg.Wait()
	})
}

func (s *Service) loadHead(ctx context.Context) error {
	seq, hash, err := s.repo.Head(ctx)
	if err != nil {
		return fmt.Errorf("audit: load chain head: %w", err)
	}
	s.mu.Lock()
	s.headSeq = seq
	s.headHash = hash
	if s.headHash == "" {
		s.headHash = GenesisHash
	}
	s.loaded = true
	s.mu.Unlock()
	return nil
}

func (s *Service) drain() {
	defer s.wg.Done()
	for {
		select {
		case e := <-s.queue:
			s.write(context.Background(), e)
		case <-s.stop:
			// Drain whatever is queued before exiting. Losing the tail of an
			// audit log on shutdown is exactly the window an attacker would
			// choose.
			for {
				select {
				case e := <-s.queue:
					s.write(context.Background(), e)
				default:
					return
				}
			}
		}
	}
}

// Record appends an event synchronously. Use this for anything where the
// record must exist before the caller proceeds -- every authentication and
// authorisation decision qualifies.
func (s *Service) Record(ctx context.Context, e Event) {
	s.write(ctx, e)
}

// RecordAsync queues an event. Use for high-volume, low-stakes activity where
// the write must not sit on a request path.
func (s *Service) RecordAsync(e Event) {
	// After Stop there is no drain goroutine, so anything queued here would
	// sit in a buffer nobody reads and vanish with the process. Write it
	// synchronously instead: slower, and the alternative is losing audit
	// events during shutdown without a trace, which is the one failure this
	// log exists to make impossible.
	if s.stopped.Load() {
		s.write(context.Background(), e)
		return
	}
	select {
	case s.queue <- e:
	default:
		// The queue is full, which means the database is not keeping up.
		// Dropping silently would make the log lie, so this is recorded as a
		// gap the operator can see.
		s.log.Error("audit queue full, event dropped",
			"action", e.Action, "actor", e.ActorID)
	}
}

func (s *Service) write(ctx context.Context, e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.loaded {
		if s.headHash == "" {
			s.headHash = GenesisHash
		}
	}

	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	if e.ID == "" {
		e.ID = newID()
	}
	if e.Outcome == "" {
		e.Outcome = OutcomeSuccess
	}
	e.PrevHash = s.headHash
	e.Hash = ChainHash(&e)

	if err := s.repo.Append(ctx, &e); err != nil {
		// A failed audit write is a serious condition. It is logged loudly
		// and the chain head is left untouched, so the next event links to
		// the last one that actually persisted rather than to a phantom.
		s.log.Error("AUDIT WRITE FAILED",
			"action", e.Action, "actor", e.ActorID, "err", err)
		return
	}
	s.headHash = e.Hash
	s.headSeq = e.Seq
}

// ChainHash computes an event's link value.
//
// The encoding is length-prefixed rather than delimiter-joined. With a plain
// separator, an actor email of "a|b" and an action of "c" hash identically to
// an email of "a" and an action of "b|c" -- so a field whose content the
// attacker controls could be used to forge a colliding entry. Length prefixes
// remove the ambiguity entirely.
func ChainHash(e *Event) string {
	h := sha256.New()
	write := func(s string) {
		fmt.Fprintf(h, "%d:", len(s))
		h.Write([]byte(s))
	}
	write(e.PrevHash)
	write(e.ID)
	write(fmt.Sprintf("%d", e.At.UTC().UnixMilli()))
	write(e.ActorID)
	write(e.ActorEmail)
	write(string(e.Action))
	write(e.Subject)
	write(string(e.Outcome))
	write(e.IP)
	write(e.UserAgent)
	write(e.Detail)
	return hex.EncodeToString(h.Sum(nil))
}

// List queries the log.
func (s *Service) List(ctx context.Context, f Filter) ([]Event, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	return s.repo.List(ctx, f)
}

// Count reports how many events match a filter.
func (s *Service) Count(ctx context.Context, f Filter) (int64, error) {
	return s.repo.Count(ctx, f)
}

// Head returns the current chain tip, which is what an external verifier
// pins.
func (s *Service) Head(ctx context.Context) (int64, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headSeq, s.headHash
}

// Verify walks the entire chain and recomputes every link.
//
// This is the operation that turns the design into a guarantee. It is
// deliberately exposed to operators rather than run only internally: a
// tamper-evident log whose evidence nobody can inspect is a claim, not a
// control.
func (s *Service) Verify(ctx context.Context) (VerifyResult, error) {
	res := VerifyResult{OK: true, CheckedAt: time.Now().UTC()}
	prev := GenesisHash

	err := s.repo.Walk(ctx, 0, func(e Event) error {
		res.Checked++
		if e.PrevHash != prev {
			res.OK = false
			res.BrokenAt = e.Seq
			res.Detail = fmt.Sprintf(
				"event %d links to %s but the previous event hashes to %s: "+
					"an entry was altered, deleted, or reordered",
				e.Seq, short(e.PrevHash), short(prev))
			return errStopWalk
		}
		want := ChainHash(&e)
		if want != e.Hash {
			res.OK = false
			res.BrokenAt = e.Seq
			res.Detail = fmt.Sprintf(
				"event %d stores hash %s but its contents hash to %s: "+
					"the row was edited in place",
				e.Seq, short(e.Hash), short(want))
			return errStopWalk
		}
		prev = e.Hash
		res.HeadSeq = e.Seq
		res.HeadHash = e.Hash
		return nil
	})
	if err != nil && err != errStopWalk {
		return res, err
	}
	if res.Checked == 0 {
		res.HeadHash = GenesisHash
	}
	return res, nil
}

var errStopWalk = fmt.Errorf("stop")

func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12] + "…"
}

// ---------------------------------------------------------------------------
// Helpers for callers
// ---------------------------------------------------------------------------

// From builds an event pre-filled with request context.
func From(actorID, actorEmail, ip, ua string) Event {
	return Event{
		ActorID: actorID, ActorEmail: actorEmail,
		IP: ip, UserAgent: truncate(ua, 256),
		At: time.Now().UTC(),
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Describe renders an event as a human sentence for the UI, so the frontend
// does not have to carry a parallel vocabulary that can drift.
func Describe(e Event) string {
	who := e.ActorEmail
	if who == "" {
		who = "an anonymous request"
	}
	switch e.Action {
	case ActionLoginSucceeded:
		return who + " signed in"
	case ActionLoginFailed:
		return "failed sign-in attempt for " + orUnknown(e.Subject)
	case ActionAccountLocked:
		return orUnknown(e.Subject) + " was locked after repeated failures"
	case ActionLogout:
		return who + " signed out"
	case ActionSessionRevoked:
		return who + " revoked a session for " + orUnknown(e.Subject)
	case ActionPasswordChanged:
		return who + " changed the password for " + orUnknown(e.Subject)
	case ActionSignUp:
		return orUnknown(e.Subject) + " created an account"
	case ActionSignUpBlocked:
		return "blocked sign-up attempt for " + orUnknown(e.Subject)
	case ActionUserCreated:
		return who + " created " + orUnknown(e.Subject)
	case ActionUserRoleChanged:
		return who + " changed the role of " + orUnknown(e.Subject) + " " + e.Detail
	case ActionUserSuspended:
		return who + " suspended " + orUnknown(e.Subject)
	case ActionUserReactivated:
		return who + " reactivated " + orUnknown(e.Subject)
	case ActionUserDeleted:
		return who + " deleted " + orUnknown(e.Subject)
	case ActionMeetingJoined:
		return who + " joined " + orUnknown(e.Subject)
	case ActionMeetingLeft:
		return who + " left " + orUnknown(e.Subject)
	case ActionMeetingScheduled:
		return who + " scheduled " + orUnknown(e.Subject)
	case ActionMeetingRescheduled:
		return who + " rescheduled " + orUnknown(e.Subject)
	case ActionMeetingCancelled:
		return who + " cancelled " + orUnknown(e.Subject)
	case ActionMeetingDeleted:
		return who + " deleted the meeting " + orUnknown(e.Subject)
	case ActionMeetingResponded:
		return who + " " + e.Detail + " " + orUnknown(e.Subject)
	case ActionAuditExported:
		return who + " exported the audit log"
	default:
		return who + " " + strings.ReplaceAll(string(e.Action), ".", " ")
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "an unknown subject"
	}
	return s
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
