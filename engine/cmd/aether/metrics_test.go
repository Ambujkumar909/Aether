package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/aether-rtc/aether/internal/ledger"
	"github.com/aether-rtc/aether/internal/platform/database"
	"github.com/aether-rtc/aether/internal/platform/httpx"
	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/sfu"
	"github.com/aether-rtc/aether/internal/signal"
)

var (
	metricName = `[a-zA-Z_:][a-zA-Z0-9_:]*`
	labelPair  = `[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\\n]|\\[\\"n])*"`
	sampleLine = regexp.MustCompile(`^(` + metricName + `)(?:\{(` + labelPair + `(?:,` + labelPair + `)*)\})? (\S+)$`)
	helpLine   = regexp.MustCompile(`^# HELP (` + metricName + `) .+$`)
	typeLine   = regexp.MustCompile(`^# TYPE (` + metricName + `) (gauge|counter)$`)
)

// parseExposition checks text against the Prometheus text format strictly and
// returns each sample line keyed by name{labels}.
func parseExposition(t *testing.T, text string) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	help, typ := map[string]bool{}, map[string]bool{}
	current := ""
	for i, ln := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		switch {
		case helpLine.MatchString(ln):
			n := helpLine.FindStringSubmatch(ln)[1]
			if help[n] {
				t.Errorf("line %d: second HELP for %s", i+1, n)
			}
			help[n] = true
		case typeLine.MatchString(ln):
			n := typeLine.FindStringSubmatch(ln)[1]
			if typ[n] || !help[n] {
				t.Errorf("line %d: TYPE for %s repeated or before its HELP", i+1, n)
			}
			typ[n], current = true, n
		case sampleLine.MatchString(ln):
			m := sampleLine.FindStringSubmatch(ln)
			if m[1] != current {
				t.Errorf("line %d: sample %s outside its family (%s)", i+1, m[1], current)
			}
			v, err := strconv.ParseFloat(m[3], 64)
			if err != nil {
				t.Errorf("line %d: value %q: %v", i+1, m[3], err)
			}
			out[m[1]+"{"+m[2]+"}"] = v
		default:
			t.Errorf("line %d is not Prometheus text format: %q", i+1, ln)
		}
	}
	return out
}

func newMetricsDeps(t *testing.T) metricsDeps {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	root := ledger.NewMeter("node")
	return metricsDeps{
		root:  root,
		rooms: room.NewRegistry(root),
		media: sfu.NewRegistry(root, slog.New(slog.NewTextHandler(io.Discard, nil)), nil),
		gw:    &signal.Gateway{},
		db:    db.DB,
	}
}

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	res, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("status %d, content type %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func TestMetricsAreValidPrometheusText(t *testing.T) {
	d := newMetricsDeps(t)
	d.rooms.GetOrCreate("board-review-7f3a") // a meeting in progress
	// A response through the real logging middleware is counted by code.
	teapot := httptest.NewServer(httpx.Logging(slog.New(slog.NewTextHandler(io.Discard, nil)),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })))
	res, err := http.Get(teapot.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	teapot.Close()

	text := scrape(t, metricsHandler(d))
	got := parseExposition(t, text)
	for k, want := range map[string]float64{
		"aether_rooms{}":                          1,
		"aether_participants{}":                   0,
		"aether_db_up{}":                          1,
		"aether_media_peers{}":                    0,
		"aether_signal_connections{}":             0,
		`aether_http_responses_total{code="418"}`: 1,
		"aether_backup_failures_total{}":          0,
	} {
		if v, ok := got[k]; !ok || v != want {
			t.Errorf("%s = %v (present %v), want %v", k, v, ok, want)
		}
	}
	if !strings.Contains(text, `aether_build_info{version="`) {
		t.Error("no build info")
	}
	// Aggregates only: nothing that names a meeting or a person.
	if strings.Contains(text, "board-review") {
		t.Errorf("a room id reached the metrics:\n%s", text)
	}
}

func TestALabelValueCannotBreakOutOfItsQuotes(t *testing.T) {
	saved := version
	version = "1.0\"} 1\nfake_metric 99\n\\"
	defer func() { version = saved }()
	got := parseExposition(t, scrape(t, metricsHandler(newMetricsDeps(t))))
	if _, ok := got["fake_metric{}"]; ok {
		t.Fatal("a crafted version string injected a metric")
	}
}

func TestADatabaseThatStopsAnsweringReadsDown(t *testing.T) {
	d := newMetricsDeps(t)
	d.db.Close()
	got := parseExposition(t, scrape(t, metricsHandler(d)))
	if got["aether_db_up{}"] != 0 {
		t.Fatalf("aether_db_up = %v with the database closed", got["aether_db_up{}"])
	}
}
