package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOnlyJSONOrAnUploadReachesAnAPIHandler(t *testing.T) {
	reached := 0
	h := BodyTypes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++ }))
	body := `{"email":"a@example.com","password":"b="}`
	for _, c := range []struct {
		method, path, ctype, body string
		want                      int
	}{
		{"POST", "/api/identity/login", "text/plain", body, 415},                        // a cross-site form
		{"POST", "/api/identity/login", "text/plain;charset=UTF-8", body, 415},          // the same, as browsers send it
		{"POST", "/api/identity/login", "application/x-www-form-urlencoded", body, 415}, // a plain form
		{"POST", "/api/identity/login", "", body, 415},                                  // no type at all
		{"PATCH", "/api/identity/users/x/role", "text/plain", body, 415},
		{"POST", "/api/identity/login", "application/json", body, 200},
		{"POST", "/api/identity/login", "application/json; charset=utf-8", body, 200},
		{"POST", "/api/messaging/conversations/x/files", "multipart/form-data; boundary=x", "--x--", 200},
		{"GET", "/api/identity/me", "", "", 200},
		{"POST", "/api/identity/logout", "", "", 200}, // no body, no type needed
		{"POST", "/somewhere-else", "text/plain", body, 200},
	} {
		reached = 0
		r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		if c.body == "" {
			r = httptest.NewRequest(c.method, c.path, nil)
		}
		if c.ctype != "" {
			r.Header.Set("Content-Type", c.ctype)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		got := w.Code
		if got == 200 && reached != 1 {
			got = -1
		}
		if got != c.want || (c.want == 415 && reached != 0) {
			t.Errorf("%s %s %q: %d (handler reached %d), want %d", c.method, c.path, c.ctype, w.Code, reached, c.want)
		}
	}
}
