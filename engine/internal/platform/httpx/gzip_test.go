package httpx

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, h http.Handler, accept string, upgrade bool) *http.Response {
	t.Helper()
	req := httptest.NewRequest("GET", "/x", nil)
	if accept != "" {
		req.Header.Set("Accept-Encoding", accept)
	}
	if upgrade {
		req.Header.Set("Upgrade", "websocket")
	}
	rec := httptest.NewRecorder()
	Gzip(h).ServeHTTP(rec, req)
	return rec.Result()
}

func text(n int) []byte {
	return bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), n)
}

func TestGzipCompressesTextWhenAcceptedAndOnlyThen(t *testing.T) {
	body := text(200) // ~9 KB
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write(body)
	})
	res := serve(t, h, "gzip, deflate, br", false)
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("not compressed: %v", res.Header)
	}
	if res.Header.Get("Vary") != "Accept-Encoding" {
		t.Fatalf("Vary missing: %v", res.Header)
	}
	if res.Header.Get("Content-Length") != "" {
		t.Fatal("a stale Content-Length was left on a compressed response")
	}
	raw, _ := io.ReadAll(res.Body)
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if !bytes.Equal(got, body) {
		t.Fatalf("round trip lost bytes: %d vs %d", len(got), len(body))
	}
	if len(raw) > len(body)/10 {
		t.Fatalf("compression barely helped: %d of %d", len(raw), len(body))
	}
	// Not accepted: untouched.
	res = serve(t, h, "", false)
	if res.Header.Get("Content-Encoding") != "" {
		t.Fatal("compressed for a client that did not ask")
	}
	res = serve(t, h, "identity, gzip;q=0", false)
	if res.Header.Get("Content-Encoding") != "" {
		t.Fatal("compressed for a client that refused gzip")
	}
}

func TestGzipLeavesBinarySmallAndAlreadyEncodedAlone(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"image": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(text(200))
		},
		"small": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "20")
			_, _ = w.Write([]byte(`{"ok":true,"n":1234}`))
		},
		"already": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Encoding", "br")
			_, _ = w.Write(text(50))
		},
		"error": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(text(50))
		},
	}
	for name, h := range cases {
		res := serve(t, h, "gzip", false)
		if enc := res.Header.Get("Content-Encoding"); (name == "already" && enc != "br") || (name != "already" && enc != "") {
			t.Fatalf("%s: Content-Encoding %q", name, enc)
		}
	}
}

func TestGzipStaysOutOfWebSocketUpgrades(t *testing.T) {
	var sawHijacker bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawHijacker = w.(http.Hijacker)
		w.WriteHeader(http.StatusSwitchingProtocols)
	})
	// An upgrade request bypasses the wrapper entirely: the handler sees the
	// server's own writer (the recorder here, which is not a hijacker).
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	Gzip(h).ServeHTTP(rec, req)
	if sawHijacker {
		t.Fatal("the recorder is not a hijacker; the wrapper must not have been in the way")
	}
}

func TestGzipDecidesOnFirstWriteWithoutExplicitHeader(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(strings.Repeat("<p>hello</p>", 200)))
	})
	res := serve(t, h, "gzip", false)
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("an implicit 200 was not compressed")
	}
}

// A small JSON answer with no Content-Length -- the shape every API handler
// here produces -- goes out raw: gzip would make it bigger. A large one is
// compressed, and the bytes written before the decision are not lost.
func TestGzipHoldsBackUnsizedResponsesUntilItKnows(t *testing.T) {
	small := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	res := serve(t, small, "gzip", false)
	if res.Header.Get("Content-Encoding") != "" {
		t.Fatal("a 11-byte JSON body was compressed")
	}
	b, _ := io.ReadAll(res.Body)
	if string(b) != `{"ok":true}` {
		t.Fatalf("body %q", b)
	}
	piece := `{"row":` + strings.Repeat("x", 20) + `},`
	large := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for i := 0; i < 40; i++ {
			_, _ = w.Write([]byte(piece)) // small writes; the hold-back fills part way through
		}
	})
	res = serve(t, large, "gzip", false)
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("a 1.2 KB JSON body written in small pieces was not compressed")
	}
	raw, _ := io.ReadAll(res.Body)
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if len(got) != 40*len(piece) {
		t.Fatalf("round trip: %d bytes, want %d (the held-back prefix must not be lost)", len(got), 40*len(piece))
	}
}
