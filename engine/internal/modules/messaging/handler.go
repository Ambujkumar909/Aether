package messaging

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aether-rtc/aether/internal/platform/httpx"
	"github.com/aether-rtc/aether/internal/wire"
	"github.com/coder/websocket"
)

// Guard is the authorisation dependency, satisfied by the identity module.
type Guard interface {
	Protect(permission string, next http.Handler) http.Handler
	ActorOf(r *http.Request) (id, email string)
	NameOf(r *http.Request) string
}

type Handler struct {
	svc    *Service
	hub    *Hub
	guard  Guard
	log    *slog.Logger
	Origin []string
}

func NewHandler(svc *Service, hub *Hub, guard Guard, log *slog.Logger) *Handler {
	return &Handler{svc: svc, hub: hub, guard: guard, log: log}
}

func (h *Handler) Routes(mux *http.ServeMux) {
	// Messaging is available to anyone who may join a meeting: a chat you
	// cannot use is not a lesser product, it is a confusing one.
	p := func(next http.HandlerFunc) http.Handler {
		return h.guard.Protect("meeting.join", next)
	}
	mux.Handle("GET /api/messaging/conversations", p(h.list))
	mux.Handle("POST /api/messaging/direct", p(h.openDirect))
	mux.Handle("POST /api/messaging/self", p(h.openSelf))
	mux.Handle("POST /api/messaging/groups", p(h.createGroup))
	mux.Handle("GET /api/messaging/conversations/{id}/messages", p(h.messages))
	mux.Handle("GET /api/messaging/conversations/{id}/calls", p(h.calls))
	mux.Handle("POST /api/messaging/conversations/{id}/messages", p(h.send))
	mux.Handle("POST /api/messaging/conversations/{id}/read", p(h.markRead))
	mux.Handle("POST /api/messaging/conversations/{id}/typing", p(h.typing))
	mux.Handle("PATCH /api/messaging/messages/{id}", p(h.edit))
	mux.Handle("DELETE /api/messaging/messages/{id}", p(h.remove))
	mux.Handle("POST /api/messaging/messages/{id}/react", p(h.react))
	mux.Handle("POST /api/messaging/messages/{id}/save", p(h.save))
	mux.Handle("POST /api/messaging/messages/{id}/hide", p(h.hide))
	mux.Handle("POST /api/messaging/messages/{id}/forward", p(h.forward))
	mux.Handle("PATCH /api/messaging/conversations/{id}/prefs", p(h.prefs))
	mux.Handle("POST /api/messaging/conversations/{id}/unread", p(h.markUnread))
	mux.Handle("DELETE /api/messaging/conversations/{id}", p(h.forget))
	h.teamRoutes(mux, p)
	mux.Handle("POST /api/messaging/channels", p(h.createChannel))
	mux.Handle("GET /api/messaging/channels", p(h.browseChannels))
	mux.Handle("POST /api/messaging/channels/{id}/join", p(h.joinChannel))
	mux.Handle("POST /api/messaging/conversations/{id}/leave", p(h.leave))
	mux.Handle("GET /api/messaging/conversations/{id}/members", p(h.members))
	mux.Handle("POST /api/messaging/conversations/{id}/members", p(h.addMembers))
	mux.Handle("DELETE /api/messaging/conversations/{id}/members/{user}", p(h.removeMember))
	mux.Handle("PATCH /api/messaging/conversations/{id}", p(h.rename))
	mux.Handle("POST /api/messaging/conversations/{id}/files", p(h.upload))
	mux.Handle("GET /api/messaging/meeting/{room}", p(h.meetingChat))
	mux.Handle("GET /api/messaging/search", p(h.search))
	mux.Handle("GET /api/messaging/blocks", p(h.blocks))
	mux.Handle("PUT /api/messaging/status", p(h.setStatus))
	mux.Handle("GET /api/messaging/messages/{id}/replies", p(h.replies))
	mux.Handle("GET /api/messaging/messages/{id}/history", p(h.history))
	mux.Handle("GET /api/messaging/status", p(h.statuses))
	mux.Handle("POST /api/messaging/messages/{id}/vote", p(h.vote))
	mux.Handle("DELETE /api/messaging/channels/{id}", p(h.deleteChannel))
	mux.Handle("PUT /api/messaging/blocks/{user}", p(h.block))
	mux.Handle("DELETE /api/messaging/blocks/{user}", p(h.unblock))
	mux.Handle("PUT /api/messaging/conversations/{id}/avatar", p(h.setAvatar))
	mux.Handle("GET /api/messaging/conversations/{id}/avatar", p(h.avatar))
	mux.Handle("POST /api/messaging/conversations/{id}/pin", p(h.pin))
	mux.Handle("GET /api/messaging/files/{id}", p(h.download))
	mux.Handle("GET /api/messaging/conversations/{id}/shared", p(h.shared))
	mux.Handle("GET /api/messaging/saved", p(h.saved))

	// The push socket. Same permission, same session cookie.
	mux.Handle("/ws", h.guard.Protect("meeting.join", http.HandlerFunc(h.socket)))
}

func (h *Handler) actor(r *http.Request) Actor {
	id, email := h.guard.ActorOf(r)
	return Actor{ID: id, Email: email, Name: h.guard.NameOf(r), Device: deviceOf(r.Header.Get("X-Aether-Device"))}
}

// deviceOf bounds a client's device id: opaque, at most 64 characters, or
// nothing. It is a routing hint, never trusted for anything else.
func deviceOf(s string) string {
	if len(s) > 64 {
		return ""
	}
	return s
}

// ---------------------------------------------------------------------------
// REST
// ---------------------------------------------------------------------------

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	cs, err := h.svc.List(r.Context(), h.actor(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	if cs == nil {
		cs = []*Conversation{}
	}
	// The availability of everyone in the list, in one query, so the list
	// draws its dots without a request per row.
	var ids []string
	seen := map[string]bool{}
	for _, c := range cs {
		for _, m := range c.Members {
			if !seen[m.UserID] && len(ids) < MaxStatusQuery {
				seen[m.UserID] = true
				ids = append(ids, m.UserID)
			}
		}
	}
	statuses, _ := h.svc.Statuses(r.Context(), h.actor(r), ids)
	if statuses == nil {
		statuses = map[string]Status{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"conversations": cs, "statuses": statuses})
}

func (h *Handler) openDirect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		// UserID opens the same conversation for a caller who knows the
		// account but not the address -- which is what the meeting roster
		// gives, deliberately.
		UserID string `json:"userId"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	var c *Conversation
	var err error
	if req.Email == "" && req.UserID != "" {
		c, err = h.svc.OpenDirectByID(r.Context(), h.actor(r), req.UserID)
	} else {
		c, err = h.svc.OpenDirect(r.Context(), h.actor(r), req.Email)
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

// openSelf is the explicit "message yourself" button. It takes no body: the
// only account it can name is the caller's own.
func (h *Handler) openSelf(w http.ResponseWriter, r *http.Request) {
	c, err := h.svc.OpenSelf(r.Context(), h.actor(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

func (h *Handler) createGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title   string   `json:"title"`
		Members []string `json:"members"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	c, err := h.svc.CreateGroup(r.Context(), h.actor(r), req.Title, req.Members)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, c)
}

func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	var before time.Time
	if v := r.URL.Query().Get("before"); v != "" {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
			before = time.UnixMilli(ms).UTC()
		}
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	ms, err := h.svc.Messages(r.Context(), h.actor(r), r.PathValue("id"), before, limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	if ms == nil {
		ms = []*Message{}
	}
	h.svc.Decorate(r.Context(), h.actor(r), ms)
	httpx.JSON(w, http.StatusOK, map[string]any{"messages": ms})
}

func (h *Handler) send(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body       string   `json:"body"`
		ReplyTo    string   `json:"replyTo"`
		Mentions   []string `json:"mentions"`
		Priority   string   `json:"priority"`
		ScheduleAt int64    `json:"scheduleAt"` // unix ms; 0 = now
		Meta       string   `json:"meta"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	opts := SendOptions{Priority: req.Priority, Meta: req.Meta}
	if req.ScheduleAt > 0 {
		t := time.UnixMilli(req.ScheduleAt).UTC()
		opts.DeliverAt = &t
	}
	m, err := h.svc.Compose(r.Context(), h.actor(r), r.PathValue("id"), req.Body, req.ReplyTo, nil, req.Mentions, opts)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, m)
}

// react puts an emoji on a message, or takes it off. The same route both
// ways, with a flag: two routes would be two places to check membership.
func (h *Handler) react(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Emoji string `json:"emoji"`
		On    *bool  `json:"on"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	on := true
	if req.On != nil {
		on = *req.On
	}
	if err := h.svc.React(r.Context(), h.actor(r), r.PathValue("id"), req.Emoji, on); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) save(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On *bool `json:"on"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	on := true
	if req.On != nil {
		on = *req.On
	}
	if err := h.svc.SaveMessage(r.Context(), h.actor(r), r.PathValue("id"), on); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) saved(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	ms, err := h.svc.Saved(r.Context(), h.actor(r), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	if ms == nil {
		ms = []*Message{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"messages": ms})
}

func (h *Handler) markRead(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.MarkRead(r.Context(), h.actor(r), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "read"})
}

func (h *Handler) typing(w http.ResponseWriter, r *http.Request) {
	// The REST form says "typing"; the socket form carries the flag.
	h.svc.Typing(r.Context(), h.actor(r), r.PathValue("id"), true)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) edit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body string `json:"body"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	m, err := h.svc.Edit(r.Context(), h.actor(r), r.PathValue("id"), req.Body)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, m)
}

// ---------------------------------------------------------------------------
// Per-person preferences
// ---------------------------------------------------------------------------

func (h *Handler) prefs(w http.ResponseWriter, r *http.Request) {
	var req Prefs
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	if err := h.svc.SetPrefs(r.Context(), h.actor(r), r.PathValue("id"), req); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) markUnread(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.MarkUnread(r.Context(), h.actor(r), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) forget(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.ForgetConversation(r.Context(), h.actor(r), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Channels and groups
// ---------------------------------------------------------------------------

func (h *Handler) createChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		Topic      string `json:"topic"`
		Visibility string `json:"visibility"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	c, err := h.svc.CreateChannel(r.Context(), h.actor(r), req.Name, req.Topic, req.Visibility)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, c)
}

func (h *Handler) browseChannels(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.svc.BrowseChannels(r.Context(), h.actor(r), r.URL.Query().Get("q"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	if list == nil {
		list = []ChannelSummary{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"channels": list})
}

func (h *Handler) joinChannel(w http.ResponseWriter, r *http.Request) {
	c, err := h.svc.JoinChannel(r.Context(), h.actor(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

func (h *Handler) leave(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Leave(r.Context(), h.actor(r), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) members(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ms, total, err := h.svc.Members(r.Context(), h.actor(r), r.PathValue("id"), offset, limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	if ms == nil {
		ms = []Member{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"members": ms, "total": total})
}

func (h *Handler) addMembers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Emails  []string `json:"emails"`
		History History  `json:"history"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	if req.History.Mode == "" {
		req.History.Mode = HistoryAll
	}
	c, skipped, err := h.svc.AddMembers(r.Context(), h.actor(r), r.PathValue("id"), req.Emails, req.History)
	if err != nil {
		h.fail(w, err)
		return
	}
	if skipped == nil {
		skipped = []string{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"conversation": c, "skipped": skipped})
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.RemoveMember(r.Context(), h.actor(r), r.PathValue("id"), r.PathValue("user")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) rename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
		Topic string `json:"topic"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	c, err := h.svc.Rename(r.Context(), h.actor(r), r.PathValue("id"), req.Title, req.Topic)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

// upload posts one message with files: multipart, fields "body" and
// "replyTo", any number of "file" parts up to the cap. Membership is checked
// before the first byte of a file is read, and the whole request is bounded
// so a client cannot stream forever.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	a := h.actor(r)
	convID := r.PathValue("id")
	if err := h.svc.CanPost(r.Context(), a, convID); err != nil {
		h.fail(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, int64(MaxAttachmentsPerMessage)*MaxAttachmentBytes+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "expected multipart form data")
		return
	}

	var body, replyTo string
	var mentions []string
	var staged []*Attachment
	var opts SendOptions
	discard := func() { h.svc.DiscardStaged(staged) }
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			discard()
			httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed upload")
			return
		}
		switch part.FormName() {
		case "body":
			b, _ := io.ReadAll(io.LimitReader(part, MaxBodyLength+1))
			body = string(b)
		case "replyTo":
			b, _ := io.ReadAll(io.LimitReader(part, 128))
			replyTo = string(b)
		case "mentions":
			b, _ := io.ReadAll(io.LimitReader(part, 2048))
			for _, id := range strings.Split(string(b), ",") {
				if id = strings.TrimSpace(id); id != "" {
					mentions = append(mentions, id)
				}
			}
		case "priority":
			b, _ := io.ReadAll(io.LimitReader(part, 32))
			opts.Priority = strings.TrimSpace(string(b))
		case "scheduleAt":
			b, _ := io.ReadAll(io.LimitReader(part, 32))
			if ms, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && ms > 0 {
				t := time.UnixMilli(ms).UTC()
				opts.DeliverAt = &t
			}
		case "meta":
			b, _ := io.ReadAll(io.LimitReader(part, MaxMetaBytes+1))
			opts.Meta = string(b)
		case "file":
			if len(staged) >= MaxAttachmentsPerMessage {
				discard()
				h.fail(w, ErrTooManyFiles)
				return
			}
			at, err := h.svc.StageUpload(r.Context(), a, part.FileName(), part.Header.Get("Content-Type"), part)
			if err != nil {
				discard()
				h.fail(w, err)
				return
			}
			staged = append(staged, at)
		default:
			_, _ = io.Copy(io.Discard, part)
		}
		_ = part.Close()
	}

	m, err := h.svc.Compose(r.Context(), a, convID, body, replyTo, staged, mentions, opts)
	if err != nil {
		discard()
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, m)
}

// search: messages you can read whose text contains the query.
func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ms, err := h.svc.SearchIn(r.Context(), h.actor(r), r.URL.Query().Get("q"), r.URL.Query().Get("conv"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	if ms == nil {
		ms = []*Message{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"messages": ms})
}

// replies is one channel thread; history is what a message said before.
func (h *Handler) replies(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ms, err := h.svc.Replies(r.Context(), h.actor(r), r.PathValue("id"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	if ms == nil {
		ms = []*Message{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"messages": ms})
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	hs, err := h.svc.History(r.Context(), h.actor(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"edits": hs})
}

// Status: what I say about myself; what my peers say about themselves.
func (h *Handler) setStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Status string     `json:"status"`
		Note   string     `json:"note"`
		Until  *time.Time `json:"until"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	st, err := h.svc.SetStatus(r.Context(), h.actor(r), req.Status, req.Note, req.Until)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, st)
}

func (h *Handler) statuses(w http.ResponseWriter, r *http.Request) {
	var ids []string
	for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	got, err := h.svc.Statuses(r.Context(), h.actor(r), ids)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"statuses": got})
}

// vote records my choice on a poll and returns the message with its tally.
func (h *Handler) vote(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Options []int `json:"options"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	if len(req.Options) > MaxPollOptions {
		httpx.Error(w, http.StatusBadRequest, "bad_vote", "too many options")
		return
	}
	m, err := h.svc.Vote(r.Context(), h.actor(r), r.PathValue("id"), req.Options)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, m)
}

func (h *Handler) deleteChannel(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteChannel(r.Context(), h.actor(r), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Blocks: mine to see and change; nobody else's business.
func (h *Handler) blocks(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.Blocks(r.Context(), h.actor(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"blocked": list})
}

func (h *Handler) block(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Block(r.Context(), h.actor(r), r.PathValue("user")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) unblock(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Unblock(r.Context(), h.actor(r), r.PathValue("user")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// meetingChat is the room's conversation, for somebody who was in it.
func (h *Handler) meetingChat(w http.ResponseWriter, r *http.Request) {
	c, err := h.svc.MeetingChat(r.Context(), h.actor(r), r.PathValue("room"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, c)
}

// avatar: a group's or channel's picture, set by a member (multipart "file"),
// served to members, image types only.
func (h *Handler) setAvatar(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxAvatarBytes+(256<<10))
	mr, err := r.MultipartReader()
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "expected multipart form data")
		return
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed upload")
			return
		}
		if part.FormName() != "file" {
			_, _ = io.Copy(io.Discard, part)
			continue
		}
		if err := h.svc.SetAvatar(r.Context(), h.actor(r), r.PathValue("id"), part.FileName(), part.Header.Get("Content-Type"), part); err != nil {
			h.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	httpx.Error(w, http.StatusBadRequest, "bad_request", "no file")
}

func (h *Handler) avatar(w http.ResponseWriter, r *http.Request) {
	mime, f, mod, err := h.svc.OpenAvatar(r.Context(), h.actor(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeContent(w, r, "", mod, f)
}

func (h *Handler) pin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MessageID string `json:"messageId"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	if err := h.svc.Pin(r.Context(), h.actor(r), r.PathValue("id"), req.MessageID); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// download streams one file to a member of its conversation. Types a browser
// could execute go out as opaque downloads; see ServeType.
func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	at, f, mod, err := h.svc.OpenAttachment(r.Context(), h.actor(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	defer f.Close()
	ct, inline := ServeType(at.MIME)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", ContentDisposition(inline, at.Name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	// Blobs are immutable, so the modtime is a fine validator and Range
	// requests (a video scrubbing) come for free.
	http.ServeContent(w, r, "", mod, f)
}

// shared is the conversation's files and links, for the Shared view.
func (h *Handler) shared(w http.ResponseWriter, r *http.Request) {
	a := h.actor(r)
	convID := r.PathValue("id")
	files, err := h.svc.SharedFiles(r.Context(), a, convID, 100)
	if err != nil {
		h.fail(w, err)
		return
	}
	links, err := h.svc.SharedLinks(r.Context(), a, convID, 100)
	if err != nil {
		h.fail(w, err)
		return
	}
	if files == nil {
		files = []Attachment{}
	}
	if links == nil {
		links = []Link{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"files": files, "links": links})
}

// forward carries one message into the named conversations.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To []string `json:"to"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "bad_request", "malformed request")
		return
	}
	msgs, err := h.svc.Forward(r.Context(), h.actor(r), r.PathValue("id"), req.To)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"messages": msgs})
}

// hide is "delete for me". No body: the only thing it can mean is "this one,
// for me", and the id is in the path.
func (h *Handler) hide(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Hide(r.Context(), h.actor(r), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.Context(), h.actor(r), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---------------------------------------------------------------------------
// Push socket
// ---------------------------------------------------------------------------

func (h *Handler) socket(w http.ResponseWriter, r *http.Request) {
	a := h.actor(r)
	if a.ID == "" {
		httpx.Error(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: h.Origin,
		// Compression is disabled for the same reason as the meeting socket:
		// DEFLATE on short binary payloads routinely expands them.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		h.log.Warn("chat socket upgrade failed", "err", err)
		return
	}
	ws.SetReadLimit(32 << 10)

	tx, rx := wire.NewPreloadedPair()
	s := &sock{ws: ws, userID: a.ID, device: deviceOf(r.URL.Query().Get("d")), tx: tx, out: make(chan []byte, maxPending)}

	first := !h.hub.Online(a.ID)
	h.svc.SocketOpened(r.Context(), a.ID)
	h.hub.add(s)
	if first {
		h.svc.AnnouncePresence(r.Context(), a.ID, true)
	}
	// Who is already here. A presence push only ever said "X arrived"; a
	// client that arrived after X never heard it and drew X offline. The
	// snapshot goes to this socket alone, one frame per online peer.
	for _, p := range h.svc.OnlinePeers(r.Context(), a.ID) {
		h.hub.PresenceTo(s, p, true)
	}
	defer func() {
		h.hub.remove(s)
		// Departure is announced only once the last socket has been gone
		// for OfflineGrace: a reconnecting client must not flicker its
		// peers' dots. Two tabs open, one closed, is not a departure at
		// all. Last-seen is stamped now, the moment they were last here.
		if !h.hub.Connected(a.ID) {
			h.svc.SocketClosed(context.Background(), a.ID)
			time.AfterFunc(OfflineGrace, func() {
				if !h.hub.Online(a.ID) {
					h.svc.AnnouncePresence(context.Background(), a.ID, false)
					h.svc.NudgeAutoStatus()
				}
			})
		}
		// A device that vanished mid-idle must not leave the person idle.
		h.svc.NudgeAutoStatus()
		s.close("normal closure")
	}()

	ctx := r.Context()
	go s.writePump(ctx)

	// The client speaks only two things on this socket: a heartbeat, and a
	// typing indicator. Everything that changes state goes over REST, where
	// it gets CSRF protection and a status code.
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary {
			s.close("binary frames only")
			return
		}
		rd := wire.NewReader(data, rx)
		hdr, err := rd.ReadHeader()
		if err != nil {
			return
		}
		switch hdr.Op {
		case wire.OpPing:
			// Through sock.send for the same reason as every other frame:
			// this encodes against the shared transmit dictionary, so it must
			// reach the queue in encode order or the connection desyncs.
			if s.send(func(pw *wire.Writer) {
				pw.WriteHeader(wire.OpPong, 0, false)
			}) == sendOverflow {
				s.close("outbound queue overflow")
				return
			}
		case wire.OpTyping:
			// {conv, on}. A client from before the flag sends the
			// conversation alone; that is "typing".
			convID := rd.Str()
			on := true
			if rd.Left() > 0 {
				on = rd.Bool()
				rd.Bits(7)
			}
			if rd.Err() == nil && convID != "" {
				h.svc.Typing(ctx, a, convID, on)
			}
		case wire.OpIdle:
			idle := rd.Bool()
			rd.Bits(7)
			if rd.Err() == nil {
				h.svc.DeviceIdle(ctx, s, idle)
			}
		case wire.OpRing:
			target := rd.Str()
			room := rd.Str()
			if rd.Err() == nil {
				h.svc.Ring(ctx, a, target, room)
			}
		case wire.OpRingAnswer:
			caller := rd.Str()
			room := rd.Str()
			accepted := rd.Bool()
			rd.Bits(7)
			if rd.Err() == nil {
				h.svc.RingAnswer(ctx, a, caller, room, accepted)
			}
		default:
			// Unknown opcodes are ignored, not fatal.
		}
	}
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, ErrForbidden):
		httpx.Error(w, http.StatusForbidden, "forbidden", "you are not in that conversation")
	case errors.Is(err, ErrEmptyBody):
		httpx.Error(w, http.StatusBadRequest, "empty", "a message needs a body")
	case errors.Is(err, ErrTooLong):
		httpx.Error(w, http.StatusBadRequest, "too_long", "that message is too long")
	case errors.Is(err, ErrNoMembers):
		httpx.Error(w, http.StatusBadRequest, "no_members", "add at least one other person")
	case errors.Is(err, ErrTooManyTargets):
		httpx.Error(w, http.StatusBadRequest, "too_many", "forward to at most 10 conversations at once")
	case errors.Is(err, ErrChannelExists):
		httpx.Error(w, http.StatusConflict, "exists", "Channel name is already taken.")
	case errors.Is(err, ErrTeamName):
		httpx.Error(w, http.StatusBadRequest, "team_name", "Give your team a name")
	case errors.Is(err, ErrTeamNotOwner):
		httpx.Error(w, http.StatusForbidden, "not_owner", "only team owners can do that")
	case errors.Is(err, ErrTeamNotMember):
		httpx.Error(w, http.StatusForbidden, "forbidden", "Either the team has been deleted or you are not a member.")
	case errors.Is(err, ErrTeamPrivate):
		httpx.Error(w, http.StatusForbidden, "private", "that team is private: ask an owner to add you, or use its team code")
	case errors.Is(err, ErrTeamCode):
		httpx.Error(w, http.StatusNotFound, "bad_code", "no team has that code")
	case errors.Is(err, ErrTeamFull):
		httpx.Error(w, http.StatusConflict, "team_full", "this team has reached its limit of members")
	case errors.Is(err, ErrTeamChannels):
		httpx.Error(w, http.StatusConflict, "too_many_channels", "this team has reached its limit of channels")
	case errors.Is(err, ErrTeamChannel):
		httpx.Error(w, http.StatusConflict, "team_channel", "Everyone on the team has access. Join or leave the team instead.")
	case errors.Is(err, ErrGeneralChannel):
		httpx.Error(w, http.StatusConflict, "general", "the General channel can't be deleted or renamed")
	case errors.Is(err, ErrNotOnTeam):
		httpx.Error(w, http.StatusConflict, "not_on_team", "only people on the team can be added to its channels")
	case errors.Is(err, ErrLastOwner):
		httpx.Error(w, http.StatusConflict, "last_owner", "a team needs an owner: make someone else an owner first")
	case errors.Is(err, ErrBadRequest):
		httpx.Error(w, http.StatusBadRequest, "bad_request", "that request is not valid")
	case errors.Is(err, ErrBadName):
		httpx.Error(w, http.StatusBadRequest, "bad_name", "a channel needs a name")
	case errors.Is(err, ErrNotOwner):
		httpx.Error(w, http.StatusForbidden, "not_owner", "only the owner can do that")
	case errors.Is(err, ErrNotJoinable):
		httpx.Error(w, http.StatusForbidden, "private", "that channel is private")
	case errors.Is(err, ErrBadPriority):
		httpx.Error(w, http.StatusBadRequest, "bad_priority", "priority must be important or urgent")
	case errors.Is(err, ErrBadSchedule):
		httpx.Error(w, http.StatusBadRequest, "bad_schedule", "a scheduled time must be between 30 seconds and 30 days from now")
	case errors.Is(err, ErrBadMeta):
		httpx.Error(w, http.StatusBadRequest, "bad_meta", "the card payload must be a small JSON object")
	case errors.Is(err, ErrNotAnImage):
		httpx.Error(w, http.StatusBadRequest, "not_image", "a picture must be a PNG, JPEG, GIF or WebP")
	case errors.Is(err, ErrNotInConv):
		httpx.Error(w, http.StatusBadRequest, "not_here", "that message is not in this conversation")
	case errors.Is(err, ErrDirectFixed):
		httpx.Error(w, http.StatusBadRequest, "direct", "a direct conversation's members cannot change")
	case errors.Is(err, ErrTooLarge):
		httpx.Error(w, http.StatusRequestEntityTooLarge, "too_large", "files may be at most 25 MB each")
	case errors.Is(err, ErrTooManyFiles):
		httpx.Error(w, http.StatusBadRequest, "too_many_files", "at most 10 files in one message")
	case errors.Is(err, ErrNoFiles):
		httpx.Error(w, http.StatusNotImplemented, "no_files", "file sharing is not enabled on this server")
	case errors.Is(err, ErrExternalClosed):
		httpx.Error(w, http.StatusForbidden, "external_closed",
			"This deployment only allows conversations inside your organisation.")
	case errors.Is(err, ErrSelfDirect):
		httpx.Error(w, http.StatusBadRequest, "self", "you cannot message yourself")
	case errors.Is(err, ErrBlocked):
		// The same words to both sides, on purpose.
		httpx.Error(w, http.StatusForbidden, "blocked", "You can't message this person right now.")
	case errors.Is(err, ErrTooManyBlocks):
		httpx.Error(w, http.StatusBadRequest, "too_many_blocks", "your block list is full")
	case errors.Is(err, ErrBadAnnouncement):
		httpx.Error(w, http.StatusBadRequest, "bad_announcement", "an announcement needs a headline of 80 characters or fewer")
	case errors.Is(err, ErrBadStatus):
		httpx.Error(w, http.StatusBadRequest, "bad_status", "status must be available, busy, dnd or away, with a note of 120 characters or fewer")
	case errors.Is(err, ErrNotAPoll):
		httpx.Error(w, http.StatusBadRequest, "not_a_poll", "that message is not a poll")
	case errors.Is(err, ErrBadVote):
		httpx.Error(w, http.StatusBadRequest, "bad_vote", "not an option on this poll")
	case errors.Is(err, ErrBadPoll):
		httpx.Error(w, http.StatusBadRequest, "bad_poll", "a poll needs a question and two to ten options")
	default:
		h.log.Error("messaging request failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal", "something went wrong")
	}
}

// calls is the call log for one direct conversation, for the thread view.
func (h *Handler) calls(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.CallsFor(r.Context(), h.actor(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if list == nil {
		list = []*Call{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"calls": list})
}
