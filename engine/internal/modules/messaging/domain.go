// Package messaging is persistent chat: conversations that outlive any
// meeting.
//
// The distinction from in-meeting chat is deliberate. A message sent in a
// meeting belongs to that meeting and disappears with it, which is what
// people expect and what keeps a transient side-channel from becoming an
// unsearchable record. A message sent here belongs to a conversation, and is
// still there tomorrow.
package messaging

import (
	"context"
	"errors"
	"time"
)

// Kind distinguishes a two-person thread from a named group.
type Kind string

const (
	KindDirect  Kind = "direct"
	KindGroup   Kind = "group"
	KindChannel Kind = "channel"
)

// Conversation is a thread with a fixed membership.
type Conversation struct {
	ID        string    `json:"id"`
	Kind      Kind      `json:"kind"`
	Title     string    `json:"title"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`

	// LastMessageAt drives the ordering of the conversation list, which is
	// the only ordering anybody ever wants.
	LastMessageAt time.Time `json:"lastMessageAt"`

	// SelfChat marks the thread somebody keeps with themselves, so the
	// interface can say so rather than showing a conversation with a person
	// who appears to be absent.
	SelfChat bool `json:"selfChat,omitempty"`

	// Channels and groups (see channels.go).
	Topic       string `json:"topic,omitempty"`
	Visibility  string `json:"visibility,omitempty"`
	MemberCount int    `json:"memberCount"`
	// MyRole is the viewer's role, filled on read.
	MyRole string `json:"myRole,omitempty"`
	// The viewer's own choices about this conversation (see prefs.go).
	Favourite bool `json:"favourite,omitempty"`
	Muted     bool `json:"muted,omitempty"`

	// MeetingRoom names the room this conversation was born in, for a
	// meeting chat (see meetingchat.go); empty otherwise.
	MeetingRoom string `json:"meetingRoom,omitempty"`

	// TeamID is the team a channel belongs to (teams.go); empty for a
	// conversation that is not a team's channel.
	TeamID string `json:"teamId,omitempty"`
	// Blocked is set on a direct conversation whose other person the viewer
	// has blocked. Their own decision, shown back to them; never the
	// reverse.
	Blocked bool `json:"blocked,omitempty"`

	// Groups, the rest of the way (see groups.go).
	Avatar          bool   `json:"avatar,omitempty"` // has a picture; fetch it at /avatar
	PinnedMessageID string `json:"pinnedMessageId,omitempty"`
	Pinned          *Quote `json:"pinned,omitempty"` // filled on read
	Mentions        int    `json:"mentions"`         // the viewer's unread mentions here

	// Members is the full list for direct conversations and small groups;
	// above bigConversation members it is left empty and MemberCount stands
	// in, with the people panel paging through the membership on demand.
	Members []Member `json:"members"`

	// Derived per-viewer, filled by the service rather than stored.
	Unread  int    `json:"unread"`
	Preview string `json:"preview,omitempty"`
	Display string `json:"display,omitempty"`
}

// Member is one participant in a conversation.
type Member struct {
	UserID   string    `json:"userId"`
	Email    string    `json:"email"`
	Name     string    `json:"name"`
	JoinedAt time.Time `json:"joinedAt"`
	LastRead time.Time `json:"lastRead"`
	Role     string    `json:"role,omitempty"`
}

// Message is one posted message.
type Message struct {
	ID             string     `json:"id"`
	ConversationID string     `json:"conversationId"`
	SenderID       string     `json:"senderId"`
	SenderName     string     `json:"senderName"`
	Body           string     `json:"body"`
	CreatedAt      time.Time  `json:"createdAt"`
	EditedAt       *time.Time `json:"editedAt,omitempty"`
	Deleted        bool       `json:"deleted"`

	// ReplyTo names the message this one answers, empty for most messages.
	//
	// An id rather than a copy of the quoted text: a reply to a message that
	// is later edited should show what it says now, not what it said when the
	// reply was written. Every chat product converged on that, because the
	// alternative preserves somebody's typo forever in the quote.
	ReplyTo string `json:"replyTo,omitempty"`

	// Quoted is the message ReplyTo names, filled in on read so the client can
	// draw the quote without a second request per message.
	Quoted *Quote `json:"quoted,omitempty"`

	// Reactions on this message, grouped by emoji.
	Reactions []Reaction `json:"reactions,omitempty"`

	// Forwarded marks a message carried here from another conversation. A
	// flag, not a link: see the migration.
	Forwarded bool `json:"forwarded,omitempty"`

	// Attachments are the files on this message, filled in on read.
	Attachments []Attachment `json:"attachments,omitempty"`

	// System marks a membership event ("added Priya"): drawn as an event,
	// not as something the sender said, and never counted as unread.
	System bool `json:"system,omitempty"`

	// Mentions are the member ids this message names.
	Mentions []string `json:"mentions,omitempty"`

	// Composition (see compose.go).
	Priority  string     `json:"priority,omitempty"`  // "", important, urgent
	DeliverAt *time.Time `json:"deliverAt,omitempty"` // set on a scheduled message
	Meta      string     `json:"meta,omitempty"`      // opaque JSON object for cards

	// Saved reports whether the person reading saved this message. Per
	// reader, so it is filled in on read rather than stored on the message.
	Saved bool `json:"saved,omitempty"`

	// Poll is the tally on a poll message (Meta carries the question and
	// options), filled in on read and carried on the push. See polls.go.
	Poll *PollTally `json:"poll,omitempty"`

	// Thread summarises the replies under a channel post: filled in on
	// read for root posts in channels. See threads.go.
	Thread *ThreadSummary `json:"thread,omitempty"`
}

// ThreadSummary is what a channel post shows about its replies.
type ThreadSummary struct {
	Count     int        `json:"count"`
	LastAt    *time.Time `json:"lastAt,omitempty"`
	LastNames []string   `json:"lastNames,omitempty"` // up to three most recent repliers
}

// Edit is one earlier version of a message.
type Edit struct {
	Body     string    `json:"body"`
	EditedAt time.Time `json:"editedAt"`
}

// PollTally is where a poll stands: per option, how many and (unless the
// poll is anonymous) up to twelve of who; and the reader's own choices.
type PollTally struct {
	Counts []int      `json:"counts"`
	Voters [][]string `json:"voters,omitempty"`
	Mine   []int      `json:"mine"`
	Total  int        `json:"total"` // distinct people who voted
}

// Status is what a person says about their own availability -- or, when
// Auto is set, what the rooms say for them: in a call while they are in
// one, presenting while they share their screen. Nothing else is
// automatic: the calendar does not set a status, and the moment the
// connection to the room ends the person reads as whatever they had
// picked, or available.
type Status struct {
	UserID    string     `json:"userId"`
	Status    string     `json:"status"` // available, busy, dnd, away, offline; incall, presenting (auto)
	Note      string     `json:"note,omitempty"`
	Until     *time.Time `json:"until,omitempty"`
	UpdatedAt time.Time  `json:"updatedAt"`
	Auto      bool       `json:"auto,omitempty"`
	// Presence, beside the status: whether a socket of theirs is up (false
	// as well when they chose to appear offline), and when they were last
	// active when it is not -- or when they are away. Hidden for someone
	// appearing offline.
	Online   bool       `json:"online"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}

// Quote is as much of a replied-to message as a reply needs to show.
type Quote struct {
	ID         string `json:"id"`
	SenderID   string `json:"senderId"`
	SenderName string `json:"senderName"`
	Body       string `json:"body"`
	Deleted    bool   `json:"deleted"`
}

// Reaction is one emoji on one message, and who used it.
//
// Grouped by emoji rather than listed per person, because that is how it is
// read: "four thumbs up, one of them mine" is the question, and a list of
// twelve rows that the client then has to group is the same data shaped for
// the database instead of for the screen.
type Reaction struct {
	Emoji string   `json:"emoji"`
	Count int      `json:"count"`
	Mine  bool     `json:"mine"`
	Who   []string `json:"who,omitempty"`
}

// Call is one ring between two accounts, the way a phone keeps a call log.
//
// It lives in messaging, not scheduling, because a call is part of a
// conversation: it appears in the thread between the two people, in time
// order with their messages, the way WhatsApp shows it. It is deliberately
// NOT a meeting for the dashboard's "recent" list -- a call log in a meeting
// history reads as clutter, and the user said so.
type Call struct {
	ID        string    `json:"id"`
	Room      string    `json:"room"`
	CallerID  string    `json:"callerId"`
	CalleeID  string    `json:"calleeId"`
	Kind      string    `json:"kind"` // "video" or "audio"
	StartedAt time.Time `json:"startedAt"`
	// Answered is true once the callee accepted. False and past its ring
	// window means a missed call.
	Answered bool `json:"answered"`
	// Seconds the call lasted, from the room's attendance. Zero while it is
	// still going or was never answered.
	Seconds int64 `json:"seconds"`
}

// MaxBodyLength bounds one message. Generous for prose, small enough that a
// conversation cannot be used as a file store.
const MaxBodyLength = 8000

var (
	ErrNotFound   = errors.New("messaging: not found")
	ErrBadRequest = errors.New("messaging: that request is not valid")
	ErrForbidden  = errors.New("messaging: not a member of that conversation")
	ErrEmptyBody  = errors.New("messaging: a message needs a body")
	ErrTooLong    = errors.New("messaging: message is too long")
	ErrNoMembers  = errors.New("messaging: a conversation needs at least one other person")
	// ErrTooManyTargets: a forward names more conversations than one person
	// plausibly means at once.
	ErrTooManyTargets = errors.New("messaging: too many conversations to forward to")
	// ErrSelfDirect is kept for the one caller that still means it: opening a
	// direct conversation with yourself BY ACCOUNT ID, from a meeting roster,
	// where picking your own tile and being sent to your notes would be a
	// surprise rather than a feature. Typing your own address, or pressing the
	// explicit button, opens your notes.
	ErrSelfDirect = errors.New("messaging: cannot open a direct conversation with yourself")

	// Blocks. ErrBlocked is returned to both sides of a direct conversation
	// while either has blocked the other; the handler says the same thing
	// to both, so being blocked is not something the API confirms.
	ErrBlocked       = errors.New("messaging: this conversation is blocked")
	ErrTooManyBlocks = errors.New("messaging: too many blocked people")

	ErrBadStatus       = errors.New("messaging: not a status")
	ErrBadAnnouncement = errors.New("messaging: an announcement needs a headline of 80 characters or fewer")
	ErrNotAPoll        = errors.New("messaging: that message is not a poll")
	ErrBadVote         = errors.New("messaging: not an option on this poll")
	ErrBadPoll         = errors.New("messaging: a poll needs a question and two to ten options")

	// ErrExternalClosed is returned when this deployment does not permit
	// conversations outside its own organisation.
	ErrExternalClosed = errors.New("messaging: this deployment does not allow conversations outside the organisation")
)

// Repository is the persistence port.
type Repository interface {
	CreateConversation(ctx context.Context, c *Conversation) error
	ConversationByID(ctx context.Context, id string) (*Conversation, error)
	// DirectBetween finds an existing two-person thread, so that opening a
	// chat with the same person twice does not produce two threads.
	DirectBetween(ctx context.Context, a, b string) (*Conversation, error)

	// SelfConversation finds the one-member thread somebody keeps for
	// themselves. Separate from DirectBetween because that query counts two
	// distinct members and this one has exactly one, which is the whole
	// difference between a conversation and a notebook.
	SelfConversation(ctx context.Context, userID string) (*Conversation, error)
	ListConversations(ctx context.Context, userID string) ([]*Conversation, error)
	IsMember(ctx context.Context, convID, userID string) (bool, error)

	AddMessage(ctx context.Context, m *Message) error
	ListMessages(ctx context.Context, convID string, before time.Time, limit int) ([]*Message, error)
	MessageByID(ctx context.Context, id string) (*Message, error)
	SoftDeleteMessage(ctx context.Context, id string) error
	EditMessage(ctx context.Context, id, body string, at time.Time) error

	// React adds or removes one emoji from one person on one message, and
	// reports which it did. Idempotent in both directions: the same emoji
	// twice is the same fact, and removing one that is not there is not an
	// error, because two clicks racing each other must not produce a failure
	// somebody has to understand.
	React(ctx context.Context, messageID, userID, emoji string, on bool) (added bool, err error)

	// ReactionsFor fills reaction groups onto a page of messages in one query,
	// rather than one query per message.
	ReactionsFor(ctx context.Context, userID string, ids []string) (map[string][]Reaction, error)

	// QuotesFor loads the messages a page of replies points at, in one query.
	QuotesFor(ctx context.Context, ids []string) (map[string]*Quote, error)

	// Save marks or unmarks a message for one person, and lists what they
	// have saved.
	Save(ctx context.Context, userID, messageID string, on bool) error
	SavedIDs(ctx context.Context, userID string, ids []string) (map[string]bool, error)
	SavedMessages(ctx context.Context, userID string, limit int) ([]*Message, error)

	// Hide is "delete for me": the pair (user, message) that keeps a message
	// out of one person's view. HiddenIDs answers for a page of them.
	Hide(ctx context.Context, userID, messageID string) error
	HiddenIDs(ctx context.Context, userID string, ids []string) (map[string]bool, error)

	// Composition (see compose.go).
	DueScheduled(ctx context.Context, now time.Time, limit int) ([]string, error)
	Deliver(ctx context.Context, id string, at time.Time) (*Message, error)
	// convID narrows a search to one conversation; "" searches them all.
	SearchMessages(ctx context.Context, userID, q, convID string, limit int) ([]*Message, error)
	ReindexIfNeeded(ctx context.Context) (int, error)

	// Threads and edit history.
	ListRoots(ctx context.Context, convID string, before time.Time, limit int) ([]*Message, error)
	ListReplies(ctx context.Context, rootID string, limit int) ([]*Message, error)
	ThreadSummaries(ctx context.Context, rootIDs []string) (map[string]*ThreadSummary, error)
	EditHistory(ctx context.Context, messageID string) ([]Edit, error)

	// Status and polls.
	SetStatus(ctx context.Context, st Status) error
	TouchSeen(ctx context.Context, userID string, at time.Time) error
	LastSeenFor(ctx context.Context, ids []string) (map[string]time.Time, error)
	StatusesFor(ctx context.Context, ids []string) (map[string]Status, error)
	SetVotes(ctx context.Context, messageID, userID string, options []int, at time.Time) error
	PollTalliesFor(ctx context.Context, userID string, messageIDs []string) (map[string]*PollTally, error)
	DeleteConversation(ctx context.Context, convID string) ([]Attachment, error)

	// Teams (teams.go).
	CreateTeam(ctx context.Context, t *Team, owner TeamMember) error
	TeamByID(ctx context.Context, id string) (*Team, error)
	TeamByCode(ctx context.Context, code string) (*Team, error)
	TeamRole(ctx context.Context, teamID, userID string) (string, error)
	MyTeams(ctx context.Context, userID string) ([]*Team, error)
	TeamChannelsFor(ctx context.Context, teamIDs []string, userID string) (map[string][]TeamChannel, error)
	TeamConversations(ctx context.Context, teamID string) ([]*Conversation, error)
	PublicTeams(ctx context.Context, userID, q string, limit int) ([]*Team, error)
	TeamMembers(ctx context.Context, teamID string, limit int) ([]TeamMember, error)
	AddTeamMember(ctx context.Context, teamID string, m TeamMember) (bool, error)
	RemoveTeamMember(ctx context.Context, teamID, userID string) error
	SetTeamRole(ctx context.Context, teamID, userID, role string) error
	TeamOwnerCount(ctx context.Context, teamID string) (int, error)
	UpdateTeam(ctx context.Context, t *Team) error
	SetTeamCode(ctx context.Context, teamID, code string) error
	DeleteTeam(ctx context.Context, teamID string) error
	CountTeamChannels(ctx context.Context, teamID string) (int, error)

	// The activity feed (activity.go).
	AddActivity(ctx context.Context, rows []activityRow) error
	ActivityPage(ctx context.Context, userID string, beforeAt int64, beforeID, kind string, unread bool, limit int) ([]*Activity, error)
	ActivityByIDs(ctx context.Context, ids []string) ([]*Activity, error)
	ActivityUnread(ctx context.Context, userID string) (int, error)
	MarkActivityRead(ctx context.Context, userID string, ids []string, all bool) ([]string, error)
	MarkActivityReadIn(ctx context.Context, userID, convID string) ([]string, error)
	RemoveReactionActivity(ctx context.Context, messageID, actorID, emoji string) (userID, id string, err error)

	// Blocks.
	Block(ctx context.Context, userID, blockedID string, at time.Time) error
	Unblock(ctx context.Context, userID, blockedID string) error
	BlockedIDs(ctx context.Context, userID string) ([]string, error)
	BlockedEither(ctx context.Context, a, b string) (bool, error)
	BlockCount(ctx context.Context, userID string) (int, error)

	// The meeting chat (see meetingchat.go). Nil, nil when the room has none.
	ConversationByMeetingRoom(ctx context.Context, room string) (*Conversation, error)

	// Groups, the rest of the way (see groups.go).
	HideBefore(ctx context.Context, convID, userID string, before time.Time) error
	MentionsFor(ctx context.Context, messageIDs []string) (map[string][]string, error)
	SetAvatar(ctx context.Context, convID, blob, mime string) (old string, err error)
	Avatar(ctx context.Context, convID string) (blob, mime string, err error)
	SetPinned(ctx context.Context, convID, messageID string) error

	// Per-person preferences and cursors (see prefs.go).
	SetPrefs(ctx context.Context, convID, userID string, favourite, muted, hidden *bool) error
	MarkUnread(ctx context.Context, convID, userID string) error
	ForgetConversation(ctx context.Context, convID, userID string) error

	// Membership, for groups and channels (see channels.go).
	AddMembers(ctx context.Context, convID string, ms []Member) error
	RemoveMember(ctx context.Context, convID, userID string) error
	MemberOf(ctx context.Context, convID, userID string) (*Member, error)
	MemberIDs(ctx context.Context, convID string) ([]string, error)
	MembersPage(ctx context.Context, convID string, offset, limit int) ([]Member, int, error)
	PromoteOldest(ctx context.Context, convID string) error
	UpdateConversation(ctx context.Context, convID, title, topic string) error
	PublicChannels(ctx context.Context, viewerID, q string, limit int) ([]ChannelSummary, error)

	// The conversation list, in two queries rather than two per row:
	// unread counts for everything this person is in, and the newest
	// message of each named conversation.
	UnreadCounts(ctx context.Context, userID string) (map[string]int, error)
	LastMessages(ctx context.Context, convIDs []string) (map[string]*Message, error)

	// Files. AddMessageWithAttachments writes the message and its rows in
	// one transaction; AttachmentsFor fills a page; SharedFiles lists a
	// conversation's files newest first.
	AddMessageWithAttachments(ctx context.Context, m *Message, atts []*Attachment) error
	AttachmentsFor(ctx context.Context, messageIDs []string) (map[string][]Attachment, error)
	AttachmentByID(ctx context.Context, id string) (*Attachment, error)
	SharedFiles(ctx context.Context, convID string, limit int) ([]Attachment, error)

	// PeersOf lists everyone who shares at least one conversation with a
	// user. Presence is pushed to exactly those people rather than
	// broadcast, which keeps a sign-in from waking every socket on the node.
	PeersOf(ctx context.Context, userID string) ([]string, error)

	MarkRead(ctx context.Context, convID, userID string, at time.Time) error
	UnreadCount(ctx context.Context, convID, userID string) (int, error)
	LastMessage(ctx context.Context, convID string) (*Message, error)

	// The call log between two accounts. See Call.
	RecordCall(ctx context.Context, c *Call) error
	AnswerCall(ctx context.Context, room string, accepted bool) error
	CallsBetween(ctx context.Context, a, b string, limit int) ([]*Call, error)
}

// Directory resolves users without this module importing identity.
type Directory interface {
	LookupByEmail(ctx context.Context, email string) (id, name string, err error)
	LookupByID(ctx context.Context, id string) (email, name string, err error)
}
