package messaging

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Files in chats. What these hold: the bytes are reachable only by members
// and only while the message stands; the ceiling is enforced and leaves
// nothing behind; names never reach the disk and cannot break a header;
// executable types never render inline; forwards carry files; uploads under
// contention neither corrupt nor leak.

func fileService(t *testing.T) (*Service, Actor, Actor, string, string) {
	t.Helper()
	s := serviceForTest(t)
	dir := filepath.Join(t.TempDir(), "files")
	fs, err := NewDiskStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.UseFiles(fs)
	me := Actor{ID: "u-me", Email: "me@example.com", Name: "Me"}
	you := Actor{ID: "u-you", Email: "other@example.com", Name: "Other"}
	c, err := s.OpenDirect(context.Background(), me, you.Email)
	if err != nil {
		t.Fatal(err)
	}
	return s, me, you, c.ID, dir
}

func blobsOnDisk(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(d.Name(), ".part-") {
			n++
		}
		return nil
	})
	return n
}

func TestUploadAttachesFilesAndOnlyMembersMayReadThem(t *testing.T) {
	s, me, you, conv, dir := fileService(t)
	ctx := context.Background()

	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 100)...)
	at1, err := s.StageUpload(ctx, me, "shot.png", "image/png", bytes.NewReader(png))
	if err != nil {
		t.Fatal(err)
	}
	at2, err := s.StageUpload(ctx, me, "notes.txt", "text/plain", strings.NewReader("hello file"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.SendStaged(ctx, me, conv, "", "", []*Attachment{at1, at2})
	if err != nil {
		t.Fatalf("send with files and no text: %v", err)
	}
	if len(m.Attachments) != 2 || m.Attachments[0].MIME != "image/png" || m.Attachments[1].MIME != "text/plain" {
		t.Fatalf("attachments on the sent message: %+v", m.Attachments)
	}
	if blobsOnDisk(t, dir) != 2 {
		t.Fatalf("%d blobs on disk, want 2", blobsOnDisk(t, dir))
	}

	// The other member reads them, on the message and by download.
	page, _ := s.Messages(ctx, you, conv, time.Time{}, 10)
	s.Decorate(ctx, you, page)
	if len(page) != 1 || len(page[0].Attachments) != 2 {
		t.Fatalf("history for the other member: %+v", page)
	}
	att, f, _, err := s.OpenAttachment(ctx, you, at2.ID)
	if err != nil {
		t.Fatalf("member download: %v", err)
	}
	got, _ := io.ReadAll(f)
	_ = f.Close()
	if string(got) != "hello file" || att.Name != "notes.txt" {
		t.Fatalf("downloaded %q as %q", got, att.Name)
	}

	// A stranger may not, by id or by guess.
	stranger := Actor{ID: "u-stranger", Email: "x@example.com", Name: "X"}
	if _, _, _, err := s.OpenAttachment(ctx, stranger, at2.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger download: err=%v, want ErrForbidden", err)
	}
	if _, _, _, err := s.OpenAttachment(ctx, me, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: err=%v, want ErrNotFound", err)
	}

	// Deleted for everyone: the files go with the message.
	if err := s.Delete(ctx, me, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.OpenAttachment(ctx, you, at2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("download after delete-for-everyone: err=%v, want ErrNotFound", err)
	}
	shared, _ := s.SharedFiles(ctx, you, conv, 10)
	if len(shared) != 0 {
		t.Fatalf("shared view still lists files of a deleted message: %+v", shared)
	}
}

func TestUploadCeilingLeavesNothingBehind(t *testing.T) {
	s, me, _, _, dir := fileService(t)
	ctx := context.Background()

	// One byte over: refused, and the partial blob is gone.
	over := io.LimitReader(zeroReader{}, MaxAttachmentBytes+1)
	if _, err := s.StageUpload(ctx, me, "big.bin", "application/octet-stream", over); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("one byte over the ceiling: err=%v, want ErrTooLarge", err)
	}
	if n := blobsOnDisk(t, dir); n != 0 {
		t.Fatalf("%d blobs left on disk after a refused upload", n)
	}
	// Exactly at the ceiling: accepted.
	at, err := s.StageUpload(ctx, me, "big.bin", "application/octet-stream", io.LimitReader(zeroReader{}, MaxAttachmentBytes))
	if err != nil {
		t.Fatalf("exactly the ceiling: %v", err)
	}
	if at.Size != MaxAttachmentBytes {
		t.Fatalf("size %d", at.Size)
	}
	s.DiscardStaged([]*Attachment{at})
	if n := blobsOnDisk(t, dir); n != 0 {
		t.Fatalf("%d blobs left after discard", n)
	}
	// Nothing at all is not a file.
	if _, err := s.StageUpload(ctx, me, "empty", "", strings.NewReader("")); !errors.Is(err, ErrEmptyBody) {
		t.Fatalf("empty upload: err=%v, want ErrEmptyBody", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestTooManyFilesInOneMessage(t *testing.T) {
	s, me, _, conv, dir := fileService(t)
	ctx := context.Background()
	var staged []*Attachment
	for i := 0; i <= MaxAttachmentsPerMessage; i++ {
		at, err := s.StageUpload(ctx, me, "f.txt", "text/plain", strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		staged = append(staged, at)
	}
	if _, err := s.SendStaged(ctx, me, conv, "", "", staged); !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("%d files: err=%v, want ErrTooManyFiles", len(staged), err)
	}
	s.DiscardStaged(staged)
	if n := blobsOnDisk(t, dir); n != 0 {
		t.Fatalf("%d blobs left behind", n)
	}
}

func TestFileNamesAreDisplayOnly(t *testing.T) {
	cases := map[string]string{
		`../../etc/passwd`:                "passwd",
		`C:\Users\me\..\..\boot.ini`:      "boot.ini",
		"report\x00.pdf":                  "report.pdf",
		`"; rm -rf /; echo "`:             "; echo", // the slash is a separator
		"   .hidden":                      "hidden",
		"..":                              "file",
		"":                                "file",
		"résumé final (v2).docx":          "résumé final (v2).docx",
		strings.Repeat("a", 500) + ".txt": strings.Repeat("a", maxNameRunes),
	}
	for in, want := range cases {
		if got := SanitizeFileName(in); got != want {
			t.Errorf("SanitizeFileName(%q) = %q, want %q", in, got, want)
		}
	}
	// And the header built from a hostile name has no way out of its quotes.
	h := ContentDisposition(false, SanitizeFileName("a\"b\r\nX-Injected: yes.txt"))
	if strings.ContainsAny(h, "\r\n") || strings.Count(h, `"`) != 2 {
		t.Fatalf("header from a hostile name: %q", h)
	}
}

func TestExecutableTypesNeverRenderInline(t *testing.T) {
	html := []byte("<!doctype html><html><script>alert(document.cookie)</script></html>")
	if got := DetectMIME(html, "image/png"); got != "text/html" {
		t.Fatalf("HTML declared as PNG sniffed as %q, want text/html", got)
	}
	if ct, inline := ServeType("text/html"); inline || ct != "application/octet-stream" {
		t.Fatalf("HTML would be served as %q inline=%v", ct, inline)
	}
	if ct, inline := ServeType("image/svg+xml"); inline || ct != "application/octet-stream" {
		t.Fatalf("SVG would be served as %q inline=%v", ct, inline)
	}
	if _, inline := ServeType("image/png"); !inline {
		t.Fatal("a PNG should render inline")
	}
	// A PDF the sniffer sees as octet-stream keeps its declared type; a
	// declared type outside the short list does not survive a blind sniff.
	if got := DetectMIME([]byte{0, 1, 2, 3}, "application/pdf"); got != "application/pdf" {
		t.Fatalf("declared pdf: %q", got)
	}
	if got := DetectMIME([]byte{0, 1, 2, 3}, "text/html"); got != "application/octet-stream" {
		t.Fatalf("declared html over unknown bytes: %q", got)
	}
}

func TestDownloadHeadersOverHTTP(t *testing.T) {
	s, me, _, conv, _ := fileService(t)
	ctx := context.Background()
	at, _ := s.StageUpload(ctx, me, "evil.html", "text/html", strings.NewReader("<script>1</script>"))
	img, _ := s.StageUpload(ctx, me, "ok.png", "image/png", bytes.NewReader(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 20)...)))
	if _, err := s.SendStaged(ctx, me, conv, "", "", []*Attachment{at, img}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(s, s.hub, stubGuard{me}, quietLog())
	mux := http.NewServeMux()
	h.Routes(mux)

	get := func(id string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/messaging/files/"+id, nil))
		return rec
	}
	rec := get(at.ID)
	if rec.Code != 200 {
		t.Fatalf("download status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("an HTML upload served as %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Fatalf("Content-Disposition %q, want attachment", cd)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
	rec = get(img.ID)
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("PNG served as %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "inline;") {
		t.Fatalf("PNG Content-Disposition %q", cd)
	}
	// Range requests work, because a video will want them.
	req := httptest.NewRequest("GET", "/api/messaging/files/"+img.ID, nil)
	req.Header.Set("Range", "bytes=0-3")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.Len() != 4 {
		t.Fatalf("range: %d, %d bytes", rec.Code, rec.Body.Len())
	}
}

// stubGuard lets every request through as one actor. The handler tests here
// are about headers and bodies, not about authentication, which has its own.
type stubGuard struct{ a Actor }

func (g stubGuard) Protect(_ string, next http.Handler) http.Handler { return next }
func (g stubGuard) ActorOf(*http.Request) (string, string)           { return g.a.ID, g.a.Email }
func (g stubGuard) NameOf(*http.Request) string                      { return g.a.Name }

func TestForwardCarriesFiles(t *testing.T) {
	s, me, you, conv, dir := fileService(t)
	ctx := context.Background()
	at, _ := s.StageUpload(ctx, you, "deck.pdf", "application/pdf", strings.NewReader("%PDF-1.4 fake"))
	src, err := s.SendStaged(ctx, you, conv, "", "", []*Attachment{at})
	if err != nil {
		t.Fatal(err)
	}
	notes, _ := s.OpenSelf(ctx, me)
	out, err := s.Forward(ctx, me, src.ID, []string{notes.ID})
	if err != nil {
		t.Fatalf("forwarding a files-only message: %v", err)
	}
	if len(out) != 1 || len(out[0].Attachments) != 1 || out[0].Attachments[0].Name != "deck.pdf" {
		t.Fatalf("forwarded: %+v", out)
	}
	if out[0].Attachments[0].ID == at.ID {
		t.Fatal("the forward must get its own attachment row")
	}
	if blobsOnDisk(t, dir) != 1 {
		t.Fatalf("a forward must share the bytes, not copy them: %d blobs", blobsOnDisk(t, dir))
	}
	// Readable by the forwarder in the target; the source's deletion does
	// not reach the copy (it is a new message).
	if _, f, _, err := s.OpenAttachment(ctx, me, out[0].Attachments[0].ID); err != nil {
		t.Fatalf("download of the forwarded file: %v", err)
	} else {
		_ = f.Close()
	}
}

func TestSharedFilesAndLinks(t *testing.T) {
	s, me, you, conv, _ := fileService(t)
	ctx := context.Background()
	for i, name := range []string{"a.txt", "b.txt", "c.txt"} {
		at, _ := s.StageUpload(ctx, me, name, "text/plain", strings.NewReader("x"))
		if _, err := s.SendStaged(ctx, me, conv, "", "", []*Attachment{at}); err != nil {
			t.Fatal(err)
		}
		_ = i
	}
	s.Send(ctx, you, conv, "see https://example.com/spec and http://intranet/wiki, again https://example.com/spec.")
	s.Send(ctx, you, conv, "no links here")
	link, _ := s.Send(ctx, me, conv, "https://deleted.example/x")
	_ = s.Delete(ctx, me, link.ID)

	files, err := s.SharedFiles(ctx, you, conv, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || files[0].Name != "c.txt" || files[2].Name != "a.txt" || files[0].SenderName != "Me" {
		t.Fatalf("shared files: %+v", files)
	}
	links, err := s.SharedLinks(ctx, me, conv, 10)
	if err != nil {
		t.Fatal(err)
	}
	urls := []string{}
	for _, l := range links {
		urls = append(urls, l.URL)
	}
	want := "[https://example.com/spec http://intranet/wiki]"
	if got := strings.Join([]string{"[" + strings.Join(urls, " ") + "]"}, ""); got != want {
		t.Fatalf("shared links %v, want %s (deduplicated, trailing punctuation trimmed, deleted excluded)", got, want)
	}
	// A stranger sees neither.
	stranger := Actor{ID: "u-stranger", Email: "x@example.com", Name: "X"}
	if _, err := s.SharedFiles(ctx, stranger, conv, 10); !errors.Is(err, ErrForbidden) {
		t.Fatalf("stranger shared files: %v", err)
	}
}

func TestExtractLinks(t *testing.T) {
	got := ExtractLinks("go to https://a.example/x?y=1&z=2). Then http://b.example, and https://a.example/x?y=1&z=2 again; not ftp://c")
	want := []string{"https://a.example/x?y=1&z=2", "http://b.example"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("ExtractLinks = %v, want %v", got, want)
	}
	if ExtractLinks("plain text") != nil {
		t.Fatal("no links expected")
	}
}

func TestConcurrentUploadsUnderRace(t *testing.T) {
	s, me, you, conv, dir := fileService(t)
	ctx := context.Background()
	const N = 24
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			who := me
			if i%2 == 1 {
				who = you
			}
			at, err := s.StageUpload(ctx, who, "f.txt", "text/plain", strings.NewReader(strings.Repeat("y", 1000+i)))
			if err != nil {
				errs <- err
				return
			}
			if _, err := s.SendStaged(ctx, who, conv, "", "", []*Attachment{at}); err != nil {
				s.DiscardStaged([]*Attachment{at})
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent upload: %v", err)
	}
	if n := blobsOnDisk(t, dir); n != N {
		t.Fatalf("%d blobs on disk, want %d", n, N)
	}
	files, _ := s.SharedFiles(ctx, me, conv, 100)
	if len(files) != N {
		t.Fatalf("%d shared files, want %d", len(files), N)
	}
	sizes := map[int64]bool{}
	for _, f := range files {
		sizes[f.Size] = true
	}
	if len(sizes) != N {
		t.Fatalf("sizes collapsed under contention: %d distinct of %d", len(sizes), N)
	}
}
