package identity

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The public config never carries a relay credential.
//
// /api/identity/config is read before sign-in (the sign-in screen needs
// allowSignup), and it once minted a TURN credential for anyone who asked.
// A credential is now only for a session that verifies.
func TestConfigMintsARelayCredentialOnlyForASignedInSession(t *testing.T) {
	svc, _ := testService(t)
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)), false, false)
	minted := 0
	h.ICEServers = func() []any { minted++; return []any{map[string]any{"urls": []string{"turn:192.0.2.1:3478"}}} }

	get := func(cookie string) map[string]any {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/api/identity/config", nil)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		h.config(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("config: %d", w.Code)
		}
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return body
	}

	if b := get(""); b["iceServers"] != nil || b["allowSignup"] == nil {
		t.Fatalf("a stranger got %v", b)
	}
	if b := get("forged-or-expired-token"); b["iceServers"] != nil {
		t.Fatalf("a made-up session got a relay credential: %v", b)
	}
	if minted != 0 {
		t.Fatalf("a credential was minted %d times for nobody", minted)
	}

	mkUser(t, svc, "member@example.com", RoleMember)
	_, token, err := svc.Authenticate(context.Background(), "member@example.com", "correct-horse-battery", "10.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	if b := get(token); b["iceServers"] == nil {
		t.Fatalf("a signed-in member got no relay: %v", b)
	}
	if minted != 1 {
		t.Fatalf("minted %d credentials for one signed-in request", minted)
	}
}
