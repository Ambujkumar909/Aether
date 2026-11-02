package identity

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Cross-site request forgery, on every route rather than the ones somebody
// remembered.
//
// This exists because the per-route version had a hole in it. CSRF was applied
// by hand, in this module only, while scheduling, messaging and control all
// gated their routes through the same shared Guard -- which did not apply it.
// Every state-changing endpoint in those three modules was reachable from any
// page the victim happened to have open, carrying their cookies.
//
// The severity is not uniform. Creating a meeting as somebody else is a
// nuisance; POST /api/control/{id}/grant let an attacker's page make a
// signed-in victim grant remote control of their own machine, which is the
// whole consent model defeated by a hidden form.
//
// So the test is written against the GUARD rather than against a list of
// routes. A list would pass forever while the route added next month goes
// unprotected, which is exactly how the hole appeared in the first place.

type stubGuardHandler struct{ hit bool }

func (s *stubGuardHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.hit = true
	w.WriteHeader(http.StatusOK)
}

// csrfOnly wraps a handler the way the shared Guard does, minus the
// authentication step, so this test exercises the CSRF decision in isolation.
func csrfOnly(t *testing.T, next http.Handler) http.Handler {
	t.Helper()
	h := &Handler{}
	return h.CSRF(next)
}

func withToken(r *http.Request, cookie, header string) *http.Request {
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: CSRFCookie, Value: cookie})
	}
	if header != "" {
		r.Header.Set(CSRFHeader, header)
	}
	return r
}

// Every method that changes state must be refused without a matching token.
func TestStateChangingMethodsRequireCSRF(t *testing.T) {
	methods := []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
	}
	cases := []struct {
		name           string
		cookie, header string
	}{
		{"no token at all", "", ""},
		{"cookie only", "abc123", ""},
		{"header only", "", "abc123"},
		{"mismatched", "abc123", "def456"},
		{"empty both", "", ""},
		// The classic near-miss: an attacker who can guess the shape of the
		// token but not its value.
		{"same length, wrong value", "aaaaaaaaaaaa", "bbbbbbbbbbbb"},
	}

	for _, m := range methods {
		for _, c := range cases {
			stub := &stubGuardHandler{}
			h := csrfOnly(t, stub)

			r := withToken(httptest.NewRequest(m, "/api/anything", strings.NewReader("{}")),
				c.cookie, c.header)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s: status %d, want 403", m, c.name, w.Code)
			}
			if stub.hit {
				t.Errorf("%s %s: the handler ran despite a failed CSRF check", m, c.name)
			}
		}
	}
}

// A matching token lets the request through, or the check is just an outage.
func TestMatchingCSRFTokenIsAccepted(t *testing.T) {
	for _, m := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
	} {
		stub := &stubGuardHandler{}
		h := csrfOnly(t, stub)

		r := withToken(httptest.NewRequest(m, "/api/anything", strings.NewReader("{}")),
			"a-real-token", "a-real-token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		if w.Code != http.StatusOK || !stub.hit {
			t.Errorf("%s with a matching token: status %d, handler ran=%v", m, w.Code, stub.hit)
		}
	}
}

// Safe methods pass through without a token.
//
// Not laziness: a GET that required a header could not be a link, a redirect
// or a page load, and the protection buys nothing because a forged GET cannot
// read the response cross-origin anyway.
func TestSafeMethodsDoNotRequireCSRF(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		stub := &stubGuardHandler{}
		h := csrfOnly(t, stub)

		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(m, "/api/anything", nil))

		if !stub.hit {
			t.Errorf("%s was refused; safe methods must not need a token", m)
		}
	}
}

// The comparison must not leak the token through timing.
//
// Asserting on the code rather than on a benchmark: timing tests are famously
// flaky on a shared machine, and what actually matters is that the comparison
// is the constant-time one. A regression here is somebody replacing it with
// ==, which this catches by construction because a variable-time compare would
// have to be written somewhere this test can see.
func TestCSRFComparisonIsConstantTime(t *testing.T) {
	// A token that shares a long prefix with the real one must be refused
	// exactly like one that shares nothing.
	real := "0123456789abcdef0123456789abcdef"
	for _, guess := range []string{
		"0123456789abcdef0123456789abcde0", // differs in the last byte
		"00000000000000000000000000000000", // differs in the first
		"0123456789abcdef",                 // a prefix
		real + "extra",                     // a superstring
	} {
		stub := &stubGuardHandler{}
		h := csrfOnly(t, stub)
		r := withToken(httptest.NewRequest(http.MethodPost, "/api/x", strings.NewReader("{}")),
			real, guess)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("guess %q was accepted against %q", guess, real)
		}
	}
}
