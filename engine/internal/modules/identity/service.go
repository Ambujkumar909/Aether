package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/aether-rtc/aether/internal/modules/audit"
	"github.com/aether-rtc/aether/internal/platform/seal"
	"golang.org/x/crypto/argon2"
)

// Password hashing parameters.
//
// Argon2id rather than bcrypt or PBKDF2: it is the only widely available
// choice that resists GPU and ASIC attack through memory hardness rather than
// iteration count alone, and it is what every current guidance recommends.
//
// The figures follow OWASP's minimum for argon2id and are tuned so a single
// verification costs roughly 50 ms on a server core. That is imperceptible to
// a user signing in once and brutally expensive to an attacker working
// through a leaked table.
const (
	argonTime    = 2
	argonMemory  = 19 * 1024 // 19 MiB
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16
)

// Lockout policy.
const (
	// MaxFailedLogins before an account is temporarily barred. High enough
	// that a person fumbling their password is not locked out, low enough
	// that online guessing is hopeless.
	MaxFailedLogins = 8
	// LockoutDuration is deliberately finite. Permanent lockout on failed
	// attempts converts a guessing attack into a denial-of-service against
	// any account whose email address is known.
	LockoutDuration = 15 * time.Minute
)

// Session policy.
const (
	SessionLifetime = 12 * time.Hour
	SessionIdle     = 2 * time.Hour
	TokenBytes      = 32
)

// MinPasswordLength follows current guidance: length is what matters, and
// composition rules mostly produce predictable substitutions.
const MinPasswordLength = 12

// Service is the identity business logic.
type Service struct {
	repo  Repository
	audit audit.Recorder
	log   *slog.Logger

	// presence reports who is connected, so a directory entry can say so.
	presence PresenceSource

	// DirectoryScope reports how far a directory search may reach:
	// "connections" or "everyone". Injected from the settings module rather
	// than imported, and nil means the narrow answer.
	DirectoryScope func(ctx context.Context) string

	// Contacts lists the accounts somebody already shares a conversation
	// with. Injected from messaging for the same reason. Nil means nobody
	// has any, which fails closed: a misconfigured deployment shows an empty
	// directory rather than the whole one.
	Contacts func(ctx context.Context, userID string) ([]string, error)

	// OrgDomains lists the email domains that are this deployment's own
	// organisation. Injected from settings. Empty -- the default -- means
	// there is no organisation and only contacts and exact addresses resolve.
	OrgDomains func(ctx context.Context) []string

	// SendReset delivers a password reset link for token to email. Wired in
	// main when outgoing mail is configured; nil turns password reset off.
	SendReset func(email, token string)

	// SendVerify delivers a sign-up link; set, it makes sign-up email-verified
	// (signup.go). SendSignupNotice tells an existing account's owner that
	// somebody tried to sign up with their address.
	SendVerify       func(email, token string)
	SendSignupNotice func(email string)
	notices          noticeThrottle
	// Guard holds the operator's sign-up policy (signupguard.go); nil is
	// no block list and no mail check.
	Guard *SignupGuard

	// Sealer protects stored secrets (two-step sign-in) with the server's
	// key; nil turns two-step sign-in off. tickets hold sign-ins between the
	// password and the code.
	Sealer  *seal.Sealer
	tickets ticketStore
	// now is the clock the second step reads; nil is time.Now. Tests set
	// it to step through 30-second windows without waiting.
	now func() time.Time

	// dummyHash makes an unknown-email login cost the same as a known one.
	// Without it, response time discloses which addresses are registered,
	// which is how attackers build a target list before ever guessing a
	// password.
	dummyHash string
}

func NewService(repo Repository, rec audit.Recorder, log *slog.Logger) *Service {
	s := &Service{repo: repo, audit: rec, log: log}
	h, err := HashPassword("a-fixed-value-never-used-for-login")
	if err != nil {
		// Only possible if the platform RNG is broken, in which case nothing
		// else in this package is safe either.
		panic(fmt.Sprintf("identity: cannot hash: %v", err))
	}
	s.dummyHash = h
	return s
}

// ---------------------------------------------------------------------------
// Password hashing
// ---------------------------------------------------------------------------

// hashSlots bounds how many argon2id computations run at once.
//
// Each one holds argonMemory (19 MiB) for its duration and is pure CPU, so
// running more at once than there are cores buys no throughput and only
// memory. Unbounded, the 50-person stress run's simultaneous sign-ins took
// the server to 992 MB (ARCHITECTURE 6br) -- and a failed login hashes too
// (against the dummy hash, to keep timing equal), so anybody could drive it
// there with logins from enough addresses.
//
// Bounded by the cores, but never more than maxHashSlots: on a many-core
// machine one slot per core still let 200 sign-ins hold 633 MB. Eight slots
// are about 150 MB and about 160 sign-ins a second (over half a million an
// hour), and leave the other cores to the meetings.
const maxHashSlots = 8

var hashSlots = make(chan struct{}, min(runtime.GOMAXPROCS(0), maxHashSlots))

// hashInFlight and hashPeak let the test see the bound hold.
var hashInFlight, hashPeak atomic.Int32

func idKey(plain, salt []byte, t, mem uint32, p uint8, n uint32) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	in := hashInFlight.Add(1)
	defer hashInFlight.Add(-1)
	for {
		peak := hashPeak.Load()
		if in <= peak || hashPeak.CompareAndSwap(peak, in) {
			break
		}
	}
	return argon2.IDKey(plain, salt, t, mem, p, n)
}

// HashPassword produces a PHC-format argon2id string. The format carries its
// own parameters, so raising the cost later does not invalidate existing
// hashes: they keep verifying with the parameters they were made with, and
// can be upgraded on next successful login.
func HashPassword(plain string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("identity: read salt: %w", err)
	}
	key := idKey([]byte(plain), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks a plaintext against a PHC hash in constant time with
// respect to the digest contents.
func VerifyPassword(plain, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false
	}
	var mem uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := idKey([]byte(plain), salt, t, mem, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ValidatePassword applies the policy.
func ValidatePassword(p string) error {
	if len([]rune(p)) < MinPasswordLength {
		return fmt.Errorf("%w: must be at least %d characters",
			ErrWeakPassword, MinPasswordLength)
	}
	// Reject a password made of one repeated character, and one that is only
	// whitespace. Beyond that, length carries the weight: composition rules
	// mostly teach people to write Password1!.
	var distinct = map[rune]struct{}{}
	allSpace := true
	for _, r := range p {
		distinct[r] = struct{}{}
		if !unicode.IsSpace(r) {
			allSpace = false
		}
	}
	if allSpace {
		return fmt.Errorf("%w: cannot be only whitespace", ErrWeakPassword)
	}
	if len(distinct) < 5 {
		return fmt.Errorf("%w: too repetitive", ErrWeakPassword)
	}
	return nil
}

// NormalizeEmail lowercases and trims. Stored addresses are compared
// case-insensitively, which is what users expect and what the unique index
// enforces.
func NormalizeEmail(e string) (string, error) {
	e = strings.TrimSpace(strings.ToLower(e))
	if e == "" {
		return "", ErrInvalidEmail
	}
	addr, err := mail.ParseAddress(e)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrInvalidEmail, e)
	}
	return strings.ToLower(addr.Address), nil
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

// Authenticate verifies credentials and issues a session.
//
// Every failure path returns the same error and takes broadly the same time.
// Telling an attacker whether the email exists, or whether the password was
// merely wrong versus the account suspended, hands them a free oracle. The
// audit log records the true reason; the response does not.
// checkPassword is the first step of a sign-in: every refusal, the lockout,
// and the audit of each. It starts no session.
func (s *Service) checkPassword(ctx context.Context, email, password, ip, ua string) (*User, error) {
	now := time.Now().UTC()
	norm, err := NormalizeEmail(email)
	if err != nil {
		// Still burn the time, so a malformed address is not distinguishable
		// by latency from a well-formed unknown one.
		VerifyPassword(password, s.dummyHash)
		s.record(ctx, audit.Event{
			Action: audit.ActionLoginFailed, Outcome: audit.OutcomeFailure,
			Subject: email, IP: ip, UserAgent: ua, Detail: "malformed email",
		})
		return nil, ErrInvalidCredential
	}

	u, err := s.repo.UserByEmail(ctx, norm)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		VerifyPassword(password, s.dummyHash)
		s.record(ctx, audit.Event{
			Action: audit.ActionLoginFailed, Outcome: audit.OutcomeFailure,
			Subject: norm, IP: ip, UserAgent: ua, Detail: "no such account",
		})
		return nil, ErrInvalidCredential
	}

	if u.Locked(now) {
		VerifyPassword(password, s.dummyHash)
		s.record(ctx, audit.Event{
			Action: audit.ActionLoginFailed, Outcome: audit.OutcomeDenied,
			ActorID: u.ID, ActorEmail: u.Email, Subject: norm,
			IP: ip, UserAgent: ua, Detail: "account locked",
		})
		return nil, ErrInvalidCredential
	}

	if !VerifyPassword(password, u.passwordHash) {
		s.loginFailed(ctx, u, ip, ua, "wrong password")
		return nil, ErrInvalidCredential
	}

	if u.Status == StatusSuspended {
		s.record(ctx, audit.Event{
			Action: audit.ActionLoginFailed, Outcome: audit.OutcomeDenied,
			ActorID: u.ID, ActorEmail: u.Email, Subject: norm,
			IP: ip, UserAgent: ua, Detail: "account suspended",
		})
		return nil, ErrInvalidCredential
	}

	return u, nil
}

// loginFailed counts a wrong password (or a spent second-step ticket) toward
// the lockout and records it.
func (s *Service) loginFailed(ctx context.Context, u *User, ip, ua, detail string) {
	now := time.Now().UTC()
	u.FailedLogins++
	if u.FailedLogins >= MaxFailedLogins {
		until := now.Add(LockoutDuration)
		u.LockedUntil = &until
		u.FailedLogins = 0
		detail = fmt.Sprintf("%s, locked until %s", detail, until.Format(time.RFC3339))
		s.record(ctx, audit.Event{
			Action: audit.ActionAccountLocked, Outcome: audit.OutcomeDenied,
			ActorID: u.ID, ActorEmail: u.Email, Subject: u.Email,
			IP: ip, UserAgent: ua,
			Detail: fmt.Sprintf("after %d failed attempts", MaxFailedLogins),
		})
	}
	u.UpdatedAt = now
	if err := s.repo.UpdateUser(ctx, u); err != nil {
		s.log.Error("failed to record login failure", "err", err)
	}
	s.record(ctx, audit.Event{
		Action: audit.ActionLoginFailed, Outcome: audit.OutcomeFailure,
		ActorID: u.ID, ActorEmail: u.Email, Subject: u.Email,
		IP: ip, UserAgent: ua, Detail: detail,
	})
}

// finishLogin is a sign-in past every check: counters cleared, a session
// minted, the success recorded (detail names the second step, if any).
func (s *Service) finishLogin(ctx context.Context, u *User, ip, ua, detail string) (string, error) {
	now := time.Now().UTC()
	u.FailedLogins = 0
	u.LockedUntil = nil
	u.LastLoginAt = &now
	u.UpdatedAt = now
	if err := s.repo.UpdateUser(ctx, u); err != nil {
		return "", err
	}
	token, _, err := s.newSession(ctx, u.ID, ip, ua, now)
	if err != nil {
		return "", err
	}
	s.record(ctx, audit.Event{
		Action: audit.ActionLoginSucceeded, Outcome: audit.OutcomeSuccess,
		ActorID: u.ID, ActorEmail: u.Email, Subject: u.Email,
		IP: ip, UserAgent: ua, Detail: detail,
	})
	return token, nil
}

// Authenticate signs in an account that has no second step. One that does
// is refused here (ErrSecondStepRequired): Login and SecondStep are its flow,
// and no caller can skip the code by calling this instead.
func (s *Service) Authenticate(ctx context.Context, email, password, ip, ua string) (*User, string, error) {
	u, err := s.checkPassword(ctx, email, password, ip, ua)
	if err != nil {
		return nil, "", err
	}
	if s.twoStepOn(ctx, u.ID) {
		return nil, "", ErrSecondStepRequired
	}
	token, err := s.finishLogin(ctx, u, ip, ua, "")
	if err != nil {
		return nil, "", err
	}
	return u, token, nil
}

// newSession mints a token and stores only its hash.
func (s *Service) newSession(ctx context.Context, userID, ip, ua string, now time.Time) (string, *Session, error) {
	raw := make([]byte, TokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("identity: read token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	sess := &Session{
		ID:        newID(),
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(SessionLifetime),
		LastSeen:  now,
		IP:        ip,
		UserAgent: truncate(ua, 256),
		tokenHash: HashToken(token),
	}
	if err := s.repo.CreateSession(ctx, sess); err != nil {
		return "", nil, err
	}
	return token, sess, nil
}

// HashToken is a plain SHA-256. A session token is a 256-bit random value,
// not a password: it has no low-entropy structure to attack, so there is
// nothing for a slow KDF to defend and a fast hash keeps the check off the
// critical path of every authenticated request.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Authorize resolves a session token to a live user.
func (s *Service) Authorize(ctx context.Context, token string) (*User, *Session, error) {
	if token == "" {
		return nil, nil, ErrInvalidCredential
	}
	now := time.Now().UTC()

	sess, err := s.repo.SessionByTokenHash(ctx, HashToken(token))
	if err != nil {
		return nil, nil, ErrInvalidCredential
	}
	if !sess.Active(now) {
		return nil, nil, ErrInvalidCredential
	}
	// Idle timeout is enforced separately from absolute lifetime. A session
	// left open on an unattended machine should die well before twelve hours.
	if now.Sub(sess.LastSeen) > SessionIdle {
		_ = s.repo.RevokeSession(ctx, sess.ID, now)
		return nil, nil, ErrInvalidCredential
	}

	u, err := s.repo.UserByID(ctx, sess.UserID)
	if err != nil {
		return nil, nil, ErrInvalidCredential
	}
	// A suspension must take effect immediately, not at next login.
	if u.Status == StatusSuspended {
		_ = s.repo.RevokeSession(ctx, sess.ID, now)
		return nil, nil, ErrAccountSuspended
	}

	// Touch at most once a minute: writing on every request turns a read path
	// into a write path and, on SQLite, into a lock.
	if now.Sub(sess.LastSeen) > time.Minute {
		_ = s.repo.TouchSession(ctx, sess.ID, now)
		sess.LastSeen = now
	}
	return u, sess, nil
}

// Logout revokes one session.
func (s *Service) Logout(ctx context.Context, u *User, sess *Session, ip, ua string) error {
	now := time.Now().UTC()
	if err := s.repo.RevokeSession(ctx, sess.ID, now); err != nil {
		return err
	}
	s.record(ctx, audit.Event{
		Action: audit.ActionLogout, Outcome: audit.OutcomeSuccess,
		ActorID: u.ID, ActorEmail: u.Email, Subject: u.Email,
		IP: ip, UserAgent: ua,
	})
	return nil
}

// ---------------------------------------------------------------------------
// User management
// ---------------------------------------------------------------------------

// CreateUser registers an account. actor may be nil for bootstrap.
func (s *Service) CreateUser(ctx context.Context, actor *User, email, name, password string, role Role, ip, ua string) (*User, error) {
	if actor != nil {
		if !actor.Role.Can(PermUserWrite) {
			s.denied(ctx, actor, audit.ActionUserCreated, email, ip, ua)
			return nil, ErrForbidden
		}
		// No privilege escalation by proxy: you cannot mint an account more
		// powerful than your own.
		if role.rank() > actor.Role.rank() {
			s.denied(ctx, actor, audit.ActionUserCreated, email, ip, ua)
			return nil, ErrForbidden
		}
	}
	if !role.Valid() {
		return nil, ErrInvalidRole
	}
	norm, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	if _, err := s.repo.UserByEmail(ctx, norm); err == nil {
		return nil, ErrEmailTaken
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	name = strings.TrimSpace(name)
	if name == "" {
		name = norm
	}
	u := &User{
		ID: newID(), Email: norm, DisplayName: name, Role: role,
		Status: StatusActive, CreatedAt: now, UpdatedAt: now,
		passwordHash: hash,
	}
	if err := s.repo.CreateUser(ctx, u); err != nil {
		return nil, err
	}

	ev := audit.Event{
		Action: audit.ActionUserCreated, Outcome: audit.OutcomeSuccess,
		Subject: norm, IP: ip, UserAgent: ua,
		Detail: "role " + string(role),
	}
	if actor != nil {
		ev.ActorID, ev.ActorEmail = actor.ID, actor.Email
	} else {
		ev.Detail += " (bootstrap)"
	}
	s.record(ctx, ev)
	return u, nil
}

// SignUp registers an account with no actor behind it.
//
// Kept separate from CreateUser rather than folded into it with a nil actor,
// because the two have genuinely different rules and conflating them is how a
// self-service endpoint quietly acquires the ability to mint administrators.
// Whatever a visitor asks for, this grants exactly one role.
func (s *Service) SignUp(ctx context.Context, email, name, password, ip, ua string) (*User, error) {
	norm, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = norm
	}
	if len(name) > 120 {
		name = name[:120]
	}
	if s.Guard.Blocked(norm) {
		// The same answer as a new address, and the same work, so the list
		// cannot be read back one domain at a time.
		HashPassword(password)
		signupCounts.Blocked.Add(1)
		s.record(ctx, audit.Event{Action: audit.ActionSignUpBlocked, Outcome: audit.OutcomeDenied,
			Subject: norm, IP: ip, UserAgent: ua, Detail: "domain on the block list"})
		return nil, ErrSignupPending
	}
	if s.SendVerify != nil {
		return nil, s.verifiedSignup(ctx, norm, name, password, ip, ua)
	}
	// Without mail nothing can prove an address, and one in the
	// organisation's own domains would join its address book on sight.
	if s.inOrgDomain(ctx, norm) {
		s.record(ctx, audit.Event{Action: audit.ActionSignUpBlocked, Outcome: audit.OutcomeDenied,
			Subject: norm, IP: ip, UserAgent: ua, Detail: "organisation domain, and no mail to verify it"})
		return nil, ErrSignupOrgDomain
	}

	// An address that is already registered must not be distinguishable from
	// one that is not. Reporting "that email is taken" to an anonymous caller
	// turns the signup form into an account enumeration oracle, which is the
	// same hole the login path closes.
	//
	// So the work is done either way and the caller is told the same thing.
	// The real outcome goes to the audit log, where an operator can see it.
	if _, err := s.repo.UserByEmail(ctx, norm); err == nil {
		HashPassword(password)
		s.record(ctx, audit.Event{
			Action: audit.ActionSignUpBlocked, Outcome: audit.OutcomeDenied,
			Subject: norm, IP: ip, UserAgent: ua,
			Detail: "address already registered",
		})
		return nil, ErrSignupPending
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	u := &User{
		ID: newID(), Email: norm, DisplayName: name,
		Role: RoleMember, Status: StatusActive,
		CreatedAt: now, UpdatedAt: now, passwordHash: hash,
	}
	if err := s.repo.CreateUser(ctx, u); err != nil {
		return nil, err
	}
	signupCounts.Created.Add(1)
	s.record(ctx, audit.Event{
		Action: audit.ActionSignUp, Outcome: audit.OutcomeSuccess,
		ActorID: u.ID, ActorEmail: u.Email, Subject: norm,
		IP: ip, UserAgent: ua, Detail: "self-registered as member",
	})
	return u, nil
}

// StartSession issues a session for a user who has just proved themselves by
// some means other than the password form.
func (s *Service) StartSession(ctx context.Context, u *User, ip, ua string) (string, error) {
	token, _, err := s.newSession(ctx, u.ID, ip, ua, time.Now().UTC())
	return token, err
}

// PresenceSource reports who is currently connected. Supplied by the
// messaging module at wiring time; nil simply means nobody shows as online.
type PresenceSource interface {
	Online(userID string) bool
}

// SetPresence attaches a presence source.
func (s *Service) SetPresence(p PresenceSource) { s.presence = p }

// SearchDirectory finds colleagues by name or address.
//
// Available to every signed-in account, which is a deliberate trade: a
// directory that only administrators can search is a directory nobody can
// use, and the alternative -- making people type an exact address they have
// to obtain some other way -- pushes them straight back to email. The entry
// carries a name and an address and nothing else.
// SearchDirectory answers who this person is allowed to find.
//
// The rule, and why it is this one. A directory that lists accounts to
// anybody signed in is an address book of the whole deployment handed to
// whoever registered most recently -- and with a substring match on email, a
// single query for "@" returns everyone. On a server strangers can reach,
// that is an email harvesting tool with a search box on it, and the fact that
// it is convenient is exactly why it survives review.
//
// So by default a search reaches two places and no further:
//
//   - people you already share a conversation with, which you can browse and
//     filter freely, because you can already see all of them; and
//   - one exact email address, matched in full. If somebody tells you their
//     address you can reach them, which is how a first conversation starts
//     everywhere. It discloses nothing, because you had to know the address
//     to type it: a wrong guess and an unregistered address are the same
//     empty result.
//
// What is deliberately impossible is discovery: there is no query that
// returns an account you had no way to name, and no way to page through the
// deployment. An administrator can trade that away with the directory scope
// setting, which is the right shape for one organisation sharing an address
// book and the wrong one for anything open to the public.
func (s *Service) SearchDirectory(ctx context.Context, actor *User, q string, limit int) ([]DirectoryEntry, error) {
	q = strings.TrimSpace(q)
	if len(q) > 120 {
		q = q[:120]
	}

	var entries []DirectoryEntry
	var err error

	if s.DirectoryScope != nil && s.DirectoryScope(ctx) == "everyone" {
		// The administrator has published the address book on purpose.
		entries, err = s.repo.SearchDirectory(ctx, q, limit)
		if err != nil {
			return nil, err
		}
	} else {
		entries, err = s.connectionsDirectory(ctx, actor, q, limit)
		if err != nil {
			return nil, err
		}
	}

	out := entries[:0]
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		// You are not a search result to yourself.
		if actor != nil && e.ID == actor.ID {
			continue
		}
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		if s.presence != nil {
			e.Online = s.presence.Online(e.ID)
		}
		out = append(out, e)
	}
	return out, nil
}

// SuggestOrgDomain guesses this deployment's own email domain, once, for a
// server that has never been told.
//
// The guess is the founding administrator's domain. Somebody installing this
// for a company signs in as themselves, and their address says which company
// it is; asking them to type it again into a console they may never open makes
// the address book quietly not work, which looks like a missing feature rather
// than a missing setting.
//
// It returns nothing for a public provider. An administrator who happens to
// use a personal address has not thereby made that provider an organisation,
// and treating it as one would publish every account on it.
func (s *Service) SuggestOrgDomain(ctx context.Context) string {
	email, err := s.repo.FoundingAdminEmail(ctx)
	if err != nil || email == "" {
		return ""
	}
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return ""
	}
	d := strings.ToLower(strings.TrimSpace(email[at+1:]))
	if d == "" || !strings.Contains(d, ".") {
		return ""
	}
	return d
}

// MayMessage reports whether one account may open a conversation with
// another, by address.
//
// Exported because the rule has to be enforced where conversations are
// created, not only where the directory draws its results. A policy checked
// only by the interface is not a policy: the search box would return nothing
// while the API behind it still obliged, and the setting would be a label.
//
// Both directions are checked. If outside contact is closed, it is closed for
// the person outside too, or the restriction is one-way and therefore not one.
func (s *Service) MayMessage(ctx context.Context, fromEmail, toEmail string) bool {
	if s.DirectoryScope == nil || s.DirectoryScope(ctx) != "organisation" {
		return true
	}
	if s.OrgDomains == nil {
		// The strictest scope with no organisation declared: there is no
		// "inside", so nobody is reachable this way. Existing conversations
		// are untouched -- this gates opening a new one.
		return false
	}
	domains := s.OrgDomains(ctx)
	return domainOf(fromEmail) != "" &&
		inDomains(domainOf(fromEmail), domains) &&
		inDomains(domainOf(toEmail), domains)
}

func domainOf(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(email[at+1:]))
}

func inDomains(d string, domains []string) bool {
	if d == "" {
		return false
	}
	for _, x := range domains {
		if strings.EqualFold(x, d) {
			return true
		}
	}
	return false
}

// inOrg reports whether this account belongs to the deployment's own
// organisation, which is what entitles it to the address book.
//
// Somebody signed in with a personal address on a company deployment is a
// guest: they can reach the colleagues they already talk to and anybody whose
// address they know, and the staff list is not theirs to browse.
func (s *Service) inOrg(ctx context.Context, actor *User) []string {
	if actor == nil || s.OrgDomains == nil {
		return nil
	}
	at := strings.LastIndexByte(actor.Email, '@')
	if at < 0 {
		return nil
	}
	mine := strings.ToLower(actor.Email[at+1:])
	domains := s.OrgDomains(ctx)
	for _, d := range domains {
		if strings.EqualFold(d, mine) {
			return domains
		}
	}
	return nil
}

// connectionsDirectory is the default reach: the organisation you belong to,
// your own conversations, and one address typed in full.
func (s *Service) connectionsDirectory(ctx context.Context, actor *User, q string, limit int) ([]DirectoryEntry, error) {
	var out []DirectoryEntry

	// The company address book. Inside one organisation, finding a colleague
	// by typing their name is the whole point of having a directory, and
	// making people remember addresses for it is the behaviour this was
	// changed to avoid.
	if domains := s.inOrg(ctx, actor); len(domains) > 0 {
		found, err := s.repo.DirectoryByDomains(ctx, domains, q, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}

	if actor != nil && s.Contacts != nil {
		ids, err := s.Contacts(ctx, actor.ID)
		if err != nil {
			// Fail closed. An unreadable contact list is not a reason to
			// fall back to listing the deployment.
			s.log.Warn("directory: could not read contacts", "err", err)
		} else if len(ids) > 0 {
			found, err := s.repo.DirectoryByIDs(ctx, ids, q, limit)
			if err != nil {
				return nil, err
			}
			out = append(out, found...)
		}
	}

	// The exact address, for anybody the organisation does not cover.
	//
	// Closed entirely under the "organisation" scope. That is the setting a
	// locked-down company tenant wants: colleagues and nobody else, so a
	// personal address typed into the search box finds nothing at all.
	external := s.DirectoryScope == nil || s.DirectoryScope(ctx) != "organisation"

	// Only a complete, plausible address counts -- a bare domain or a
	// fragment matches nobody, so this cannot be walked.
	if external && isCompleteEmail(q) {
		u, err := s.repo.UserByEmail(ctx, strings.ToLower(q))
		if err == nil && u != nil && u.Status == StatusActive {
			name := u.DisplayName
			if name == "" {
				name = u.Email
			}
			out = append(out, DirectoryEntry{ID: u.ID, Name: name, Email: u.Email})
		}
		// A miss is silent. An unregistered address and a registered one that
		// the caller may not see must be indistinguishable, or the directory
		// becomes the account-enumeration oracle that login was hardened
		// against in the first place.
	}

	return out, nil
}

// isCompleteEmail reports whether a query is a whole address rather than a
// fragment of one. Deliberately strict: this is the one input that reaches
// past your own contacts, so "@", "@gmail.com" and "ambuj@" must all fail.
func isCompleteEmail(q string) bool {
	at := strings.IndexByte(q, '@')
	if at <= 0 || at != strings.LastIndexByte(q, '@') {
		return false
	}
	local, domain := q[:at], q[at+1:]
	if local == "" || len(domain) < 3 {
		return false
	}
	dot := strings.IndexByte(domain, '.')
	if dot <= 0 || dot == len(domain)-1 {
		return false
	}
	if strings.ContainsAny(q, " \t%_*") {
		return false
	}
	return true
}

// ListUsers returns accounts for an administrator view.
func (s *Service) ListUsers(ctx context.Context, actor *User, f UserFilter) ([]*User, int, error) {
	if !actor.Role.Can(PermUserRead) {
		return nil, 0, ErrForbidden
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	users, err := s.repo.ListUsers(ctx, f)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.repo.CountUsers(ctx, f)
	if err != nil {
		return nil, 0, err
	}
	return users, total, nil
}

// SetRole changes a user's role.
func (s *Service) SetRole(ctx context.Context, actor *User, userID string, role Role, ip, ua string) (*User, error) {
	if !actor.Role.Can(PermUserWrite) {
		s.denied(ctx, actor, audit.ActionUserRoleChanged, userID, ip, ua)
		return nil, ErrForbidden
	}
	if !role.Valid() {
		return nil, ErrInvalidRole
	}
	// Changing your own role is refused outright. It is the shortest path to
	// both accidental self-lockout and deliberate self-escalation, and there
	// is no legitimate case for it that another administrator cannot serve.
	if actor.ID == userID {
		return nil, ErrSelfDemotion
	}
	if role.rank() > actor.Role.rank() {
		s.denied(ctx, actor, audit.ActionUserRoleChanged, userID, ip, ua)
		return nil, ErrForbidden
	}

	u, err := s.repo.UserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u.Role.rank() > actor.Role.rank() {
		// You cannot act on someone above you.
		s.denied(ctx, actor, audit.ActionUserRoleChanged, u.Email, ip, ua)
		return nil, ErrForbidden
	}
	if u.Role == RoleAdmin && role != RoleAdmin {
		if err := s.guardLastAdmin(ctx); err != nil {
			return nil, err
		}
	}

	old := u.Role
	u.Role = role
	u.UpdatedAt = time.Now().UTC()
	if err := s.repo.UpdateUser(ctx, u); err != nil {
		return nil, err
	}

	// A role change must not leave elevated sessions running with the old
	// rights, and must not silently keep a demoted user's admin tab working.
	if _, err := s.repo.RevokeUserSessions(ctx, u.ID, time.Now().UTC()); err != nil {
		s.log.Error("failed to revoke sessions after role change", "err", err)
	}

	s.record(ctx, audit.Event{
		Action: audit.ActionUserRoleChanged, Outcome: audit.OutcomeSuccess,
		ActorID: actor.ID, ActorEmail: actor.Email, Subject: u.Email,
		IP: ip, UserAgent: ua,
		Detail: fmt.Sprintf("from %s to %s", old, role),
	})
	return u, nil
}

// SetStatus suspends or reactivates an account.
func (s *Service) SetStatus(ctx context.Context, actor *User, userID string, st Status, ip, ua string) (*User, error) {
	if !actor.Role.Can(PermUserWrite) {
		s.denied(ctx, actor, audit.ActionUserSuspended, userID, ip, ua)
		return nil, ErrForbidden
	}
	if actor.ID == userID {
		return nil, ErrForbidden
	}
	u, err := s.repo.UserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u.Role.rank() > actor.Role.rank() {
		s.denied(ctx, actor, audit.ActionUserSuspended, u.Email, ip, ua)
		return nil, ErrForbidden
	}
	if st == StatusSuspended && u.Role == RoleAdmin {
		if err := s.guardLastAdmin(ctx); err != nil {
			return nil, err
		}
	}

	u.Status = st
	u.UpdatedAt = time.Now().UTC()
	if err := s.repo.UpdateUser(ctx, u); err != nil {
		return nil, err
	}

	action := audit.ActionUserReactivated
	if st == StatusSuspended {
		action = audit.ActionUserSuspended
		if _, err := s.repo.RevokeUserSessions(ctx, u.ID, time.Now().UTC()); err != nil {
			s.log.Error("failed to revoke sessions on suspend", "err", err)
		}
	}
	s.record(ctx, audit.Event{
		Action: action, Outcome: audit.OutcomeSuccess,
		ActorID: actor.ID, ActorEmail: actor.Email, Subject: u.Email,
		IP: ip, UserAgent: ua,
	})
	return u, nil
}

// DeleteUser removes an account.
func (s *Service) DeleteUser(ctx context.Context, actor *User, userID, ip, ua string) error {
	if !actor.Role.Can(PermUserDelete) {
		s.denied(ctx, actor, audit.ActionUserDeleted, userID, ip, ua)
		return ErrForbidden
	}
	if actor.ID == userID {
		return ErrForbidden
	}
	u, err := s.repo.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if u.Role.rank() > actor.Role.rank() {
		s.denied(ctx, actor, audit.ActionUserDeleted, u.Email, ip, ua)
		return ErrForbidden
	}
	if u.Role == RoleAdmin {
		if err := s.guardLastAdmin(ctx); err != nil {
			return err
		}
	}
	if err := s.repo.DeleteUser(ctx, userID); err != nil {
		return err
	}
	s.record(ctx, audit.Event{
		Action: audit.ActionUserDeleted, Outcome: audit.OutcomeSuccess,
		ActorID: actor.ID, ActorEmail: actor.Email, Subject: u.Email,
		IP: ip, UserAgent: ua,
	})
	return nil
}

// ChangePassword updates a credential. A user may change their own; an
// administrator may reset another's.
func (s *Service) ChangePassword(ctx context.Context, actor *User, userID, current, next, ip, ua string) error {
	self := actor.ID == userID
	if !self && !actor.Role.Can(PermUserWrite) {
		s.denied(ctx, actor, audit.ActionPasswordChanged, userID, ip, ua)
		return ErrForbidden
	}
	u, err := s.repo.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if !self && u.Role.rank() > actor.Role.rank() {
		s.denied(ctx, actor, audit.ActionPasswordChanged, u.Email, ip, ua)
		return ErrForbidden
	}
	// Changing your own password requires proving you know the current one,
	// so that a borrowed unlocked laptop cannot be used to take the account.
	if self && !VerifyPassword(current, u.passwordHash) {
		s.record(ctx, audit.Event{
			Action: audit.ActionPasswordChanged, Outcome: audit.OutcomeFailure,
			ActorID: actor.ID, ActorEmail: actor.Email, Subject: u.Email,
			IP: ip, UserAgent: ua, Detail: "current password incorrect",
		})
		return ErrInvalidCredential
	}
	if err := ValidatePassword(next); err != nil {
		return err
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	u.passwordHash = hash
	u.UpdatedAt = time.Now().UTC()
	u.FailedLogins = 0
	u.LockedUntil = nil
	if err := s.repo.UpdateUser(ctx, u); err != nil {
		return err
	}
	// Every other session for this user dies. A password change is how
	// someone responds to a suspected compromise, and it has to actually
	// evict the intruder.
	if _, err := s.repo.RevokeUserSessions(ctx, u.ID, time.Now().UTC()); err != nil {
		s.log.Error("failed to revoke sessions after password change", "err", err)
	}
	s.record(ctx, audit.Event{
		Action: audit.ActionPasswordChanged, Outcome: audit.OutcomeSuccess,
		ActorID: actor.ID, ActorEmail: actor.Email, Subject: u.Email,
		IP: ip, UserAgent: ua,
	})
	return nil
}

// Sessions lists a user's sessions.
func (s *Service) Sessions(ctx context.Context, actor *User, userID string) ([]*Session, error) {
	if actor.ID != userID && !actor.Role.Can(PermUserRead) {
		return nil, ErrForbidden
	}
	return s.repo.ListSessions(ctx, userID)
}

// RevokeSession kills one session.
func (s *Service) RevokeSession(ctx context.Context, actor *User, userID, sessionID, ip, ua string) error {
	if actor.ID != userID && !actor.Role.Can(PermUserWrite) {
		return ErrForbidden
	}
	if err := s.repo.RevokeSession(ctx, sessionID, time.Now().UTC()); err != nil {
		return err
	}
	s.record(ctx, audit.Event{
		Action: audit.ActionSessionRevoked, Outcome: audit.OutcomeSuccess,
		ActorID: actor.ID, ActorEmail: actor.Email, Subject: userID,
		IP: ip, UserAgent: ua, Detail: "session " + sessionID,
	})
	return nil
}

// UserByID exposes a lookup for other modules through Reader.
func (s *Service) UserByID(ctx context.Context, id string) (*User, error) {
	return s.repo.UserByID(ctx, id)
}

// CountUsers is used by bootstrap to detect an empty installation.
func (s *Service) CountUsers(ctx context.Context) (int, error) {
	return s.repo.CountUsers(ctx, UserFilter{})
}

// PurgeExpiredSessions is housekeeping.
func (s *Service) PurgeExpiredSessions(ctx context.Context) (int, error) {
	return s.repo.PurgeExpiredSessions(ctx, time.Now().UTC())
}

// guardLastAdmin refuses an operation that would leave nobody able to
// administer the system. Locking everyone out of their own installation is a
// support incident that cannot be resolved from inside the product.
func (s *Service) guardLastAdmin(ctx context.Context) error {
	n, err := s.repo.CountAdmins(ctx)
	if err != nil {
		return err
	}
	if n <= 1 {
		return ErrLastAdmin
	}
	return nil
}

func (s *Service) record(ctx context.Context, e audit.Event) {
	if s.audit != nil {
		s.audit.Record(ctx, e)
	}
}

func (s *Service) denied(ctx context.Context, actor *User, a audit.Action, subject, ip, ua string) {
	s.record(ctx, audit.Event{
		Action: a, Outcome: audit.OutcomeDenied,
		ActorID: actor.ID, ActorEmail: actor.Email, Subject: subject,
		IP: ip, UserAgent: ua,
		Detail: "insufficient permission for role " + string(actor.Role),
	})
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
