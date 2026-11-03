package room

import (
	"sort"
	"testing"

	"github.com/aether-rtc/aether/internal/ledger"
)

func ids(ps []*Participant) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.ID)
	}
	sort.Strings(out)
	return out
}

// A camera goes to those who have it on screen, or who never said what they
// show; a publisher coming into view is reported (for a key frame); a report
// that names nobody in the room changes nothing.
func TestSocketViews(t *testing.T) {
	r := New("v", ledger.NewMeter("t"))
	for _, id := range []string{"a", "b", "c", "d"} {
		if _, err := r.Join(&Participant{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if got := ids(r.CameraAudience("a")); len(got) != 3 {
		t.Fatalf("nobody has reported: a goes to everyone else, got %v", got)
	}
	// b shows c only; a's camera is off b's screen.
	if became := r.SetSocketView("b", map[string]bool{"a": false, "c": true, "track-xyz": true}); len(became) != 1 || became[0] != "c" {
		t.Fatalf("came into view: %v", became)
	}
	if got := ids(r.CameraAudience("a")); len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Fatalf("a's camera with b not showing it: %v", got)
	}
	if got := ids(r.CameraAudience("c")); len(got) != 3 {
		t.Fatalf("c's camera: %v", got)
	}
	// The same report again: nothing new came into view.
	if became := r.SetSocketView("b", map[string]bool{"a": false, "c": true}); len(became) != 0 {
		t.Fatalf("nothing changed, but %v", became)
	}
	// a scrolls into b's view.
	if became := r.SetSocketView("b", map[string]bool{"a": true, "c": true}); len(became) != 1 || became[0] != "a" {
		t.Fatalf("a came into view: %v", became)
	}
	// A report naming only tracks (a WebRTC client) is not a socket report.
	if became := r.SetSocketView("d", map[string]bool{"track-1": true}); became != nil {
		t.Fatalf("tracks only: %v", became)
	}
	if got := ids(r.CameraAudience("a")); len(got) != 3 {
		t.Fatalf("d said nothing about people, so still gets everything: %v", got)
	}
	// Somebody who is not here reports nothing; a viewer cannot list itself.
	if became := r.SetSocketView("zz", map[string]bool{"a": true}); became != nil {
		t.Fatal("a stranger's report")
	}
	if became := r.SetSocketView("c", map[string]bool{"c": true}); became != nil {
		t.Fatal("itself")
	}
}
