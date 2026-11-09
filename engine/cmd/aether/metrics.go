package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/modules/identity"
	"github.com/aether-rtc/aether/internal/platform/httpx"
	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/sfu"
	"github.com/aether-rtc/aether/internal/signal"
)

// Prometheus metrics, in the text exposition format, on a listener of their
// own (-metrics-addr): never on the public port, and no room, person or
// address in any label -- aggregates only, so the label set stays bounded
// however many meetings run.

// Set by the scheduled backup loop.
var (
	backupLastOK   atomic.Int64 // unix seconds of the last good snapshot
	backupFailures atomic.Uint64
)

type sample struct {
	labels string // already rendered: k="v",k2="v2"
	value  float64
}

type family struct {
	name, help, typ string
	samples         []sample
}

func gauge(name, help string, v float64) family {
	return family{name, help, "gauge", []sample{{"", v}}}
}

func counter(name, help string, v float64) family {
	return family{name, help, "counter", []sample{{"", v}}}
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func label(k, v string) string { return k + `="` + labelEscaper.Replace(v) + `"` }

func writeMetrics(w io.Writer, fams []family) {
	for _, f := range fams {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.typ)
		for _, s := range f.samples {
			v := strconv.FormatFloat(s.value, 'g', -1, 64)
			if s.labels == "" {
				fmt.Fprintf(w, "%s %s\n", f.name, v)
			} else {
				fmt.Fprintf(w, "%s{%s} %s\n", f.name, s.labels, v)
			}
		}
	}
}

type metricsDeps struct {
	root  *ledger.Meter
	rooms *room.Registry
	media *sfu.Registry
	gw    *signal.Gateway
	db    *sql.DB
}

func gatherMetrics(ctx context.Context, d metricsDeps) []family {
	snap := d.root.Snapshot()
	fams := []family{
		{"aether_build_info", "The running build.", "gauge",
			[]sample{{label("version", version) + "," + label("goversion", runtime.Version()), 1}}},
		gauge("aether_uptime_seconds", "Seconds since the server started.", snap.Elapsed.Seconds()),
		gauge("aether_signal_connections", "Open meeting sockets.", float64(d.gw.Connections())),
	}

	rs := d.rooms.All()
	people := 0
	for _, r := range rs {
		people += r.Size()
	}
	peers, tracks := 0, 0
	for _, rt := range d.media.All() {
		p, t := rt.Counts()
		peers += p
		tracks += t
	}
	fams = append(fams,
		gauge("aether_rooms", "Meetings in progress.", float64(len(rs))),
		gauge("aether_participants", "People in meetings.", float64(people)),
		gauge("aether_media_peers", "Participants connected to the media server.", float64(peers)),
		gauge("aether_media_tracks", "Tracks being published.", float64(tracks)),
	)

	wire := family{name: "aether_wire_bytes_total", help: "Bytes on the wire by traffic class, framing included.", typ: "counter"}
	for i, b := range snap.Bytes {
		if b > 0 {
			wire.samples = append(wire.samples, sample{label("class", ledger.Class(i).String()), float64(b)})
		}
	}
	fams = append(fams, wire,
		gauge("aether_wire_efficiency_ratio", "Useful bytes over total bytes on the wire.", snap.Efficiency()))

	su := family{name: "aether_signups_total", help: "Self sign-ups by outcome (created: without verification; pending: link sent; completed: link used; existing, blocked, no_mail: refused, same answer to the caller).", typ: "counter"}
	for _, k := range []string{"created", "pending", "completed", "existing", "blocked", "no_mail"} {
		su.samples = append(su.samples, sample{label("outcome", k), float64(identity.SignupTotals()[k])})
	}
	fams = append(fams, su)

	codes := httpx.ResponseCounts()
	keys := make([]int, 0, len(codes))
	for c := range codes {
		keys = append(keys, c)
	}
	slices.Sort(keys)
	resp := family{name: "aether_http_responses_total", help: "HTTP responses by status code.", typ: "counter"}
	for _, c := range keys {
		resp.samples = append(resp.samples, sample{label("code", strconv.Itoa(c)), float64(codes[c])})
	}
	fams = append(fams, resp)

	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	up := 1.0
	if d.db.PingContext(pctx) != nil {
		up = 0
	}
	ds := d.db.Stats()
	fams = append(fams,
		gauge("aether_db_up", "1 if the database answered a ping.", up),
		gauge("aether_db_open_connections", "Open database connections.", float64(ds.OpenConnections)),
		counter("aether_db_wait_total", "Times a query waited for a free connection.", float64(ds.WaitCount)),
		gauge("aether_backup_last_success_timestamp_seconds", "When the last scheduled backup succeeded (unix time; 0: none yet).", float64(backupLastOK.Load())),
		counter("aether_backup_failures_total", "Scheduled backups that failed.", float64(backupFailures.Load())),
	)

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	threads, _ := runtime.ThreadCreateProfile(nil)
	fams = append(fams,
		gauge("go_goroutines", "Goroutines.", float64(runtime.NumGoroutine())),
		gauge("go_threads", "OS threads created.", float64(threads)),
		gauge("go_memstats_heap_alloc_bytes", "Heap bytes in use.", float64(ms.HeapAlloc)),
		gauge("go_memstats_sys_bytes", "Bytes obtained from the OS.", float64(ms.Sys)),
	)
	return fams
}

func metricsHandler(d metricsDeps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		writeMetrics(w, gatherMetrics(r.Context(), d))
	})
	return mux
}
