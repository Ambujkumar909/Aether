package httpx

import (
	"bufio"
	"compress/gzip"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Gzip compresses responses that are text -- HTML, JavaScript, CSS, JSON,
// SVG -- for clients that accept it. Measured before it existed: one page
// load of the client fetched 700 KB of script and stylesheet uncompressed,
// with Cache-Control: no-store on top, so every load paid it again.
//
// It deliberately does not touch:
//   - anything that is not text (images, Opus, video, the wire protocol
//     over WebSocket): already compressed, and deflating it expands it;
//   - WebSocket upgrades: the connection is handed to the socket code,
//     which needs the raw writer (Hijack);
//   - responses under gzipMinBytes: the gzip header and the CPU cost more
//     than they save;
//   - responses that already carry a Content-Encoding.
//
// The decision is made when the handler writes its headers, because that is
// when the Content-Type is known; until then nothing is committed.
const gzipMinBytes = 512

var compressible = []string{
	"text/", "application/json", "application/javascript", "application/x-javascript",
	"application/manifest+json", "image/svg+xml", "application/xml", "application/wasm",
}

var gzipPool = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
	return w
}}

func Gzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

func acceptsGzip(v string) bool {
	for _, part := range strings.Split(v, ",") {
		enc := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if strings.EqualFold(enc, "gzip") {
			return !strings.Contains(part, "q=0,") && !strings.HasSuffix(strings.TrimSpace(part), "q=0")
		}
	}
	return false
}

func isCompressible(ct string) bool {
	ct = strings.ToLower(ct)
	for _, p := range compressible {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	return false
}

type gzipWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
	status  int
	// A candidate response with no Content-Length is held back up to
	// gzipMinBytes: a 40-byte JSON answer must not go out as 60 bytes of
	// gzip. Once the hold-back fills, or the handler returns, the choice is
	// made and the held bytes go out first.
	held    []byte
	holding bool
}

func (g *gzipWriter) decide(status int) {
	if g.decided {
		return
	}
	g.decided = true
	g.status = status
	h := g.Header()
	if status != http.StatusOK || h.Get("Content-Encoding") != "" || !isCompressible(h.Get("Content-Type")) {
		return
	}
	if cl := h.Get("Content-Length"); cl != "" {
		if n, err := strconv.Atoi(cl); err == nil && n < gzipMinBytes {
			return
		}
		g.startGzip()
		return
	}
	g.holding = true
}

func (g *gzipWriter) startGzip() {
	h := g.Header()
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	g.gz = gzipPool.Get().(*gzip.Writer)
	g.gz.Reset(g.ResponseWriter)
}

// settle ends a hold-back: compress if enough was written, else pass raw.
func (g *gzipWriter) settle(compress bool) error {
	g.holding = false
	if compress {
		g.startGzip()
	}
	g.ResponseWriter.WriteHeader(g.status)
	var err error
	if len(g.held) > 0 {
		if g.gz != nil {
			_, err = g.gz.Write(g.held)
		} else {
			_, err = g.ResponseWriter.Write(g.held)
		}
	}
	g.held = nil
	return err
}

func (g *gzipWriter) WriteHeader(status int) {
	g.decide(status)
	if !g.holding {
		g.ResponseWriter.WriteHeader(status)
	}
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.decided {
		g.decide(http.StatusOK)
	}
	if g.holding {
		g.held = append(g.held, b...)
		if len(g.held) < gzipMinBytes {
			return len(b), nil
		}
		if err := g.settle(true); err != nil {
			return 0, err
		}
		return len(b), nil
	}
	if g.gz != nil {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipWriter) Flush() {
	if g.holding {
		// A handler that flushes is streaming; commit to what is known.
		_ = g.settle(len(g.held) >= gzipMinBytes)
	}
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack hands the connection over untouched; nothing compressed has been
// written when a handler hijacks, because upgrades never reach the gzip path.
func (g *gzipWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := g.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (g *gzipWriter) close() {
	if g.holding {
		_ = g.settle(false) // the whole response fit under the line: raw
	}
	if g.gz != nil {
		_ = g.gz.Close()
		g.gz.Reset(nil)
		gzipPool.Put(g.gz)
		g.gz = nil
	}
}
