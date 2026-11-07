// Package identity owns users, credentials, sessions, and roles.
package identity

import (
	"context"
	"errors"
	"time"
)

// Role is a coarse bundle of permissions. Coarse on purpose: fine-grained
// per-object ACLs are the standard way an access-control system becomes
// impossible to reason about, and nobody can tell you what a given user can
// actually do. Four roles, an explicit permission table, and every check
// resolved server-side.
type Role string

const (
	RoleGuest     Role = "guest"
	RoleMember    Role = "member"
	RoleModerator Role = "moderator"
	RoleAdmin     Role = "admin"
)

func (r Role) Valid() bool {
	switch r {
	case RoleGuest, RoleMember, RoleModerator, RoleAdmin:
		return true
	}
	return false
}

// rank orders roles for the "cannot escalate above yourself" rule.
func (r Role) rank() int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleModerator:
		return 2
	case RoleMember:
		return 1
	default:
		return 0
	}
}

// Permission is a single capability.
type Permission string

const (
	PermJoinMeeting   Permission = "meeting.join"
	PermHostMeeting   Permission = "meeting.host"
	PermRecordMeeting Permission = "meeting.record"
	PermMuteOthers    Permission = "meeting.mute_others"
	PermRemoveOthers  Permission = "meeting.remove_others"

	PermUserRead   Permission = "user.read"
	PermUserWrite  Permission = "user.write"
	PermUserDelete Permission = "user.delete"

	PermAuditRead   Permission = "audit.read"
	PermAuditVerify Permission = "audit.verify"
	PermAuditExport Permission = "audit.export"

	PermTelemetryRead Permission = "telemetry.read"
)

// grants is the whole authorisation model, in one readable table.
//
// Being able to read the entire policy on one screen is worth more than the
// flexibility of a rules engine. When someone asks "can a moderator delete a
// user", the answer is here, and it is the same answer the code gives.
var grants = map[Role][]Permission{
	RoleGuest: {
		PermJoinMeeting,
	},
	RoleMember: {
		PermJoinMeeting, PermHostMeeting,
	},
	RoleModerator: {
		PermJoinMeeting, PermHostMeeting, PermRecordMeeting,
		PermMuteOthers, PermRemoveOthers,
		PermUserRead, PermAuditRead, PermTelemetryRead,
	},
	RoleAdmin: {
		PermJoinMeeting, PermHostMeeting, PermRecordMeeting,
		PermMuteOthers, PermRemoveOthers,
		PermUserRead, PermUserWrite, PermUserDelete,
		PermAuditRead, PermAuditVerify, PermAuditExport,
		PermTelemetryRead,
	},
}

// Can reports whether a role holds a permission.
func (r Role) Can(p Permission) bool {
	for _, g := range grants[r] {
		if g == p {
			return true
		}
	}
	return false
}

// Permissions lists a role's capabilities, for the client to render an
// interface that matches what the server will actually allow. The client copy
// is a convenience and never an authority: every endpoint re-checks.
func (r Role) Permissions() []Permission {
	out := make([]Permission, len(grants[r]))
	copy(out, grants[r])
	return out
}

// Status is an account's lifecycle state.
type Status string

const (
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
)

// User is an account. The password hash never leaves this package.
type User struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	DisplayName  string     `json:"displayName"`
	Role         Role       `json:"role"`
	Status       Status     `json:"status"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	LastLoginAt  *time.Time `json:"lastLoginAt,omitempty"`
	FailedLogins int        `json:"failedLogins"`
	LockedUntil  *time.Time `json:"lockedUntil,omitempty"`

	passwordHash string
}

// PasswordHash is accessible only within the package.
func (u *User) PasswordHash() string { return u.passwordHash }

// Locked reports whether the account is currently barred from signing in.
func (u *User) Locked(now time.Time) bool {
	return u.LockedUntil != nil && now.Before(*u.LockedUntil)
}

// Session is a server-side session. The token itself is never stored, only
// its hash: a database dump must not hand the reader a set of live sessions.
type Session struct {
	ID        string     `json:"id"`
	UserID    string     `json:"userId"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt time.Time  `json:"expiresAt"`
	LastSeen  time.Time  `json:"lastSeen"`
	IP        string     `json:"ip"`
	UserAgent string     `json:"userAgent"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`

	tokenHash string
}

// Active reports whether a session may still be used.
func (s *Session) Active(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.ExpiresAt)
}

// Errors surfaced to callers. Deliberately few and deliberately vague where
// vagueness protects: see Service.Authenticate.
var (
	ErrNotFound          = errors.New("identity: not found")
	ErrInvalidCredential = errors.New("identity: invalid credentials")
	ErrAccountLocked     = errors.New("identity: account locked")
	ErrAccountSuspended  = errors.New("identity: account suspended")
	ErrEmailTaken        = errors.New("identity: email already registered")
	ErrWeakPassword      = errors.New("identity: password does not meet policy")
	ErrInvalidEmail      = errors.New("identity: invalid email")
	ErrInvalidRole       = errors.New("identity: invalid role")
	ErrForbidden         = errors.New("identity: forbidden")
	ErrSelfDemotion      = errors.New("identity: cannot change your own role")
	ErrLastAdmin         = errors.New("identity: cannot remove the last administrator")
	ErrSignupClosed      = errors.New("identity: self-registration is closed")
	// ErrSignupPending is returned when an address is already registered.
	// Named for what the caller is told rather than what happened, because
	// what happened is deliberately not disclosed.
	ErrSignupPending = errors.New("identity: check your email to continue")
)

// UserFilter narrows a user listing.
type UserFilter struct {
	Search string
	Role   Role
	Status Status
	Limit  int
	Offset int
}

// DirectoryEntry is the minimum needed to find and address a colleague.
//
// Deliberately narrower than User: no role, no status, no timestamps, no
// failure counters. A directory exists so people can find each other, not so
// any member can profile the account list.
type DirectoryEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Online bool   `json:"online"`
}

// Repository is the persistence port.
type Repository interface {
	CreateUser(ctx context.Context, u *User) error
	UpdateUser(ctx context.Context, u *User) error
	DeleteUser(ctx context.Context, id string) error
	UserByID(ctx context.Context, id string) (*User, error)
	UserByEmail(ctx context.Context, email string) (*User, error)

	// FoundingAdminEmail is the address of the oldest administrator account.
	// Used once, to guess the organisation's own domain on a deployment that
	// has never configured one.
	FoundingAdminEmail(ctx context.Context) (string, error)

	// DirectoryByDomains searches every active account whose email domain is
	// one of the deployment's own. This is the company address book: inside
	// one organisation, finding a colleague by typing their name is the point
	// of having a directory at all.
	DirectoryByDomains(ctx context.Context, domains []string, q string, limit int) ([]DirectoryEntry, error)

	// DirectoryByIDs searches within a known set of accounts. This is the
	// scoped directory: the caller has already decided which accounts this
	// person is entitled to see, and the query only narrows that set. An
	// empty id list returns nothing, never everything.
	DirectoryByIDs(ctx context.Context, ids []string, q string, limit int) ([]DirectoryEntry, error)
	ListUsers(ctx context.Context, f UserFilter) ([]*User, error)
	SearchDirectory(ctx context.Context, q string, limit int) ([]DirectoryEntry, error)
	CountUsers(ctx context.Context, f UserFilter) (int, error)
	CountAdmins(ctx context.Context) (int, error)

	CreateSession(ctx context.Context, s *Session) error
	SessionByTokenHash(ctx context.Context, hash string) (*Session, error)
	TouchSession(ctx context.Context, id string, seen time.Time) error
	RevokeSession(ctx context.Context, id string, at time.Time) error
	RevokeUserSessions(ctx context.Context, userID string, at time.Time) (int, error)
	ListSessions(ctx context.Context, userID string) ([]*Session, error)
	PurgeExpiredSessions(ctx context.Context, before time.Time) (int, error)

	// Password resets (reset.go).
	ResetCreatedAt(ctx context.Context, userID string) (time.Time, error)
	SaveReset(ctx context.Context, tokenHash, userID string, created, expires time.Time) error
	ConsumeReset(ctx context.Context, tokenHash string, now time.Time) (string, error)
}

// Reader is the narrow interface other modules depend on, per the module
// contract. Nothing outside identity may reach the repository, and nothing
// outside identity may see a password hash.
type Reader interface {
	UserByID(ctx context.Context, id string) (*User, error)
	Authorize(ctx context.Context, sessionToken string) (*User, *Session, error)
}
