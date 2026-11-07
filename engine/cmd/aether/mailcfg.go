package main

import (
	"crypto/x509"
	"fmt"
	"net"
	netmail "net/mail"
	"net/url"
	"os"
	"strings"

	"github.com/aether-rtc/aether/internal/platform/mail"
)

// parsePublicURL returns the origin links in email point to: -public-url,
// or https:// and the first ACME domain. Never anything a request supplied:
// a link built from the Host header is a link an attacker can aim.
func parsePublicURL(raw string, domains []string) (string, error) {
	if raw == "" {
		if len(domains) == 0 {
			return "", nil
		}
		return "https://" + domains[0], nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("-public-url %q: want an origin such as https://meet.example.com", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", fmt.Errorf("-public-url %q: links in email must be https", raw)
		}
	default:
		return "", fmt.Errorf("-public-url %q: want https://", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

func checkFrom(from string) error {
	a, err := netmail.ParseAddress(from)
	if err != nil || a.Name != "" || a.Address != from {
		return fmt.Errorf("-smtp-from %q: want a bare address such as noreply@example.com", from)
	}
	return nil
}

// newSMTP: port 465 speaks TLS from the start; any other port must offer
// STARTTLS. caFile, if set, is a PEM certificate authority trusted for the
// mail server beside the system's (a private CA inside an organisation).
func newSMTP(addr, user, password, from, caFile string) (*mail.SMTP, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("-smtp-addr %q: want host:port", addr)
	}
	if err := checkFrom(from); err != nil {
		return nil, err
	}
	s := &mail.SMTP{Addr: addr, Username: user, Password: password, From: from, ImplicitTLS: port == "465"}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("-smtp-ca: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("-smtp-ca %s: no certificate in it", caFile)
		}
		s.RootCAs = pool
	}
	return s, nil
}

// resetSender queues the email carrying a reset link. The token travels in
// the fragment, which browsers never send to a server: it stays out of
// access logs, proxies and Referer headers.
func resetSender(q *mail.Queue, base string) func(email, token string) {
	return func(email, token string) {
		q.Enqueue(mail.Message{To: email, Subject: "Reset your Aether password", Body: resetBody(base, email, token)})
	}
}

// verifySender mails the link that finishes a sign-up; noticeSender tells an
// existing account's owner that somebody tried to sign up with their address.
func verifySender(q *mail.Queue, base string) func(email, token string) {
	return func(email, token string) {
		q.Enqueue(mail.Message{To: email, Subject: "Finish creating your Aether account", Body: verifyBody(base, token)})
	}
}

func noticeSender(q *mail.Queue, base string) func(email string) {
	return func(email string) {
		q.Enqueue(mail.Message{To: email, Subject: "Someone tried to sign up with your address", Body: noticeBody(base)})
	}
}

func verifyBody(base, token string) string {
	return strings.Join([]string{
		"Someone, hopefully you, signed up for Aether with this address.",
		"",
		"To finish, open this link within 24 hours and enter the password you chose:",
		"",
		base + "/#verify=" + token,
		"",
		"If it was not you, ignore this email: no account exists until the link is used.",
	}, "\n")
}

func noticeBody(base string) string {
	return strings.Join([]string{
		"Someone tried to create an Aether account with this address, which already has one.",
		"Nothing was changed.",
		"",
		"If it was you, sign in at " + base + " -- or use \"Forgot your password?\" there.",
		"If it was not you, you can ignore this email.",
	}, "\n")
}

func resetBody(base, email, token string) string {
	return strings.Join([]string{
		"Someone asked to reset the password for the Aether account " + email + ".",
		"",
		"To choose a new password, open this link within 30 minutes:",
		"",
		base + "/#reset=" + token,
		"",
		"If you did not ask, ignore this email: your password has not changed,",
		"and the link stops working on its own.",
	}, "\n")
}
