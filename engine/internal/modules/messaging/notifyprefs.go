package messaging

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/aether-rtc/aether/internal/platform/httpx"
)

// What a person wants to be told about, and how loudly.
//
// Three levels. Alert: a sound and a banner when they are here, a push when
// Aether is closed. Quiet: it counts as unread, or sits in the feed, and
// makes no noise. Off: not in the feed either, offered only where there is
// a feed row to leave out.
//
// One row per person who has changed something; no row means the defaults,
// which are what Aether did before there were settings. A level only ever
// takes away: nothing it says causes a notice that would not otherwise
// have been sent. (The email period is the exception and starts off.) Urgent messages and calls are not settings. They always
// come through, and do not disturb and quiet hours have their own rules
// for those.
//
// A setting is enforced where the notice is made, not where it is offered:
//
//   - notifyAbsent, the push to a closed Aether. Preview decides whether
//     the words leave the server at all, which is why it cannot be a
//     client preference: the client that would hide them is not running.
//   - recordActivity, the feed: off means the row is never written.
//   - the client (maybeNotify in chat.js), for the sound and the banner of
//     a person who is here, from the copy the conversation list carries.

const (
	NotifyAlert = "alert"
	NotifyQuiet = "quiet"
	NotifyOff   = "off"
)

var ErrBadNotify = errors.New("messaging: not a notification setting")

// NotifyPrefs is one person's settings. A message falls under exactly one
// of the first four: a mention of them, else a reply to something they
// wrote, else a direct chat, else a group or channel.
type NotifyPrefs struct {
	Direct   string `json:"direct"`   // alert, quiet
	Group    string `json:"group"`    // alert, quiet
	Mention  string `json:"mention"`  // alert, quiet: a mention is always in the feed
	Reply    string `json:"reply"`    // alert, quiet, off
	Reaction string `json:"reaction"` // quiet, off: a reaction never alerts
	// Preview is whether a notice may carry the words of the message.
	Preview bool `json:"preview"`
	// Email is how often a missed-activity email may be sent: off, 10m,
	// hourly, 8h or daily (maildigest.go). It is the one setting here that
	// adds a notice, so it starts off.
	Email string `json:"email"`
}

// emailPeriods is the closed set of Email, in minutes; it is stored as the
// number.
var emailPeriods = map[string]int{"off": 0, "10m": 10, "hourly": 60, "8h": 480, "daily": 1440}

func emailPeriodName(minutes int) string {
	for name, m := range emailPeriods {
		if m == minutes {
			return name
		}
	}
	return "off"
}

// DefaultNotifyPrefs is what everyone has until they choose otherwise.
func DefaultNotifyPrefs() NotifyPrefs {
	return NotifyPrefs{Direct: NotifyAlert, Group: NotifyAlert, Mention: NotifyAlert, Reply: NotifyAlert, Reaction: NotifyQuiet, Preview: true, Email: "off"}
}

func (p NotifyPrefs) valid() bool {
	two := func(v string) bool { return v == NotifyAlert || v == NotifyQuiet }
	if _, ok := emailPeriods[p.Email]; !ok {
		return false
	}
	return two(p.Direct) && two(p.Group) && two(p.Mention) &&
		(two(p.Reply) || p.Reply == NotifyOff) &&
		(p.Reaction == NotifyQuiet || p.Reaction == NotifyOff)
}

// level is the setting a message falls under for one reader.
func (p NotifyPrefs) level(mentioned, replied, direct bool) string {
	switch {
	case mentioned:
		return p.Mention
	case replied:
		return p.Reply
	case direct:
		return p.Direct
	}
	return p.Group
}

// NotifyPrefsOf is the caller's own settings.
func (s *Service) NotifyPrefsOf(ctx context.Context, a Actor) (NotifyPrefs, error) {
	got, err := s.repo.NotifyPrefsFor(ctx, []string{a.ID})
	if err != nil {
		return NotifyPrefs{}, err
	}
	if p, ok := got[a.ID]; ok {
		return p, nil
	}
	return DefaultNotifyPrefs(), nil
}

// SetNotifyPrefs replaces the caller's settings. Every field is required: a
// missing one is not a level.
func (s *Service) SetNotifyPrefs(ctx context.Context, a Actor, p NotifyPrefs) (NotifyPrefs, error) {
	if !p.valid() {
		return NotifyPrefs{}, ErrBadNotify
	}
	if err := s.repo.SetNotifyPrefs(ctx, a.ID, p, time.Now().UTC()); err != nil {
		return NotifyPrefs{}, err
	}
	return p, nil
}

// notifyPrefsOf reads settings for several people in one query and answers
// for anyone, the defaults for those who have none.
func (s *Service) notifyPrefsOf(ctx context.Context, ids []string) func(id string) NotifyPrefs {
	got, err := s.repo.NotifyPrefsFor(ctx, ids)
	if err != nil {
		got = nil // the defaults: failing to read a preference must not lose a notice
	}
	def := DefaultNotifyPrefs()
	return func(id string) NotifyPrefs {
		if p, ok := got[id]; ok {
			return p
		}
		return def
	}
}

// --- repository -------------------------------------------------------------

var notifyLevels = []string{NotifyOff, NotifyQuiet, NotifyAlert} // stored as the index

func notifyLevel(v string) int {
	for i, x := range notifyLevels {
		if x == v {
			return i
		}
	}
	return -1
}

func (r *SQLRepository) SetNotifyPrefs(ctx context.Context, userID string, p NotifyPrefs, now time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO messaging_notify_prefs(user_id, direct, grp, mention, reply, reaction, preview, email, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET direct=excluded.direct, grp=excluded.grp, mention=excluded.mention,
			reply=excluded.reply, reaction=excluded.reaction, preview=excluded.preview, email=excluded.email, updated_at=excluded.updated_at`,
		userID, notifyLevel(p.Direct), notifyLevel(p.Group), notifyLevel(p.Mention), notifyLevel(p.Reply),
		notifyLevel(p.Reaction), boolInt(p.Preview), emailPeriods[p.Email], now.UnixMilli())
	return err
}

// NotifyPrefsFor is the settings of those among ids who have any: one
// query however many are asked.
func (r *SQLRepository) NotifyPrefsFor(ctx context.Context, ids []string) (map[string]NotifyPrefs, error) {
	out := map[string]NotifyPrefs{}
	if len(ids) == 0 {
		return out, nil
	}
	ph, args := placeholders(ids)
	rows, err := r.db.QueryContext(ctx, `
		SELECT user_id, direct, grp, mention, reply, reaction, preview, email
		  FROM messaging_notify_prefs WHERE user_id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var d, g, m, re, rx, pv, em int
		if err := rows.Scan(&id, &d, &g, &m, &re, &rx, &pv, &em); err != nil {
			return nil, err
		}
		out[id] = NotifyPrefs{Direct: notifyLevels[d], Group: notifyLevels[g], Mention: notifyLevels[m],
			Reply: notifyLevels[re], Reaction: notifyLevels[rx], Preview: pv != 0, Email: emailPeriodName(em)}
	}
	return out, rows.Err()
}

// --- handlers ---------------------------------------------------------------

func (h *Handler) notifyPrefs(w http.ResponseWriter, r *http.Request) {
	p, err := h.svc.NotifyPrefsOf(r.Context(), h.actor(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	// emailOffered: whether this server can send mail at all, so the page
	// does not offer a period nothing will ever act on.
	httpx.JSON(w, http.StatusOK, map[string]any{"notify": p, "emailOffered": h.svc.Mail != nil})
}

func (h *Handler) setNotifyPrefs(w http.ResponseWriter, r *http.Request) {
	var req NotifyPrefs
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	p, err := h.svc.SetNotifyPrefs(r.Context(), h.actor(r), req)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"notify": p})
}
