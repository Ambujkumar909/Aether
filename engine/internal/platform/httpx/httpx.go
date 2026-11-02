// Package httpx is shared HTTP plumbing: JSON responses, middleware, and the
// small number of decisions that must be identical across every module.
package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// JSON writes a value with the correct headers.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already out; there is nothing useful left to do
		// but avoid pretending the write succeeded.
		return
	}
}

// ErrorBody is the single error shape every endpoint returns, so a client
// never has to guess which of three formats it received.
type ErrorBody struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
	Details string `json:"details,omitempty"`
}

// Error writes a problem response.
func Error(w http.ResponseWriter, status int, code, msg string) {
	JSON(w, status, ErrorBody{Error: msg, Code: code})
}

// Decode reads a JSON body with a size limit.
//
// The limit is not optional. An endpoint that will read an unbounded body is
// a memory exhaustion primitive available to anyone who can reach it.
func Decode(w http.ResponseWriter, r *http.Request, v any) error {
	const maxBody = 1 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	// Reject trailing content: two JSON documents in one body is never
	// something a legitimate client sends and is often something a
	// request-smuggling attempt does.
	if dec.More() {
		return errors.New("unexpected trailing content in request body")
	}
	return nil
}

// ClientIP extracts the caller address.
//
// X-Forwarded-For is honoured only when the immediate peer is trusted,
// because it is a client-supplied header and treating it as authoritative
// lets anyone forge the source address in the audit log -- which is exactly
// the field an attacker most wants to control.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Leftmost entry is the original client as recorded by the first
			// proxy in the chain.
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
		if xr := r.Header.Get("X-Real-IP"); xr != "" {
			return strings.TrimSpace(xr)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(c int) {
	w.status = c
	w.ResponseWriter.WriteHeader(c)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, which is
// what keeps WebSocket upgrades working through this middleware.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Logging records one line per request.
func Logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		lvl := slog.LevelDebug
		if sw.status >= 500 {
			lvl = slog.LevelError
		} else if sw.status >= 400 {
			lvl = slog.LevelWarn
		}
		log.Log(r.Context(), lvl, "http",
			"method", r.Method, "path", r.URL.Path, "status", sw.status,
			"bytes", sw.bytes, "ms", time.Since(start).Milliseconds())
	})
}

// Recover turns a panic into a 500 rather than a dropped connection and a
// dead server.
func Recover(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				log.Error("panic serving request",
					"path", r.URL.Path, "panic", p, "stack", string(debug.Stack()))
				Error(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// SecurityHeaders applies the defaults that should never have to be argued
// about per endpoint.
func SecurityHeaders(secure bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(self), microphone=(self), display-capture=(self)")

		// The CSP allows only same-origin resources. blob: is needed for
		// media streams; nothing external is permitted at all, which is what
		// keeps a compromised dependency from exfiltrating a session.
		//
		// connect-src is 'self' alone, and it is worth saying why the obvious
		// addition is wrong. This used to read "'self' ws: wss:", which looks
		// like it is permitting the meeting socket and is in fact permitting
		// EVERY WebSocket host on the internet: ws: and wss: are scheme
		// wildcards, not same-origin shorthands. Injected script could open a
		// socket to an attacker and stream whatever it could read straight
		// out, which is precisely the exfiltration this policy exists to stop.
		// 'self' already covers same-origin ws:// and wss://.
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; "+
				"script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; "+
				"connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; "+
				"form-action 'self'; object-src 'none'")

		// HSTS, but only when the connection is actually secure.
		//
		// Sending it over plain HTTP is at best ignored and at worst a way to
		// lock a developer out of their own localhost for a year, so it is
		// gated on the same flag that decides whether cookies are Secure.
		if secure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

// Limiter is a per-key token bucket.
//
// Applied to authentication endpoints, this is what turns an online password
// guessing attack from "a few million attempts a day" into "a few thousand",
// at which point the account lockout has time to work and the audit log has
// time to be read by a human.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   float64
	last    time.Time
}

type bucket struct {
	tokens float64
	seen   time.Time
}

// NewLimiter allows burst requests immediately and refills at rate per second.
func NewLimiter(rate, burst float64) *Limiter {
	return &Limiter{buckets: make(map[string]*bucket), rate: rate, burst: burst}
}

// Allow reports whether a key may proceed.
func (l *Limiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// Opportunistic sweep so an attacker cycling source addresses cannot
	// grow this map without bound.
	if now.Sub(l.last) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.seen) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.last = now
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, seen: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.seen).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.seen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// RateLimit rejects requests over the limit with 429 and a Retry-After.
func RateLimit(l *Limiter, keyFn func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(keyFn(r)) {
			w.Header().Set("Retry-After", strconv.Itoa(30))
			Error(w, http.StatusTooManyRequests, "rate_limited",
				"too many requests, please wait a moment")
			return
		}
		next.ServeHTTP(w, r)
	})
}
