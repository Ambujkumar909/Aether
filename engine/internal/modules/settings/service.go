package settings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aether-rtc/aether/internal/modules/audit"
)

var (
	ErrUnknownKey = errors.New("settings: no such setting")
	ErrBadValue   = errors.New("settings: value is not allowed for this setting")
	ErrForbidden  = errors.New("settings: only an administrator may change deployment policy")
)

// Actor is whoever is asking.
type Actor struct {
	ID    string
	Email string
	Admin bool
}

type Service struct {
	repo Repository
	rec  audit.Recorder
	log  *slog.Logger

	// Cached, because these are read on the join path of every meeting and
	// changed a handful of times in a deployment's life. Refreshed on write
	// rather than expired on a timer: a policy change an administrator has
	// just made must apply to the next person through the door, not to the
	// next person after a cache interval.
	mu     sync.RWMutex
	cache  map[Key]Setting
	loaded bool
}

func NewService(repo Repository, rec audit.Recorder, log *slog.Logger) *Service {
	return &Service{repo: repo, rec: rec, log: log, cache: map[Key]Setting{}}
}

// Load warms the cache. Called at startup so the first join does not pay for
// a database read.
func (s *Service) Load(ctx context.Context) error {
	all, err := s.repo.All(ctx)
	if err != nil {
		return fmt.Errorf("settings: load: %w", err)
	}
	s.mu.Lock()
	s.cache = all
	s.loaded = true
	s.mu.Unlock()
	return nil
}

// Value resolves one setting, falling back to its catalogue default.
//
// Never returns an error and never returns empty for a known key. A caller on
// the join path asking "may this person share their screen" needs an answer,
// and the only safe answer when the store is unreachable is the documented
// default rather than an empty string that every call site would have to
// remember to interpret.
func (s *Service) Value(ctx context.Context, k Key) string {
	spec, ok := specFor(k)
	if !ok {
		return ""
	}
	s.mu.RLock()
	got, present := s.cache[k]
	s.mu.RUnlock()
	if present && got.Value != "" {
		return got.Value
	}
	return spec.Default
}

// Bool resolves a boolean setting.
func (s *Service) Bool(ctx context.Context, k Key) bool {
	return s.Value(ctx, k) == "true"
}

// All returns every setting with its resolved value, for the console.
func (s *Service) All(ctx context.Context, a Actor) ([]Setting, error) {
	if !a.Admin {
		return nil, ErrForbidden
	}
	out := make([]Setting, 0, len(Catalogue))
	for _, spec := range Catalogue {
		v := s.Value(ctx, spec.Key)
		s.mu.RLock()
		stored := s.cache[spec.Key]
		s.mu.RUnlock()
		out = append(out, Setting{
			Key: spec.Key, Value: v,
			UpdatedAt: stored.UpdatedAt, UpdatedBy: stored.UpdatedBy,
		})
	}
	return out, nil
}

// Set changes one setting.
//
// Administrator only, validated against the catalogue, and audited. Deployment
// policy is exactly the kind of change that needs to be attributable months
// later: "who turned remote control back on" is a question somebody eventually
// asks, and an answer of "it has always been like that" is how incidents get
// written up badly.
func (s *Service) Set(ctx context.Context, a Actor, k Key, v, ip, ua string) error {
	if !a.Admin {
		s.rec.Record(ctx, audit.Event{
			ActorID: a.ID, ActorEmail: a.Email,
			Action: audit.ActionSettingChanged, Subject: string(k),
			Outcome: audit.OutcomeDenied, IP: ip, UserAgent: ua,
			Detail: "not an administrator",
		})
		return ErrForbidden
	}

	spec, ok := specFor(k)
	if !ok {
		return ErrUnknownKey
	}
	if !validFor(spec, v) {
		return ErrBadValue
	}

	before := s.Value(ctx, k)
	rec := Setting{Key: k, Value: v, UpdatedAt: time.Now().UTC(), UpdatedBy: a.ID}
	if err := s.repo.Put(ctx, rec); err != nil {
		return err
	}

	s.mu.Lock()
	s.cache[k] = rec
	s.mu.Unlock()

	s.rec.Record(ctx, audit.Event{
		ActorID: a.ID, ActorEmail: a.Email,
		Action: audit.ActionSettingChanged, Subject: string(k),
		Outcome: audit.OutcomeSuccess, IP: ip, UserAgent: ua,
		Detail: fmt.Sprintf("%s: %q -> %q", spec.Label, before, v),
	})
	s.log.Info("deployment policy changed",
		"key", k, "from", before, "to", v, "by", a.Email)
	return nil
}

// validFor checks a value against what the setting accepts.
func validFor(spec Spec, v string) bool {
	switch spec.Type {
	case "bool":
		return v == "true" || v == "false"
	case "choice":
		for _, c := range spec.Choices {
			if c == v {
				return true
			}
		}
		return false
	case "text":
		// Bounded, and single-line. A control character in a setting is either a
		// paste accident or an attempt to break whatever reads it back; neither is
		// a value worth storing. Everything further -- which domains are real,
		// which are public providers -- is decided where the value is read, so an
		// ordinary typo is corrected rather than refused at the door.
		if len(v) > 400 {
			return false
		}
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				return false
			}
		}
		return true
	}
	return false
}

// ClampAdmission holds a host's chosen admission policy to what the
// deployment permits.
//
// This is where the ceiling is actually enforced, rather than in the console
// that offers it. A host who sends "open" to a deployment whose floor is
// "trusted" gets "trusted", not an error: the request is not malicious, it is
// a client that does not know the local policy, and the correct response is to
// apply the policy rather than to refuse the meeting.
//
// Ordered from most open to most closed, so "may not be more open than" is a
// comparison rather than a table of special cases.
// SeedOrgDomains writes a guessed organisation domain, but only if the
// setting has never been written.
//
// Never overwrites. An administrator who deliberately cleared this to close
// the address book must not have it reopened by a restart, which is the whole
// difference between a default and a decision.
func (s *Service) SeedOrgDomains(ctx context.Context, domain string) (bool, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" || publicProvider[domain] {
		return false, nil
	}
	// "Never written" is presence in the cache, which is loaded from the
	// store at start and updated on every Set. A row that exists and is empty
	// counts as written: somebody cleared it on purpose.
	s.mu.RLock()
	_, written := s.cache[KeyOrgDomains]
	s.mu.RUnlock()
	if written {
		return false, nil
	}
	rec := Setting{Key: KeyOrgDomains, Value: domain, UpdatedAt: time.Now().UTC()}
	if err := s.repo.Put(ctx, rec); err != nil {
		return false, err
	}
	s.mu.Lock()
	s.cache[KeyOrgDomains] = rec
	s.mu.Unlock()
	return true, nil
}

// OrgDomains returns the deployment's own email domains, lowercased.
//
// A short guard list is applied on the way out. An administrator who types
// "gmail.com" here has misunderstood the setting, and the consequence -- every
// account on that provider becoming browsable by every other -- is bad enough
// to be worth refusing rather than honouring.
func (s *Service) OrgDomains(ctx context.Context) []string {
	raw := s.Value(ctx, KeyOrgDomains)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		d := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "@")))
		if d == "" || !strings.Contains(d, ".") || publicProvider[d] {
			continue
		}
		out = append(out, d)
	}
	return out
}

// Providers whose users are the general public, not one organisation.
var publicProvider = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "outlook.com": true,
	"hotmail.com": true, "live.com": true, "msn.com": true,
	"yahoo.com": true, "yahoo.co.in": true, "ymail.com": true,
	"icloud.com": true, "me.com": true, "aol.com": true,
	"proton.me": true, "protonmail.com": true, "gmx.com": true,
	"mail.com": true, "zoho.com": true, "yandex.com": true,
	"rediffmail.com": true, "qq.com": true, "163.com": true,
}

// DirectoryScope reports how far a directory search may reach. Anything
// unrecognised is the narrower answer, as everywhere else in this file.
func (s *Service) DirectoryScope(ctx context.Context) string {
	switch s.Value(ctx, KeyDirectoryScope) {
	case "everyone":
		return "everyone"
	case "organisation":
		return "organisation"
	default:
		// Anything unrecognised is the middle rung, which is the documented
		// default rather than the strictest: a value nobody chose should not
		// silently cut a deployment off from everyone it works with.
		return "connections"
	}
}

func (s *Service) ClampAdmission(ctx context.Context, chosen string) string {
	rank := map[string]int{"open": 0, "trusted": 1, "knock": 2}
	floor := s.Value(ctx, KeyAdmissionFloor)

	c, okC := rank[chosen]
	f, okF := rank[floor]
	if !okC {
		// Unrecognised input becomes the strictest, as everywhere else.
		return "knock"
	}
	if !okF {
		return chosen
	}
	if c < f {
		return floor
	}
	return chosen
}
