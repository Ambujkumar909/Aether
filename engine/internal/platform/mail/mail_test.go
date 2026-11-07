package mail

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"log/slog"
	"math/big"
	"mime"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"net/textproto"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A real SMTP server on a real socket: enough of RFC 5321 for the client.
type smtpd struct {
	ln            net.Listener
	cert          tls.Certificate
	offerStartTLS bool
	implicit      bool

	mu       sync.Mutex
	commands []string // every command line, as received
	mails    []received
}

type received struct {
	from, to string
	data     []byte
	secure   bool
	user     string
	pass     string
}

func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "smtp test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func startSMTPD(t *testing.T, offerStartTLS, implicit bool) (*smtpd, *x509.CertPool) {
	t.Helper()
	cert, pool := selfSigned(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &smtpd{ln: ln, cert: cert, offerStartTLS: offerStartTLS, implicit: implicit}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go d.serve(c)
		}
	}()
	return d, pool
}

func (d *smtpd) serve(c net.Conn) {
	defer c.Close()
	cfg := &tls.Config{Certificates: []tls.Certificate{d.cert}}
	secure := false
	if d.implicit {
		tc := tls.Server(c, cfg)
		if tc.Handshake() != nil {
			return
		}
		c, secure = tc, true
	}
	tp := textproto.NewConn(c)
	tp.PrintfLine("220 test ESMTP")
	var cur received
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		d.mu.Lock()
		d.commands = append(d.commands, line)
		d.mu.Unlock()
		f := strings.Fields(line)
		if len(f) == 0 {
			tp.PrintfLine("500 empty")
			continue
		}
		switch strings.ToUpper(f[0]) {
		case "EHLO":
			exts := []string{"test"}
			if d.offerStartTLS && !secure {
				exts = append(exts, "STARTTLS")
			}
			exts = append(exts, "AUTH PLAIN", "8BITMIME")
			for i, e := range exts {
				sep := "-"
				if i == len(exts)-1 {
					sep = " "
				}
				tp.PrintfLine("250%s%s", sep, e)
			}
		case "STARTTLS":
			tp.PrintfLine("220 go ahead")
			tc := tls.Server(c, cfg)
			if tc.Handshake() != nil {
				return
			}
			c, secure = tc, true
			tp = textproto.NewConn(c)
		case "AUTH":
			raw, _ := base64.StdEncoding.DecodeString(f[len(f)-1])
			parts := strings.Split(string(raw), "\x00")
			if len(parts) == 3 {
				cur.user, cur.pass = parts[1], parts[2]
			}
			tp.PrintfLine("235 ok")
		case "MAIL":
			cur.from = strings.Trim(strings.TrimPrefix(f[1], "FROM:"), "<>")
			tp.PrintfLine("250 ok")
		case "RCPT":
			cur.to = strings.Trim(strings.TrimPrefix(f[1], "TO:"), "<>")
			tp.PrintfLine("250 ok")
		case "DATA":
			tp.PrintfLine("354 go")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			cur.data, cur.secure = data, secure
			d.mu.Lock()
			d.mails = append(d.mails, cur)
			d.mu.Unlock()
			tp.PrintfLine("250 queued")
		case "QUIT":
			tp.PrintfLine("221 bye")
			return
		default:
			tp.PrintfLine("250 ok")
		}
	}
}

func (d *smtpd) snapshot() ([]string, []received) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.commands...), append([]received(nil), d.mails...)
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

// The body a person would read, decoded from what arrived.
func readBody(t *testing.T, data []byte) (*netmail.Message, string) {
	t.Helper()
	msg, err := netmail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not a mail message: %v\n%s", err, data)
	}
	if msg.Header.Get("Content-Transfer-Encoding") != "quoted-printable" {
		t.Fatalf("transfer encoding %q", msg.Header.Get("Content-Transfer-Encoding"))
	}
	b, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if err != nil {
		t.Fatal(err)
	}
	return msg, string(b)
}

const tricky = "Hello Zoë,\n.a line that starts with a dot\n..and two\nand a long line that keeps going well past the seventy-six character limit for one encoded line of text\nlast line"

func TestDeliversOverSTARTTLSWithCredentials(t *testing.T) {
	d, pool := startSMTPD(t, true, false)
	s := &SMTP{Addr: d.ln.Addr().String(), Username: "aether", Password: "s3cret", From: "noreply@aether.test", RootCAs: pool}
	if err := s.Send(ctx(t), Message{To: "zoe@example.com", Subject: "Réinitialiser", Body: tricky}); err != nil {
		t.Fatal(err)
	}
	cmds, mails := d.snapshot()
	if len(mails) != 1 {
		t.Fatalf("%d messages arrived", len(mails))
	}
	m := mails[0]
	if !m.secure || m.user != "aether" || m.pass != "s3cret" {
		t.Fatalf("secure %v, credentials %q/%q", m.secure, m.user, m.pass)
	}
	// Nothing but the greeting and STARTTLS before encryption.
	for _, c := range cmds[:indexOf(cmds, "STARTTLS")] {
		if !strings.HasPrefix(c, "EHLO") {
			t.Fatalf("%q was sent before STARTTLS", c)
		}
	}
	msg, body := readBody(t, m.data)
	// The test server's dot reader hands lines back LF-terminated.
	if body = strings.TrimSuffix(body, "\n"); body != tricky {
		t.Fatalf("body changed in transit:\n%q\nwant\n%q", body, tricky)
	}
	dec := new(mime.WordDecoder)
	if subj, _ := dec.DecodeHeader(msg.Header.Get("Subject")); subj != "Réinitialiser" {
		t.Fatalf("subject %q", subj)
	}
	if msg.Header.Get("To") != "zoe@example.com" || msg.Header.Get("From") != "noreply@aether.test" || msg.Header.Get("Message-Id") == "" {
		t.Fatalf("headers: %v", msg.Header)
	}
}

func TestRefusesAServerWithoutSTARTTLS(t *testing.T) {
	d, pool := startSMTPD(t, false, false)
	s := &SMTP{Addr: d.ln.Addr().String(), Username: "aether", Password: "s3cret", From: "noreply@aether.test", RootCAs: pool}
	err := s.Send(ctx(t), Message{To: "zoe@example.com", Subject: "x", Body: "y"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err = %v, want a STARTTLS refusal", err)
	}
	cmds, mails := d.snapshot()
	for _, c := range cmds {
		if strings.HasPrefix(c, "AUTH") || strings.HasPrefix(c, "MAIL") {
			t.Fatalf("%q crossed the network in the clear", c)
		}
	}
	if len(mails) != 0 {
		t.Fatal("a message was delivered unencrypted")
	}
}

func TestRefusesACertificateItCannotVerify(t *testing.T) {
	d, _ := startSMTPD(t, true, false)
	s := &SMTP{Addr: d.ln.Addr().String(), Username: "aether", Password: "s3cret", From: "noreply@aether.test"} // system roots
	if err := s.Send(ctx(t), Message{To: "zoe@example.com", Subject: "x", Body: "y"}); err == nil {
		t.Fatal("sent to a server presenting an unverifiable certificate")
	}
	if cmds, _ := d.snapshot(); indexOf(cmds, "AUTH") >= 0 {
		t.Fatal("credentials sent to an unverified server")
	}
}

func TestDeliversOverImplicitTLS(t *testing.T) {
	d, pool := startSMTPD(t, false, true)
	s := &SMTP{Addr: d.ln.Addr().String(), From: "noreply@aether.test", ImplicitTLS: true, RootCAs: pool}
	if err := s.Send(ctx(t), Message{To: "zoe@example.com", Subject: "x", Body: "y"}); err != nil {
		t.Fatal(err)
	}
	if _, mails := d.snapshot(); len(mails) != 1 || !mails[0].secure {
		t.Fatalf("%d messages, secure %v", len(mails), len(mails) == 1 && mails[0].secure)
	}
}

func TestAHeaderValueCannotAddHeadersOrRecipients(t *testing.T) {
	d, pool := startSMTPD(t, true, false)
	s := &SMTP{Addr: d.ln.Addr().String(), From: "noreply@aether.test", RootCAs: pool}
	for _, m := range []Message{
		{To: "zoe@example.com\r\nBcc: everyone@example.com", Subject: "x", Body: "y"},
		{To: "zoe@example.com", Subject: "x\r\nBcc: everyone@example.com", Body: "y"},
		{To: "zoe@example.com", Subject: "x\nBcc: everyone@example.com", Body: "y"},
		{To: "Zoe <zoe@example.com>", Subject: "x", Body: "y"},
		{To: "zoe@example.com, everyone@example.com", Subject: "x", Body: "y"},
	} {
		if err := s.Send(ctx(t), m); err == nil {
			t.Errorf("sent %+v", m)
		}
	}
	if cmds, _ := d.snapshot(); len(cmds) != 0 {
		t.Fatalf("a refused message still connected: %v", cmds)
	}
}

func TestAServerThatNeverAnswersCannotHoldTheSender(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // accepted, never greeted
		}
	}()
	s := &SMTP{Addr: ln.Addr().String(), From: "noreply@aether.test"}
	c, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	if err := s.Send(c, Message{To: "zoe@example.com", Subject: "x", Body: "y"}); err == nil {
		t.Fatal("no error from a silent server")
	}
	if el := time.Since(t0); el > 2*time.Second {
		t.Fatalf("held for %v past a 300 ms deadline", el)
	}
}

type blockingSender struct {
	release chan struct{}
	sent    atomic.Int32
}

func (b *blockingSender) Send(ctx context.Context, m Message) error {
	<-b.release
	b.sent.Add(1)
	return nil
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestTheQueueIsBoundedAndNeverBlocksTheCaller(t *testing.T) {
	b := &blockingSender{release: make(chan struct{})}
	q := NewQueue(b, discard)
	accepted := 0
	done := make(chan struct{})
	go func() {
		for i := 0; i < QueueLen+50; i++ {
			if q.Enqueue(Message{To: "a@example.com"}) {
				accepted++
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue blocked behind a stuck mail server")
	}
	// One is in the sender's hands, QueueLen wait; the rest are dropped.
	if accepted > QueueLen+1 || accepted < QueueLen {
		t.Fatalf("accepted %d with a bound of %d", accepted, QueueLen)
	}
	close(b.release)
	q.Close()
	if int(b.sent.Load()) != accepted {
		t.Fatalf("Close returned with %d of %d sent", b.sent.Load(), accepted)
	}
}

func TestQueueClosePaths(t *testing.T) {
	base := runtime.NumGoroutine()
	// Never used, closed twice.
	q := NewQueue(&blockingSender{release: closedChan()}, discard)
	q.Close()
	q.Close()
	// Closed from many goroutines at once, racing writers.
	q = NewQueue(&blockingSender{release: closedChan()}, discard)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); q.Enqueue(Message{To: "a@example.com"}) }()
		go func() { defer wg.Done(); q.Close() }()
	}
	wg.Wait()
	if q.Enqueue(Message{To: "a@example.com"}) {
		t.Fatal("accepted a message after Close")
	}
	for i := 0; i < 100 && runtime.NumGoroutine() > base; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Fatalf("%d goroutines, %d before", n, base)
	}
}

func closedChan() chan struct{} { c := make(chan struct{}); close(c); return c }

func indexOf(cmds []string, prefix string) int {
	for i, c := range cmds {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}
