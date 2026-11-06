// Command sfuload puts N people into one meeting over WebRTC, the path real
// browsers use, and measures what each of them receives.
//
// Each person signs in as themselves (the session cookies are given), joins
// over the meeting socket, and negotiates the two peer connections the
// browser does (it offers the publishing one; the server offers the
// subscribing one). Each publishes a synthetic VP8 camera and an Opus
// microphone over real ICE, DTLS and SRTP, with the send time inside every
// payload, and reports a gallery of `shown` tiles as on screen. Then it
// counts, per track, what arrives: on-screen cameras, off-screen cameras and
// audio, with latency.
//
// It is driven by tools/e2e/stress.mjs, which builds the server, makes the
// accounts and reads the JSON this prints. ARCHITECTURE 6bu.
//
//	sfuload -in people.json -secs 20 -shown 9
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aether-rtc/aether/internal/wire"
	"github.com/coder/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type input struct {
	Base   string   `json:"base"` // http://host:port
	Room   string   `json:"room"`
	People []person `json:"people"`
	Admin  person   `json:"admin"` // for /api/telemetry: what the server thinks of a stalled feed
}

type person struct {
	Cookie string `json:"cookie"`
	IP     string `json:"ip"`
	Name   string `json:"name"`
}

// track is one received track's counters.
type track struct {
	publisher string
	kind      string
	packets   atomic.Int64
	bytes     atomic.Int64
	latMu     sync.Mutex
	lat       []float64 // ms, sampled
}

type client struct {
	idx    int
	id     string
	ws     *websocket.Conn
	tx     *wire.Dict
	wmu    sync.Mutex
	pub    *webrtc.PeerConnection
	sub    *webrtc.PeerConnection
	tracks sync.Map // track id -> *track
	ready  chan struct{}
	once   sync.Once
	errs   atomic.Int64
	// pending candidates that arrived before the remote description
	pendMu  sync.Mutex
	pending map[wire.Direction][]webrtc.ICECandidateInit
}

// vp8Stamp is where a video payload carries its send time: past the
// descriptor and the key frame's payload header and start code.
const vp8Stamp = 8

var epoch = time.Now()

// A few raw payload heads, for checking what the server forwards.
var (
	heads    atomic.Int64
	headMu   sync.Mutex
	headList []string
)

func nowMs() float64 { return float64(time.Since(epoch).Microseconds()) / 1000 }

func (c *client) send(enc func(w *wire.Writer)) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	w := wire.NewWriter(c.tx)
	enc(w)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageBinary, w.Bytes()); err != nil {
		c.errs.Add(1)
	}
}

func (c *client) pc(dir wire.Direction) *webrtc.PeerConnection {
	if dir == wire.Direction(1) {
		return c.sub
	}
	return c.pub
}

func (c *client) addICE(dir wire.Direction, init webrtc.ICECandidateInit) {
	pc := c.pc(dir)
	if pc.RemoteDescription() == nil {
		c.pendMu.Lock()
		c.pending[dir] = append(c.pending[dir], init)
		c.pendMu.Unlock()
		return
	}
	_ = pc.AddICECandidate(init)
}

func (c *client) flushICE(dir wire.Direction) {
	c.pendMu.Lock()
	list := c.pending[dir]
	c.pending[dir] = nil
	c.pendMu.Unlock()
	for _, init := range list {
		_ = c.pc(dir).AddICECandidate(init)
	}
}

func join(in input, idx int, p person) (*client, error) {
	u := strings.Replace(in.Base, "http", "ws", 1) + "/rtc?room=" + in.Room + "&name=" + strings.ReplaceAll(p.Name, " ", "%20") + "&media=webrtc"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{
		"Cookie": {p.Cookie}, "X-Forwarded-For": {p.IP}, "Origin": {in.Base},
	}})
	if err != nil {
		return nil, fmt.Errorf("dial %d: %w", idx, err)
	}
	ws.SetReadLimit(64 << 20)
	tx, rx := wire.NewPreloadedPair()
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	// What a browser does: the default codecs, and NACK, receiver reports and
	// transport-wide congestion feedback. Without the feedback the server's
	// bandwidth estimate for this receiver has nothing to go on.
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	reg := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, reg); err != nil {
		return nil, err
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se), webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(reg))
	pub, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	sub, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	c := &client{idx: idx, ws: ws, tx: tx, pub: pub, sub: sub, ready: make(chan struct{}), pending: map[wire.Direction][]webrtc.ICECandidateInit{}}

	for dir, pc := range map[wire.Direction]*webrtc.PeerConnection{0: pub, 1: sub} {
		pc.OnICECandidate(func(ic *webrtc.ICECandidate) {
			if ic == nil {
				return
			}
			b, _ := json.Marshal(ic.ToJSON())
			m := wire.ICE{Dir: dir, Candidate: string(b)}
			c.send(m.Encode)
		})
	}
	sub.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		t := &track{publisher: tr.StreamID(), kind: tr.Kind().String()}
		v, _ := c.tracks.LoadOrStore(tr.ID(), t)
		t = v.(*track)
		go func() {
			for {
				pkt, _, err := tr.ReadRTP()
				if err != nil {
					return
				}
				t.packets.Add(1)
				t.bytes.Add(int64(len(pkt.Payload)))
				if n := heads.Add(1); n <= 6 {
					headMu.Lock()
					headList = append(headList, fmt.Sprintf("%s pt=%d len=%d pad=%v %x", t.kind, pkt.PayloadType, len(pkt.Payload), pkt.Padding, pkt.Payload[:min(16, len(pkt.Payload))]))
					headMu.Unlock()
				}
				if pl := pkt.Payload; t.packets.Load()%8 == 0 {
					off := vp8Stamp
					if t.kind == "audio" {
						off = 0
					}
					if len(pl) >= off+8 {
						sent := float64(binary.BigEndian.Uint64(pl[off:off+8])) / 1000
						t.latMu.Lock()
						t.lat = append(t.lat, nowMs()-sent)
						t.latMu.Unlock()
					}
				}
			}
		}()
	})
	if _, err := sub.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		return nil, err
	}

	// The server's frames: only Welcome, SDP and ICE matter here, and SDP and
	// ICE carry their bodies as blobs, so skipping every other frame cannot
	// desynchronise the strings this client reads.
	go func() {
		for {
			_, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var each func([]byte) error
			each = func(f []byte) error {
				r := wire.NewReader(f, rx)
				h, err := r.ReadHeader()
				if err != nil {
					return nil
				}
				switch h.Op {
				case wire.OpBatch:
					return wire.SplitBatch(r, each)
				case wire.OpWelcome:
					if c.id == "" {
						c.id = r.Str()
						c.once.Do(func() { close(c.ready) })
					}
				case wire.OpSdp:
					var m wire.SDP
					if m.Decode(r) != nil {
						return nil
					}
					if m.Dir == wire.Direction(0) { // the answer to our offer
						if err := pub.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.Body}); err != nil {
							c.errs.Add(1)
						}
						c.flushICE(0)
						return nil
					}
					if err := sub.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: m.Body}); err != nil {
						c.errs.Add(1)
						return nil
					}
					c.flushICE(1)
					ans, err := sub.CreateAnswer(nil)
					if err == nil && sub.SetLocalDescription(ans) == nil {
						reply := wire.SDP{Dir: 1, IsOffer: false, Body: ans.SDP}
						c.send(reply.Encode)
					} else {
						c.errs.Add(1)
					}
				case wire.OpIce:
					var m wire.ICE
					if m.Decode(r) != nil {
						return nil
					}
					var init webrtc.ICECandidateInit
					if json.Unmarshal([]byte(m.Candidate), &init) == nil {
						c.addICE(m.Dir, init)
					}
				}
				return nil
			}
			_ = each(data)
		}
	}()
	return c, nil
}

// publish adds a camera, and a microphone if this person is talking, and
// sends until stop. (A muted browser sends no audio at all.)
func (c *client) publish(stop <-chan struct{}, talking bool) error {
	cam, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, fmt.Sprint("cam-", c.idx), c.id)
	if err != nil {
		return err
	}
	mic, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, fmt.Sprint("mic-", c.idx), c.id)
	if err != nil {
		return err
	}
	locals := []webrtc.TrackLocal{cam}
	if talking {
		locals = append(locals, mic)
	}
	for _, tr := range locals {
		s, err := c.pub.AddTrack(tr)
		if err != nil {
			return err
		}
		go func() {
			buf := make([]byte, 1500)
			for {
				if _, _, err := s.Read(buf); err != nil {
					return
				}
			}
		}()
	}
	offer, err := c.pub.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := c.pub.SetLocalDescription(offer); err != nil {
		return err
	}
	m := wire.SDP{Dir: 0, IsOffer: true, Body: offer.SDP}
	c.send(m.Encode)

	// Camera: 15 frames a second, two 1,000-byte packets each, a key frame
	// (eight packets) every 30 frames. Microphone: 120 bytes every 20 ms.
	go func() {
		var seq uint16
		var ts uint32
		frame := 0
		tick := time.NewTicker(time.Second / 15)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			key := frame%30 == 0
			n := 2
			if key {
				n = 8
			}
			for i := 0; i < n; i++ {
				pl := make([]byte, 1000)
				if i == 0 {
					// The VP8 descriptor (S: a partition starts here), then the
					// payload header: P=0 and the start code mark a key frame.
					pl[0] = 0x10
					if key {
						pl[1], pl[2], pl[3], pl[4], pl[5] = 0x00, 0x00, 0x9d, 0x01, 0x2a
					} else {
						pl[1] = 0x01
					}
				}
				binary.BigEndian.PutUint64(pl[vp8Stamp:vp8Stamp+8], uint64(nowMs()*1000))
				_ = cam.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: ts, Marker: i == n-1}, Payload: pl})
				seq++
			}
			ts += 6000
			frame++
		}
	}()
	if !talking {
		return nil
	}
	go func() {
		var seq uint16
		var ts uint32
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			pl := make([]byte, 120)
			binary.BigEndian.PutUint64(pl[0:8], uint64(nowMs()*1000))
			_ = mic.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: ts}, Payload: pl})
			seq++
			ts += 960
		}
	}()
	return nil
}

// telemetryFeeds is the server's view of every forwarded feed, keyed
// "subscriber|track": layer, switches, visibility, packets.
func telemetryFeeds(in input) map[string]string {
	out := map[string]string{}
	if in.Admin.Cookie == "" {
		return out
	}
	req, _ := http.NewRequest("GET", in.Base+"/api/telemetry", nil)
	req.Header.Set("Cookie", in.Admin.Cookie)
	req.Header.Set("X-Forwarded-For", in.Admin.IP)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return out
	}
	defer res.Body.Close()
	var t struct {
		Media []struct {
			Feeds     []map[string]any `json:"feeds"`
			Estimates map[string]int   `json:"estimates"`
		} `json:"media"`
	}
	if json.NewDecoder(res.Body).Decode(&t) != nil {
		return out
	}
	for _, m := range t.Media {
		for _, f := range m.Feeds {
			f["subscriberEstimate"] = m.Estimates[fmt.Sprint(f["subscriber"])]
			b, _ := json.Marshal(f)
			out[fmt.Sprint(f["subscriber"])+"|"+fmt.Sprint(f["track"])] = string(b)
		}
	}
	return out
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return -1
	}
	sort.Float64s(xs)
	i := int(p / 100 * float64(len(xs)))
	if i >= len(xs) {
		i = len(xs) - 1
	}
	return xs[i]
}

func main() {
	inPath := flag.String("in", "", "people.json")
	secs := flag.Int("secs", 20, "seconds of media to measure")
	shown := flag.Int("shown", 9, "cameras each person shows")
	talkers := flag.Int("talkers", 3, "people with their microphone on")
	settle := flag.Int("settle", 12, "seconds for negotiation to settle before measuring")
	flag.Parse()
	raw, err := os.ReadFile(*inPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	t0 := time.Now()
	clients := make([]*client, len(in.People))
	var wg sync.WaitGroup
	var joinErrs atomic.Int64
	for i, p := range in.People {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := join(in, i, p)
			if err != nil {
				joinErrs.Add(1)
				fmt.Fprintln(os.Stderr, err)
				return
			}
			select {
			case <-c.ready:
			case <-time.After(15 * time.Second):
				joinErrs.Add(1)
				return
			}
			clients[i] = c
		}()
	}
	wg.Wait()
	joinMs := time.Since(t0).Milliseconds()

	stop := make(chan struct{})
	for _, c := range clients {
		if c != nil {
			if err := c.publish(stop, c.idx < *talkers); err != nil {
				c.errs.Add(1)
			}
		}
	}
	// Negotiation and subscriptions settle; then each person says which
	// cameras are on screen: the next `shown` people after them.
	time.Sleep(time.Duration(*settle) * time.Second)
	ids := make([]string, len(clients))
	for i, c := range clients {
		if c != nil {
			ids[i] = c.id
		}
	}
	shownBy := make([]map[string]bool, len(clients))
	reported := make([]map[string]bool, len(clients))
	for i, c := range clients {
		if c == nil {
			continue
		}
		shownBy[i] = map[string]bool{}
		for k := 1; k <= *shown && k < len(clients); k++ {
			shownBy[i][ids[(i+k)%len(clients)]] = true
		}
		var tiles []wire.Tile
		reported[i] = map[string]bool{}
		c.tracks.Range(func(k, v any) bool {
			t := v.(*track)
			if t.kind == "video" {
				tiles = append(tiles, wire.Tile{Track: k.(string), Width: 320, Height: 180, Visible: shownBy[i][t.publisher]})
				reported[i][k.(string)] = true
			}
			return true
		})
		vp := wire.Viewport{Tiles: tiles}
		c.send(vp.Encode)
	}
	time.Sleep(3 * time.Second) // the allocator acts on the reports
	// Measure from here.
	base := make([]map[string][2]int64, len(clients))
	for i, c := range clients {
		if c == nil {
			continue
		}
		base[i] = map[string][2]int64{}
		c.tracks.Range(func(k, v any) bool {
			t := v.(*track)
			base[i][k.(string)] = [2]int64{t.packets.Load(), t.bytes.Load()}
			t.latMu.Lock()
			t.lat = nil
			t.latMu.Unlock()
			return true
		})
	}
	cpu0 := processCPU()
	time.Sleep(time.Duration(*secs) * time.Second)
	cpu1 := processCPU()
	feeds := telemetryFeeds(in)
	close(stop)
	window := float64(*secs)

	type out struct {
		People        int      `json:"people"`
		Joined        int      `json:"joined"`
		JoinMs        int64    `json:"joinMs"`
		Errors        int64    `json:"errors"`
		VideoTracks   []int    `json:"videoTracksPerPerson"` // tracks received
		AudioTracks   []int    `json:"audioTracksPerPerson"`
		ShownKbps     float64  `json:"shownCameraKbpsAvg"`  // per on-screen camera
		HiddenKbps    float64  `json:"hiddenCameraKbpsAvg"` // per off-screen camera
		ShownStalled  int      `json:"shownCamerasStalled"` // on screen, < 5 packets/s
		StalledWhy    []string `json:"stalled,omitempty"`   // receiver, publisher, packets, reported
		LatSamples    int      `json:"latencySamples"`
		LatBad        int      `json:"latencyBad"` // < 0 or > 10 s: a stamp misread
		Heads         []string `json:"heads,omitempty"`
		AudioStalled  int      `json:"audioStalled"` // < 40 packets/s
		RecvMbpsP50   float64  `json:"recvMbpsP50"`
		RecvMbpsMax   float64  `json:"recvMbpsMax"`
		VideoLatP50   float64  `json:"videoLatencyP50"`
		VideoLatP99   float64  `json:"videoLatencyP99"`
		AudioLatP50   float64  `json:"audioLatencyP50"`
		AudioLatP99   float64  `json:"audioLatencyP99"`
		WindowSeconds float64  `json:"windowSeconds"`
		ClientCores   float64  `json:"loadClientCoresBusy"`
		Talkers       int      `json:"talkers"`
	}
	o := out{People: len(clients), JoinMs: joinMs, WindowSeconds: window, Talkers: min(*talkers, len(clients))}
	o.ClientCores = (cpu1 - cpu0).Seconds() / window
	var shownSum, hiddenSum float64
	var shownN, hiddenN int
	var recv []float64
	var vlat, alat []float64
	for i, c := range clients {
		if c == nil {
			continue
		}
		o.Joined++
		o.Errors += c.errs.Load()
		vt, at := 0, 0
		var bytes int64
		c.tracks.Range(func(k, v any) bool {
			t := v.(*track)
			b := base[i][k.(string)]
			dp, db := t.packets.Load()-b[0], t.bytes.Load()-b[1]
			bytes += db
			t.latMu.Lock()
			if t.kind == "video" {
				vt++
				vlat = append(vlat, t.lat...)
				kbps := float64(db*8) / window / 1000
				if shownBy[i][t.publisher] {
					shownSum += kbps
					shownN++
					if float64(dp)/window < 5 {
						o.ShownStalled++
						if len(o.StalledWhy) < 10 {
							o.StalledWhy = append(o.StalledWhy, fmt.Sprintf("receiver %d, publisher %s: %d packets, reported %v; server: %s", i, t.publisher, dp, reported[i][k.(string)], feeds[c.id+"|"+k.(string)]))
						}
					}
				} else {
					hiddenSum += kbps
					hiddenN++
				}
			} else {
				at++
				alat = append(alat, t.lat...)
				if float64(dp)/window < 40 {
					o.AudioStalled++
				}
			}
			t.latMu.Unlock()
			return true
		})
		o.VideoTracks = append(o.VideoTracks, vt)
		o.AudioTracks = append(o.AudioTracks, at)
		recv = append(recv, float64(bytes*8)/window/1e6)
	}
	if shownN > 0 {
		o.ShownKbps = shownSum / float64(shownN)
	}
	if hiddenN > 0 {
		o.HiddenKbps = hiddenSum / float64(hiddenN)
	}
	o.RecvMbpsP50 = pct(recv, 50)
	o.RecvMbpsMax = pct(recv, 100)
	for _, l := range append(append([]float64(nil), vlat...), alat...) {
		o.LatSamples++
		// Zero is not bad: loopback forwarding is quicker than the clock's
		// tick (about 0.5 ms on Windows). Negative or absurd is a misread.
		if l < 0 || l > 10000 {
			o.LatBad++
		}
	}
	o.VideoLatP50, o.VideoLatP99 = pct(vlat, 50), pct(vlat, 99)
	o.AudioLatP50, o.AudioLatP99 = pct(alat, 50), pct(alat, 99)
	o.Heads = headList
	b, _ := json.Marshal(o)
	fmt.Println(string(b))
	for _, c := range clients {
		if c != nil {
			_ = c.pub.Close()
			_ = c.sub.Close()
			_ = c.ws.CloseNow()
		}
	}
	if joinErrs.Load() > 0 {
		os.Exit(2)
	}
}
