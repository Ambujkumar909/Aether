package main

import (
	"strings"
	"testing"
)

func TestLinksPointOnlyWhereTheOperatorSaid(t *testing.T) {
	for _, c := range []struct {
		raw     string
		domains []string
		want    string
		bad     bool
	}{
		{"", nil, "", false},
		{"", []string{"meet.example.com", "alt.example.com"}, "https://meet.example.com", false},
		{"https://meet.example.com", nil, "https://meet.example.com", false},
		{"https://meet.example.com/", nil, "https://meet.example.com", false},
		{"https://meet.example.com:8443", nil, "https://meet.example.com:8443", false},
		{"http://localhost:8080", nil, "http://localhost:8080", false},
		{"http://127.0.0.1:8080", nil, "http://127.0.0.1:8080", false},
		{"http://meet.example.com", nil, "", true},          // a reset link in clear text
		{"https://meet.example.com/app", nil, "", true},     // origins only
		{"https://meet.example.com/?x=1", nil, "", true},    // no query
		{"https://meet.example.com/#x", nil, "", true},      // no fragment
		{"https://user:pw@meet.example.com", nil, "", true}, // no userinfo
		{"javascript:alert(1)", nil, "", true},
		{"meet.example.com", nil, "", true},
		{"ftp://meet.example.com", nil, "", true},
	} {
		got, err := parsePublicURL(c.raw, c.domains)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("parsePublicURL(%q, %v) = %q, %v", c.raw, c.domains, got, err)
		}
	}
}

func TestTheResetEmailCarriesTheTokenInTheFragment(t *testing.T) {
	b := resetBody("https://meet.example.com", "zoe@example.com", "TOKEN123")
	if !strings.Contains(b, "\nhttps://meet.example.com/#reset=TOKEN123\n") || !strings.Contains(b, "30 minutes") {
		t.Fatalf("body:\n%s", b)
	}
}

func TestPort465IsTLSFromTheStartAndAnyOtherPortSTARTTLS(t *testing.T) {
	s, err := newSMTP("smtp.example.com:465", "u", "p", "noreply@example.com", "")
	if err != nil || !s.ImplicitTLS {
		t.Fatalf("465: %+v %v", s, err)
	}
	s, err = newSMTP("smtp.example.com:587", "u", "p", "noreply@example.com", "")
	if err != nil || s.ImplicitTLS {
		t.Fatalf("587: %+v %v", s, err)
	}
	for _, bad := range [][2]string{{"smtp.example.com", "noreply@example.com"}, {"smtp.example.com:587", "Aether <noreply@example.com>"}, {"smtp.example.com:587", "noreply@example.com\r\nBcc: x@example.com"}} {
		if _, err := newSMTP(bad[0], "", "", bad[1], ""); err == nil {
			t.Errorf("accepted %q from %q", bad[0], bad[1])
		}
	}
}
