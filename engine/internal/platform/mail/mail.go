// Package mail sends plain-text email through an SMTP server the operator
// names: their own, their organisation's, or any provider's. Nothing here is
// tied to a vendor.
//
// The connection is always encrypted before anything is said that matters:
// STARTTLS is required on a submission port (the session ends if the server
// does not offer it) or TLS from the first byte on port 465. Credentials and
// mail never cross the network in the clear.
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"net/smtp"
	"strings"
	"sync"
	"time"
)

// Message is one plain-text email to one address.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Sender delivers a message.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// SMTP sends through an SMTP server.
type SMTP struct {
	Addr     string // host:port
	Username string // empty: no authentication
	Password string
	From     string // the sender address
	// ImplicitTLS speaks TLS from the first byte (port 465). Otherwise the
	// server must offer STARTTLS.
	ImplicitTLS bool
	RootCAs     *x509.CertPool // nil: the system's roots
}

// ErrHeader is returned for an address or subject that would let its value
// write extra headers (a CR or LF in it).
var ErrHeader = errors.New("mail: header value contains a line break")

func clean(v string) error {
	if strings.ContainsAny(v, "\r\n") {
		return ErrHeader
	}
	return nil
}

// bareAddress accepts exactly one address with no display name.
func bareAddress(v string) (string, error) {
	if err := clean(v); err != nil {
		return "", err
	}
	a, err := netmail.ParseAddress(v)
	if err != nil || a.Name != "" || a.Address != strings.TrimSpace(v) {
		return "", fmt.Errorf("mail: not a single bare address: %q", v)
	}
	return a.Address, nil
}

// compose renders the message with headers. The body is quoted-printable, so
// any text reaches any server intact whatever its 8-bit support, and the
// server's DATA writer dot-stuffs it.
func compose(from, to, subject, body string, now time.Time) ([]byte, error) {
	var b bytes.Buffer
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	domain := from[strings.LastIndexByte(from, '@')+1:]
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\n",
		from, to, mime.QEncoding.Encode("utf-8", subject), now.Format(time.RFC1123Z), hex.EncodeToString(id), domain)
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\nAuto-Submitted: auto-generated\r\n\r\n")
	qp := quotedprintable.NewWriter(&b)
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	if _, err := qp.Write([]byte(body)); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Send delivers m, honouring ctx's deadline for the whole conversation.
func (s *SMTP) Send(ctx context.Context, m Message) error {
	from, err := bareAddress(s.From)
	if err != nil {
		return err
	}
	to, err := bareAddress(m.To)
	if err != nil {
		return err
	}
	if err := clean(m.Subject); err != nil {
		return err
	}
	msg, err := compose(from, to, m.Subject, m.Body, time.Now())
	if err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return err
	}
	tcfg := &tls.Config{ServerName: host, RootCAs: s.RootCAs, MinVersion: tls.VersionTLS12}

	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if s.ImplicitTLS {
		tc := tls.Client(conn, tcfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return err
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if !s.ImplicitTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("mail: the server does not offer STARTTLS; refusing to send in the clear")
		}
		if err := c.StartTLS(tcfg); err != nil {
			return err
		}
	}
	if s.Username != "" {
		// PlainAuth itself refuses an unencrypted connection to anything but
		// localhost; this one is always encrypted by now.
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, host)); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// Queue sends in the background so a request never waits on (or reveals,
// by its timing) a mail server. It is bounded: when it is full a message is
// dropped and logged rather than held.
type Queue struct {
	s    Sender
	log  *slog.Logger
	ch   chan Message
	done chan struct{}

	mu     sync.Mutex // guards closed and every send on ch
	closed bool
}

// QueueLen bounds the messages waiting to be sent.
const QueueLen = 256

// NewQueue starts one sender goroutine; Close stops it.
func NewQueue(s Sender, log *slog.Logger) *Queue {
	q := &Queue{s: s, log: log, ch: make(chan Message, QueueLen), done: make(chan struct{})}
	go func() {
		defer close(q.done)
		for m := range q.ch {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := q.s.Send(ctx, m); err != nil {
				// The address is logged, never the body: it may carry a link
				// that grants access.
				q.log.Error("mail: send failed", "to", m.To, "err", err)
			}
			cancel()
		}
	}()
	return q
}

// Enqueue reports whether m was accepted; after Close it never is.
func (q *Queue) Enqueue(m Message) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	select {
	case q.ch <- m:
		return true
	default:
		q.log.Error("mail: queue full, message dropped", "to", m.To)
		return false
	}
}

// Close sends what is queued, then returns. Safe to call more than once and
// from several goroutines.
func (q *Queue) Close() {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.ch)
	}
	q.mu.Unlock()
	<-q.done
}
