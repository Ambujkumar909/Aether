package signal

import (
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aether-rtc/aether/internal/room"
	"github.com/aether-rtc/aether/internal/wire"
)

// Voice selection on the socket path, through the real dispatcher and real
// sockets: with more microphones open than the room relays, a listener is
// sent the voices that are heard and nothing of the others; the frame
// before the one that won a place arrives in front of it; a camera is not
// a voice and is relayed regardless; nobody is sent their own.

// voiceFrame: an audio frame as a client sends it, the level in the spare
// bits, the body naming the sender and a sequence number.
func (f *presenterFixture) voiceFrame(id string, level uint32, seq int) {
	f.send(id, func(w *wire.Writer) {
		w.WriteHeader(wire.OpMediaFrame, 0, false)
		w.Bits(0, 1) // audio
		w.Bits(1, 1)
		w.Bits(level, 6)
		w.Raw([]byte(fmt.Sprintf("%s#%d", id, seq)))
	})
}

// media collects the media frames that reach id within d, as
// "publisher/kind/body", in arrival order.
func (f *presenterFixture) media(id string, d time.Duration) []string {
	var out []string
	var one func([]byte) error
	one = func(frame []byte) error {
		r := wire.NewReader(frame, f.dicts[id])
		h, err := r.ReadHeader()
		if err != nil {
			return nil
		}
		switch h.Op {
		case wire.OpBatch:
			return wire.SplitBatch(r, one)
		case wire.OpMediaFrame:
			who := r.Str()
			kind := r.Bits(1)
			r.Bits(7)
			if kind == 1 {
				r.Uvarint()
			}
			out = append(out, fmt.Sprintf("%s/%d/%s", who, kind, r.Tail()))
		}
		return nil
	}
	deadline := time.After(d)
	for {
		select {
		case data := <-f.rx[id]:
			_ = one(data)
		case <-deadline:
			return out
		}
	}
}

func TestSocketVoicesPastTheLimitAreChosenNotAllRelayed(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the hold between two words")
	}
	ids := []string{"listener"}
	for i := 0; i < room.MaxVoices+2; i++ {
		ids = append(ids, fmt.Sprintf("p%d", i))
	}
	f := newPresenterFixture(t, ids...)
	six, seventh, eighth := ids[1:1+room.MaxVoices], ids[1+room.MaxVoices], ids[2+room.MaxVoices]
	const quiet, loud = 0, room.VoiceLoud

	// Six open microphones, nobody talking: all of it is relayed, as before.
	for seq := 1; seq <= 3; seq++ {
		for _, id := range six {
			f.voiceFrame(id, quiet, seq)
		}
	}
	// One over, and the next: quiet, and not relayed.
	f.voiceFrame(seventh, quiet, 1)
	f.voiceFrame(eighth, quiet, 1)
	got := f.media("listener", 300*time.Millisecond)
	if len(got) != 3*room.MaxVoices {
		t.Fatalf("six quiet microphones, three frames each: the listener got %d: %v", len(got), got)
	}
	for _, m := range got {
		if strings.HasPrefix(m, seventh+"/") || strings.HasPrefix(m, eighth+"/") {
			t.Fatalf("a quiet microphone past the limit was relayed: %s", m)
		}
	}
	// In order, per sender.
	last := map[string]string{}
	for _, m := range got {
		who := m[:strings.IndexByte(m, '/')]
		if m <= last[who] {
			t.Fatalf("out of order for %s: %s after %s", who, m, last[who])
		}
		last[who] = m
	}

	// The six stay quiet past the hold (and keep sending: their places are
	// not free). The seventh draws breath, then speaks: both frames arrive,
	// the breath first.
	time.Sleep(1600 * time.Millisecond)
	for _, id := range six {
		f.voiceFrame(id, quiet, 4)
	}
	f.voiceFrame(seventh, quiet, 2)
	f.voiceFrame(seventh, loud, 3)
	// The eighth speaks too and takes the next place.
	f.voiceFrame(eighth, loud, 2)
	f.voiceFrame(eighth, loud, 3)
	got = f.media("listener", 300*time.Millisecond)
	var of7, of8 []string
	for _, m := range got {
		switch {
		case strings.HasPrefix(m, seventh+"/"):
			of7 = append(of7, m)
		case strings.HasPrefix(m, eighth+"/"):
			of8 = append(of8, m)
		}
	}
	if want := []string{seventh + "/0/" + seventh + "#2", seventh + "/0/" + seventh + "#3"}; strings.Join(of7, " ") != strings.Join(want, " ") {
		t.Fatalf("the seventh's first word: %v, want %v (the frame before it, then it)", of7, want)
	}
	if want := []string{eighth + "/0/" + eighth + "#2", eighth + "/0/" + eighth + "#3"}; strings.Join(of8, " ") != strings.Join(want, " ") {
		t.Fatalf("the eighth: %v, want %v (nothing stale in front)", of8, want)
	}

	// From here six are heard: the two who spoke and four of the first six.
	for _, id := range ids[1:] {
		f.voiceFrame(id, quiet, 9)
	}
	// A camera is not a voice: whoever lost their place still shows.
	for _, id := range six {
		f.send(id, func(w *wire.Writer) {
			w.WriteHeader(wire.OpMediaFrame, 0, false)
			w.Bits(1, 1)
			w.Bits(1, 1)
			w.Bits(0, 6)
			w.Uvarint(1)
			w.Raw([]byte("cam"))
		})
	}
	got = f.media("listener", 300*time.Millisecond)
	voices, cams := map[string]bool{}, 0
	for _, m := range got {
		who := m[:strings.IndexByte(m, '/')]
		if strings.Contains(m, "/1/") {
			cams++
		} else {
			voices[who] = true
		}
	}
	if len(voices) != room.MaxVoices || !voices[seventh] || !voices[eighth] {
		t.Fatalf("eight microphones open: the listener hears %d (%v), want %d with the two who spoke", len(voices), voices, room.MaxVoices)
	}
	if cams != room.MaxVoices {
		t.Fatalf("six cameras sent, %d relayed: a camera must not depend on the voice", cams)
	}
	// Nobody is sent their own voice.
	for _, m := range f.media(seventh, 200*time.Millisecond) {
		if strings.HasPrefix(m, seventh+"/") {
			t.Fatalf("the seventh was relayed its own frame: %s", m)
		}
	}
}

// A hundred microphones open and three people talking, through the real
// dispatcher and a hundred real sockets: every frame of the three reaches
// everyone else, in order, and nobody is sent more than six voices. Before
// voice selection this room cost each person a hundred streams (the ceiling
// row of TestHundredPersonRoomRelaysEveryAudioFrameToEveryoneElse). Run
// with -v for the figures.
func TestHundredOpenMicrophonesThreeTalkingRelaysSix(t *testing.T) {
	if testing.Short() {
		t.Skip("100 real sockets")
	}
	const N, fps, seconds = 100, 50, 1
	ids := make([]string, N)
	for i := range ids {
		ids[i] = fmt.Sprintf("p%03d", i)
	}
	f := newPresenterFixture(t, ids...)
	talkers := map[string]bool{ids[N-3]: true, ids[N-2]: true, ids[N-1]: true} // the last to send in every 20 ms

	type heard struct {
		seqs  map[string][]uint64
		bytes int
	}
	got := make([]heard, N)
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i, id := range ids {
		got[i].seqs = map[string][]uint64{}
		readers.Add(1)
		go func(i int, id string) {
			defer readers.Done()
			var one func([]byte) error
			one = func(frame []byte) error {
				r := wire.NewReader(frame, f.dicts[id])
				h, err := r.ReadHeader()
				if err != nil {
					return nil
				}
				switch h.Op {
				case wire.OpBatch:
					return wire.SplitBatch(r, one)
				case wire.OpMediaFrame:
					who := r.Str()
					r.Bits(8)
					if body := r.Tail(); len(body) >= 8 {
						got[i].seqs[who] = append(got[i].seqs[who], binary.BigEndian.Uint64(body))
					}
				}
				return nil
			}
			for {
				select {
				case data := <-f.rx[id]:
					got[i].bytes += len(data)
					_ = one(data)
				case <-stop:
					return
				}
			}
		}(i, id)
	}

	payload := make([]byte, 120) // a 48 kbit/s x 20 ms Opus frame; the first 8 bytes are the sequence
	tick := time.NewTicker(time.Second / fps)
	defer tick.Stop()
	var busy time.Duration
	start := time.Now()
	for seq := 1; seq <= fps*seconds; seq++ {
		<-tick.C
		t0 := time.Now()
		binary.BigEndian.PutUint64(payload, uint64(seq))
		for _, id := range ids {
			level := uint32(0)
			if talkers[id] {
				level = 20
			}
			f.send(id, func(w *wire.Writer) {
				w.WriteHeader(wire.OpMediaFrame, 0, false)
				w.Bits(0, 1)
				w.Bits(1, 1)
				w.Bits(level, 6)
				w.Raw(payload)
			})
		}
		busy += time.Since(t0)
	}
	wall := time.Since(start)
	time.Sleep(700 * time.Millisecond) // the coalescers flush, the readers drain
	close(stop)
	readers.Wait()

	total, most := 0, 0
	for i, id := range ids {
		for talker := range talkers {
			if talker == id {
				if len(got[i].seqs[talker]) != 0 {
					t.Fatalf("%s was sent its own voice", id)
				}
				continue
			}
			s := got[i].seqs[talker]
			if len(s) != fps*seconds {
				t.Fatalf("%s got %d of %d frames from %s, who was talking", id, len(s), fps*seconds, talker)
			}
			for k, v := range s {
				if v != uint64(k+1) {
					t.Fatalf("%s got %s's frames out of order: %v", id, talker, s[:k+1])
				}
			}
		}
		voices := 0
		for _, s := range got[i].seqs {
			if len(s) > 1 { // more than the one frame a place was held for at the start
				voices++
			}
		}
		if voices > room.MaxVoices {
			t.Fatalf("%s was sent %d voices, more than %d", id, voices, room.MaxVoices)
		}
		most = max(most, voices)
		total += got[i].bytes
	}
	if most != room.MaxVoices {
		t.Fatalf("the most voices anyone was sent is %d, want %d (three talking and three places kept)", most, room.MaxVoices)
	}
	perRecipient := total / N / seconds
	t.Logf("100 microphones open, 3 talking, %d s: %d frames in, relay work %v of %v wall (%.1f%% of one core), %.1f µs per frame in; %d B/s per recipient (%.0f kbit/s)",
		seconds, N*fps*seconds, busy.Round(time.Millisecond), wall.Round(time.Millisecond), 100*busy.Seconds()/wall.Seconds(),
		float64(busy.Microseconds())/float64(N*fps*seconds), perRecipient, float64(perRecipient)*8/1000)
	if perRecipient > 60_000 {
		t.Fatalf("%d B/s per recipient: more than six voices' worth", perRecipient)
	}
}
