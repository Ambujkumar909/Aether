package notify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aether-rtc/aether/internal/platform/webpush"
)

// Service keeps subscriptions and sends notices.
type Service struct {
	repo   Repository
	sender *webpush.Sender // nil when the server has no push key
	log    *slog.Logger
	now    func() time.Time

	queue   chan job
	workers sync.WaitGroup
	stop    chan struct{}
	once    sync.Once

	// Dropped counts notices the full queue refused, and Sent the ones a
	// push service accepted; read by the metrics page.
	Dropped atomic.Int64
	Sent    atomic.Int64
	// delivered observes the outcome of each send in tests.
	delivered func(sub *Subscription, err error)
}

type job struct {
	userID string
	n      Notice
}

func NewService(repo Repository, sender *webpush.Sender, log *slog.Logger) *Service {
	return &Service{repo: repo, sender: sender, log: log, now: time.Now, queue: make(chan job, QueueDepth), stop: make(chan struct{})}
}

// Enabled reports whether this server can send push at all.
func (s *Service) Enabled() bool { return s.sender != nil }

// PublicKey is what the page hands PushManager.subscribe.
func (s *Service) PublicKey() string {
	if s.sender == nil {
		return ""
	}
	return s.sender.Key.Public()
}

// Start runs the senders; Stop drains them.
func (s *Service) Start(workers int) {
	s.once.Do(func() {
		for range workers {
			s.workers.Add(1)
			go s.run()
		}
	})
}

func (s *Service) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.workers.Wait()
}

func (s *Service) run() {
	defer s.workers.Done()
	for {
		select {
		case <-s.stop:
			return
		case j := <-s.queue:
			s.deliver(j)
		}
	}
}

// Subscribe records a browser's subscription for this session. The same
// endpoint subscribing again (a page reload) refreshes the row rather than
// adding one; a new endpoint past MaxPerUser is refused.
func (s *Service) Subscribe(ctx context.Context, userID, sessionID, endpoint, p256dh, auth string) error {
	if s.sender == nil {
		return ErrUnavailable
	}
	if userID == "" || sessionID == "" {
		return ErrBadRequest
	}
	if _, err := webpush.ParseSubscription(endpoint, p256dh, auth); err != nil {
		return ErrBadRequest
	}
	n, err := s.repo.CountForUser(ctx, userID)
	if err != nil {
		return err
	}
	if n >= MaxPerUser {
		// Unless this endpoint is already one of them, in which case the
		// upsert refreshes it and adds nothing.
		subs, err := s.repo.ForUser(ctx, userID, s.now())
		if err != nil {
			return err
		}
		known := false
		for _, x := range subs {
			if x.Endpoint == endpoint {
				known = true
			}
		}
		if !known {
			return ErrTooMany
		}
	}
	return s.repo.Upsert(ctx, &Subscription{
		ID: newID(), UserID: userID, SessionID: sessionID,
		Endpoint: endpoint, P256dh: p256dh, Auth: auth, CreatedAt: s.now().UTC(),
	})
}

// Unsubscribe forgets one endpoint of this user's. Someone else's endpoint
// is not theirs to remove: the user id is part of the key.
func (s *Service) Unsubscribe(ctx context.Context, userID, endpoint string) error {
	if userID == "" || endpoint == "" || len(endpoint) > 2048 {
		return ErrBadRequest
	}
	return s.repo.Delete(ctx, userID, endpoint)
}

// Notify queues a notice for every live subscription of a user. It never
// blocks the caller: a full queue drops the notice and counts it. Called
// from the chat path when the person has no socket open, so this is the
// only way the notice reaches them.
func (s *Service) Notify(userID, title, body, url, tag string, urgent bool) {
	if s.sender == nil || userID == "" {
		return
	}
	select {
	case s.queue <- job{userID: userID, n: Notice{Title: title, Body: body, URL: url, Tag: tag, Urgent: urgent}}:
	default:
		if s.Dropped.Add(1)%100 == 1 {
			s.log.Warn("push queue full; notices dropped", "dropped", s.Dropped.Load())
		}
	}
}

func (s *Service) deliver(j job) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	subs, err := s.repo.ForUser(ctx, j.userID, s.now())
	if err != nil || len(subs) == 0 {
		return
	}
	payload, err := json.Marshal(j.n)
	if err != nil {
		return
	}
	ttl, urgency := 10*time.Minute, "normal"
	if j.n.Urgent {
		ttl, urgency = time.Minute, "high"
	}
	for _, sub := range subs {
		ws, err := webpush.ParseSubscription(sub.Endpoint, sub.P256dh, sub.Auth)
		if err != nil {
			_ = s.repo.DeleteByID(ctx, sub.ID)
			continue
		}
		err = s.sender.Send(ctx, ws, payload, ttl, urgency)
		switch {
		case err == nil:
			s.Sent.Add(1)
			_ = s.repo.Succeeded(ctx, sub.ID)
		case errors.Is(err, webpush.ErrGone):
			_ = s.repo.DeleteByID(ctx, sub.ID)
		default:
			if dropped, _ := s.repo.Failed(ctx, sub.ID); dropped {
				s.log.Info("push subscription dropped after repeated refusals", "user", j.userID)
			}
		}
		if s.delivered != nil {
			s.delivered(sub, err)
		}
	}
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "ps-" + hex.EncodeToString(b[:])
}
