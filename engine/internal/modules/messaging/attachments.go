package messaging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Files in chats.
//
// An attachment is bytes on disk plus a row that says which message they
// belong to. The bytes are keyed by a server-generated id and nothing else --
// the name the sender gave the file is stored for display and never touches
// the filesystem, so there is no path to traverse. The type is sniffed from
// the bytes, not read from the request, and on the way back out anything
// that a browser could execute is served as an opaque download: an uploaded
// HTML file that rendered inline on this origin would run with this origin's
// session, which is the whole ballgame.
//
// Reading a file is reading the message: same membership check, and a
// message deleted for everyone takes its files with it.

const (
	// MaxAttachmentBytes is the ceiling for one file. Twenty-five megabytes
	// covers a slide deck or a minute of screen video; anything larger is a
	// transfer, not a message, and belongs in a system built for transfers.
	MaxAttachmentBytes = 25 << 20
	// MaxAttachmentsPerMessage bounds one send.
	MaxAttachmentsPerMessage = 10
	// maxNameRunes bounds the display name of a file.
	maxNameRunes = 160
)

var (
	ErrTooLarge     = errors.New("messaging: file is too large")
	ErrTooManyFiles = errors.New("messaging: too many files in one message")
	ErrNoFiles      = errors.New("messaging: file storage is not configured")
)

// Attachment is one file on one message.
type Attachment struct {
	ID             string    `json:"id"`
	MessageID      string    `json:"messageId,omitempty"`
	ConversationID string    `json:"-"`
	UploaderID     string    `json:"-"`
	Name           string    `json:"name"`
	MIME           string    `json:"mime"`
	Size           int64     `json:"size"`
	Blob           string    `json:"-"`
	CreatedAt      time.Time `json:"createdAt"`
	// SenderName is filled for the shared-files view, where the file is
	// listed away from its message.
	SenderName string `json:"senderName,omitempty"`
}

// Link is a URL somebody posted, for the shared view.
type Link struct {
	URL        string    `json:"url"`
	MessageID  string    `json:"messageId"`
	SenderName string    `json:"senderName"`
	At         time.Time `json:"at"`
}

// FileStore is where the bytes live.
type FileStore interface {
	// Put streams r into the blob named key and returns the byte count.
	Put(key string, r io.Reader) (int64, error)
	Open(key string) (io.ReadSeekCloser, time.Time, error)
	Remove(key string) error
}

// DiskStore keeps blobs under one directory, two levels deep so a busy
// deployment does not put a million files in one folder.
type DiskStore struct{ root string }

func NewDiskStore(root string) (*DiskStore, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("file store: %w", err)
	}
	return &DiskStore{root: root}, nil
}

var blobKey = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

func (d *DiskStore) path(key string) (string, error) {
	// Keys are generated here, never by a client. The check stays because a
	// bug elsewhere must not become a path traversal.
	if !blobKey.MatchString(key) {
		return "", ErrNotFound
	}
	return filepath.Join(d.root, key[:2], key), nil
}

func (d *DiskStore) Put(key string, r io.Reader) (int64, error) {
	p, err := d.path(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return 0, err
	}
	// Written to a temporary name and renamed: a reader never sees half a
	// file, and a crash mid-upload leaves nothing that looks complete.
	tmp, err := os.CreateTemp(filepath.Dir(p), ".part-*")
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return 0, err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		_ = os.Remove(tmp.Name())
		return 0, err
	}
	return n, nil
}

func (d *DiskStore) Open(key string) (io.ReadSeekCloser, time.Time, error) {
	p, err := d.path(key)
	if err != nil {
		return nil, time.Time{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, time.Time{}, ErrNotFound
		}
		return nil, time.Time{}, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, time.Time{}, err
	}
	return f, st.ModTime(), nil
}

func (d *DiskStore) Remove(key string) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// Names and types
// ---------------------------------------------------------------------------

// SanitizeFileName makes a display name out of whatever the client sent.
//
// Both separators are stripped (a Windows client sends backslashes), then
// control characters and the quote characters that would let a name break
// out of a Content-Disposition header. What is left is bounded. It is only
// ever a display name and a download name; it never names anything on disk.
func SanitizeFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f || r == '"' || r == '\'' || r == unicode.ReplacementChar:
			continue
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimLeft(out, ". ") // no hidden files, no bare "..", no leading dots at all
	if rs := []rune(out); len(rs) > maxNameRunes {
		out = string(rs[:maxNameRunes])
	}
	if out == "" {
		return "file"
	}
	return out
}

// declaredOK lists client-declared types that the sniffer cannot recognise
// (it sees a zip, or plain text) and that are safe to record as declared.
var declaredOK = map[string]bool{
	"application/pdf":               true,
	"application/zip":               true,
	"application/json":              true,
	"text/csv":                      true,
	"application/msword":            true,
	"application/vnd.ms-excel":      true,
	"application/vnd.ms-powerpoint": true,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         true,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
}

// DetectMIME decides what a file is from its first bytes, falling back to
// the declared type only where the sniffer is blind and the declaration is
// one of a short list of document types. The declared type never wins over a
// sniff that says "this is HTML" or "this is an image".
func DetectMIME(head []byte, declared string) string {
	sniffed := http.DetectContentType(head)
	if i := strings.Index(sniffed, ";"); i >= 0 {
		sniffed = strings.TrimSpace(sniffed[:i])
	}
	declared = strings.ToLower(strings.TrimSpace(declared))
	if i := strings.Index(declared, ";"); i >= 0 {
		declared = strings.TrimSpace(declared[:i])
	}
	if (sniffed == "application/octet-stream" || sniffed == "text/plain" || sniffed == "application/zip") &&
		declaredOK[declared] {
		return declared
	}
	return sniffed
}

// inlineOK is what a browser may render in place. Everything else is served
// as an opaque download, whatever it claims to be. SVG is not here on
// purpose: it is XML that can carry script.
var inlineOK = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
	"application/pdf": true,
	"video/mp4":       true, "video/webm": true,
	"audio/mpeg": true, "audio/ogg": true, "audio/wav": true, "audio/webm": true,
	"text/plain": true,
}

// ServeType says how to send a stored type back: the content type to put on
// the wire, and whether it may render inline.
func ServeType(stored string) (contentType string, inline bool) {
	if inlineOK[stored] {
		if stored == "text/plain" {
			return "text/plain; charset=utf-8", true
		}
		return stored, true
	}
	return "application/octet-stream", false
}

// ContentDisposition builds the header for a download name. The ASCII
// fallback and the RFC 5987 form are both given, so a name in any script
// survives every browser.
func ContentDisposition(inline bool, name string) string {
	kind := "attachment"
	if inline {
		kind = "inline"
	}
	ascii := strings.Map(func(r rune) rune {
		if r > 0x7e || r < 0x20 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return mime.FormatMediaType(kind, map[string]string{"filename": ascii}) +
		"; filename*=UTF-8''" + escapeRFC5987(name)
}

func escapeRFC5987(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// UseFiles gives the service somewhere to keep bytes. Without it, uploads
// are refused with ErrNoFiles and everything else works.
func (s *Service) UseFiles(fs FileStore) { s.files = fs }

// CanPost is the membership check an upload needs BEFORE it accepts bytes: a
// non-member must not be able to fill the disk and only then be told no.
func (s *Service) CanPost(ctx context.Context, a Actor, convID string) error {
	return s.mustBeMember(ctx, convID, a.ID)
}

// StageUpload stores one file's bytes and returns the row to attach, not yet
// written. Refuses anything over the ceiling and leaves nothing behind when
// it does.
func (s *Service) StageUpload(ctx context.Context, a Actor, name, declaredMIME string, r io.Reader) (*Attachment, error) {
	if s.files == nil {
		return nil, ErrNoFiles
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	head = head[:n]
	if n == 0 {
		return nil, ErrEmptyBody
	}

	key := newID() + newID() // 32 url-safe chars; two ids so a blob key is never a message id
	// One byte past the ceiling is enough to know, and cheap to know early.
	limited := io.LimitReader(io.MultiReader(strings.NewReader(string(head)), r), MaxAttachmentBytes+1)
	sum := sha256.New()
	size, err := s.files.Put(key, io.TeeReader(limited, sum))
	if err != nil {
		_ = s.files.Remove(key)
		return nil, err
	}
	if size > MaxAttachmentBytes {
		_ = s.files.Remove(key)
		return nil, ErrTooLarge
	}
	_ = hex.EncodeToString(sum.Sum(nil)) // recorded when a dedupe pass wants it; not stored yet
	return &Attachment{
		ID:         newID(),
		UploaderID: a.ID,
		Name:       SanitizeFileName(name),
		MIME:       DetectMIME(head, declaredMIME),
		Size:       size,
		Blob:       key,
		CreatedAt:  time.Now().UTC(),
	}, nil
}

// DiscardStaged removes bytes for uploads that never became a message.
func (s *Service) DiscardStaged(staged []*Attachment) {
	if s.files == nil {
		return
	}
	for _, a := range staged {
		if a != nil && a.Blob != "" {
			_ = s.files.Remove(a.Blob)
		}
	}
}

// SendStaged posts one message carrying the staged files. The body may be
// empty when there is at least one file; a message with neither is nothing.
func (s *Service) SendStaged(ctx context.Context, a Actor, convID, body, replyTo string, staged []*Attachment) (*Message, error) {
	if len(staged) == 0 {
		return s.SendReply(ctx, a, convID, body, replyTo)
	}
	return s.SendMessage(ctx, a, convID, body, replyTo, staged, nil)
}

// OpenAttachment hands back a file for streaming, after the same checks a
// read of its message would pass.
func (s *Service) OpenAttachment(ctx context.Context, a Actor, id string) (*Attachment, io.ReadSeekCloser, time.Time, error) {
	if s.files == nil {
		return nil, nil, time.Time{}, ErrNotFound
	}
	att, err := s.repo.AttachmentByID(ctx, id)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	m, err := s.repo.MessageByID(ctx, att.MessageID)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	if m.Deleted {
		return nil, nil, time.Time{}, ErrNotFound
	}
	if err := s.mustBeMember(ctx, att.ConversationID, a.ID); err != nil {
		return nil, nil, time.Time{}, err
	}
	f, mod, err := s.files.Open(att.Blob)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	return att, f, mod, nil
}

// SharedFiles lists a conversation's files, newest first.
func (s *Service) SharedFiles(ctx context.Context, a Actor, convID string, limit int) ([]Attachment, error) {
	if err := s.mustBeMember(ctx, convID, a.ID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	return s.repo.SharedFiles(ctx, convID, limit)
}

// urlRE finds links in prose. Deliberately plain: a scheme and a run of
// URL characters, trailing punctuation trimmed. The client renders it as
// text with an href; nothing here is trusted beyond that.
var urlRE = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `]+`)

// ExtractLinks pulls the URLs out of one body, in order, deduplicated.
func ExtractLinks(body string) []string {
	if !strings.Contains(body, "://") {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, u := range urlRE.FindAllString(body, -1) {
		u = strings.TrimRight(u, ".,;:!?)]}")
		if len(u) > 2048 || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// SharedLinks lists the URLs posted in a conversation, newest first, from
// the most recent messages. Computed on read: links are rare and the scan
// is bounded, which beats a second table that has to stay in step with
// edits and deletes.
func (s *Service) SharedLinks(ctx context.Context, a Actor, convID string, limit int) ([]Link, error) {
	if err := s.mustBeMember(ctx, convID, a.ID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	msgs, err := s.repo.ListMessages(ctx, convID, time.Now().UTC().Add(time.Minute), 500)
	if err != nil {
		return nil, err
	}
	msgs = s.withoutHidden(ctx, a, msgs)
	out := make([]Link, 0, 16)
	for _, m := range msgs { // newest first already
		if m.Deleted {
			continue
		}
		for _, u := range ExtractLinks(m.Body) {
			out = append(out, Link{URL: u, MessageID: m.ID, SenderName: m.SenderName, At: m.CreatedAt})
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// decorateAttachments fills Attachments on a page of messages.
func (s *Service) decorateAttachments(ctx context.Context, msgs []*Message, ids []string) {
	atts, err := s.repo.AttachmentsFor(ctx, ids)
	if err != nil {
		s.log.Warn("could not read attachments", "err", err)
		return
	}
	for _, m := range msgs {
		m.Attachments = atts[m.ID]
	}
}
