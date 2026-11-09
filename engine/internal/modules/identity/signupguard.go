package identity

import (
	"bufio"
	"context"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Guards on public sign-up, beyond the per-address limiter and the emailed
// link: a block list of domains (throwaway-mail services, chosen by the
// operator), a check that the address's domain can receive mail at all
// before any is sent, and counters by outcome for the metrics. None of them
// changes the answer a caller sees: an address that is blocked, or whose
// domain has no mail server, is told the same words as a new one.

// SignupGuard is what the operator configured.
type SignupGuard struct {
	blocked map[string]bool
	// LookupDomain reports whether a domain can receive mail (an MX record,
	// or failing that an address). Nil skips the check; tests inject one.
	LookupDomain func(ctx context.Context, domain string) bool
}

// LoadBlockedDomains reads a file of domains, one per line, # comments.
func LoadBlockedDomains(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		out[strings.ToLower(strings.TrimPrefix(line, "@"))] = true
	}
	return out, sc.Err()
}

// NewSignupGuard makes a guard with the given blocked domains and the real
// resolver.
func NewSignupGuard(blocked map[string]bool) *SignupGuard {
	return &SignupGuard{blocked: blocked, LookupDomain: domainReceivesMail}
}

// Blocked reports whether an address's domain, or any parent of it, is on
// the list ("mail.throwaway.example" is blocked by "throwaway.example").
func (g *SignupGuard) Blocked(email string) bool {
	if g == nil || len(g.blocked) == 0 {
		return false
	}
	d := domainOf(email)
	for d != "" {
		if g.blocked[d] {
			return true
		}
		i := strings.IndexByte(d, '.')
		if i < 0 {
			return false
		}
		d = d[i+1:]
	}
	return false
}

// domainReceivesMail is the real check, bounded to two seconds: a domain
// with an MX record, or with an address when it has none (RFC 5321 falls
// back to the A record).
func domainReceivesMail(ctx context.Context, domain string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if mx, err := net.DefaultResolver.LookupMX(ctx, domain); err == nil && len(mx) > 0 {
		// A single "." MX is the explicit "no mail here" of RFC 7505.
		return !(len(mx) == 1 && (mx[0].Host == "." || mx[0].Host == ""))
	}
	addrs, err := net.DefaultResolver.LookupHost(ctx, domain)
	return err == nil && len(addrs) > 0
}

// SignupCounts counts sign-up outcomes, for the metrics (cmd/aether).
type SignupCounts struct {
	Created, Pending, Existing, Blocked, NoMail, Completed atomic.Uint64
}

var signupCounts SignupCounts

// SignupTotals reads the counters.
func SignupTotals() map[string]uint64 {
	c := &signupCounts
	return map[string]uint64{
		"created":   c.Created.Load(),   // without verification
		"pending":   c.Pending.Load(),   // link sent
		"completed": c.Completed.Load(), // link used
		"existing":  c.Existing.Load(),  // address already had an account
		"blocked":   c.Blocked.Load(),   // domain on the block list
		"no_mail":   c.NoMail.Load(),    // domain cannot receive mail
	}
}

// globalLimiter is the sign-up ceiling across every address.
type globalLimiter struct {
	mu     sync.Mutex
	tokens float64
	rate   float64 // per second
	burst  float64
	last   time.Time
}

func newGlobalLimiter(perMinute int) *globalLimiter {
	if perMinute <= 0 {
		return nil
	}
	return &globalLimiter{tokens: float64(perMinute), rate: float64(perMinute) / 60, burst: float64(perMinute), last: time.Now()}
}

func (g *globalLimiter) allow() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.tokens = min(g.burst, g.tokens+now.Sub(g.last).Seconds()*g.rate)
	g.last = now
	if g.tokens < 1 {
		return false
	}
	g.tokens--
	return true
}
