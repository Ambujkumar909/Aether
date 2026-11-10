// Package notify reaches a person whose Aether is closed: a browser's push
// subscription, kept per sign-in session, and a bounded queue of notices
// posted to the browser's push service with the payload encrypted for that
// browser alone (internal/platform/webpush).
//
// What goes out is what a locked phone shows on its screen: a title, a
// line, and where to go when tapped. The push service sees only ciphertext;
// the application never learns anything from it but "delivered" or "gone".
package notify

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Subscription is one browser's push endpoint, bound to the session that
// made it so signing out there stops the notices there.
type Subscription struct {
	ID        string
	UserID    string
	SessionID string
	Endpoint  string
	P256dh    string // base64url, as the browser gave it
	Auth      string // base64url
	CreatedAt time.Time
	Fails     int
}

// Notice is one notification as the page will show it.
type Notice struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	// URL is same-origin, where a tap takes the person.
	URL string `json:"url,omitempty"`
	// Tag collapses notices about the same thing (a conversation, a call).
	Tag string `json:"tag,omitempty"`
	// Urgent asks the push service to wake the device now: a call.
	Urgent bool `json:"-"`
}

const (
	// MaxPerUser bounds subscriptions per account: a laptop, a phone, a
	// couple of browsers. The eleventh is refused.
	MaxPerUser = 10
	// MaxFails is how many consecutive refusals a push service may give
	// before the subscription is dropped as dead.
	MaxFails = 5
	// QueueDepth bounds notices waiting to be sent. Full means dropped and
	// counted, never a growing buffer.
	QueueDepth = 1024
)

var (
	ErrTooMany     = errors.New("notify: too many subscriptions for this account")
	ErrBadRequest  = errors.New("notify: the subscription is not valid")
	ErrUnavailable = errors.New("notify: push is not configured on this server")
)

// Repository stores subscriptions.
type Repository interface {
	// Upsert adds or refreshes a subscription; the endpoint is the key.
	Upsert(ctx context.Context, s *Subscription) error
	Delete(ctx context.Context, userID, endpoint string) error
	DeleteByID(ctx context.Context, id string) error
	// ForUser lists subscriptions whose session is still live: not revoked,
	// not expired. A signed-out device is not notified.
	ForUser(ctx context.Context, userID string, now time.Time) ([]*Subscription, error)
	CountForUser(ctx context.Context, userID string) (int, error)
	// Failed counts a refusal; at MaxFails the row goes. Returns whether it
	// was dropped.
	Failed(ctx context.Context, id string) (bool, error)
	Succeeded(ctx context.Context, id string) error
}

// Guard is what the handler needs from identity.
type Guard interface {
	Protect(permission string, next http.Handler) http.Handler
	ActorOf(r *http.Request) (id, email string)
	// SessionOf is the id of the session behind the request.
	SessionOf(r *http.Request) string
}
