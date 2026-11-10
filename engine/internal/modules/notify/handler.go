package notify

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/aether-rtc/aether/internal/platform/httpx"
)

// Handler exposes push subscription over HTTP.
type Handler struct {
	svc   *Service
	guard Guard
	log   *slog.Logger
}

func NewHandler(svc *Service, guard Guard, log *slog.Logger) *Handler {
	return &Handler{svc: svc, guard: guard, log: log}
}

func (h *Handler) Routes(mux *http.ServeMux) {
	// Anyone who may use chat may be notified about it. Protect adds the
	// session check, the CSRF check on writes, and the throttle.
	p := func(next http.HandlerFunc) http.Handler { return h.guard.Protect("meeting.join", next) }
	mux.Handle("GET /api/notify/push/key", p(h.key))
	mux.Handle("POST /api/notify/push/subscribe", p(h.subscribe))
	mux.Handle("POST /api/notify/push/unsubscribe", p(h.unsubscribe))
}

// key is the application server key, and whether push is on at all.
func (h *Handler) key(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{"enabled": h.svc.Enabled(), "key": h.svc.PublicKey()})
}

// subscribe takes PushSubscription.toJSON() as the browser makes it.
func (h *Handler) subscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := httpx.Decode(w, r, &body); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	id, _ := h.guard.ActorOf(r)
	err := h.svc.Subscribe(r.Context(), id, h.guard.SessionOf(r), body.Endpoint, body.Keys.P256dh, body.Keys.Auth)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrUnavailable):
		httpx.Error(w, http.StatusNotImplemented, "push_off", "this server does not send push notifications")
	case errors.Is(err, ErrTooMany):
		httpx.Error(w, http.StatusBadRequest, "too_many", "this account already has the most devices it can notify; sign out of one")
	case errors.Is(err, ErrBadRequest):
		httpx.Error(w, http.StatusBadRequest, "bad_subscription", "the subscription is not valid")
	default:
		h.log.Error("subscribe", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "could not save the subscription")
	}
}

func (h *Handler) unsubscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := httpx.Decode(w, r, &body); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	id, _ := h.guard.ActorOf(r)
	if err := h.svc.Unsubscribe(r.Context(), id, body.Endpoint); err != nil {
		if errors.Is(err, ErrBadRequest) {
			httpx.Error(w, http.StatusBadRequest, "bad_request", "an endpoint is required")
			return
		}
		h.log.Error("unsubscribe", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "could not remove the subscription")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
