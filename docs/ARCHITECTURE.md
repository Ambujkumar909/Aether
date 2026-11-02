# Aether — system architecture

> Quality is never traded for bandwidth. Bits are removed only where they
> carry no perceptual information, and the capacity that frees is spent
> putting quality back.

Everything below is either implemented or explicitly marked as not. Numbers
are measured with `tools/bitcheck.mjs` against real captured packets, not
computed from a model.

---

## 1. The thesis, and what measurement did to it

The project began on a claim: most of what a video call transmits is not
picture or sound. Capturing real traffic both confirmed that and corrected
the way it should be stated.

**Measured, control plane, worst case** — one logical frame per WebSocket
message, which is what a naive client produces:

```
application payload    479 B   27.1%
WebSocket framing       70 B    4.0%
TCP header             380 B   21.5%
IP header (v6)         760 B   43.1%
link framing            76 B    4.3%
                      ─────
TOTAL                 1765 B     payload efficiency 27.1%
vs JSON control plane                          3.4× smaller
```

**Measured, same protocol, realistic burst traffic** — the coalescing window
doing its job at 12.38 frames per message:

```
application payload    505 B   48.0%
WebSocket framing       36 B    3.4%
TCP header             160 B   15.2%
IP header (v6)         320 B   30.4%
link framing            32 B    3.0%
                      ─────
TOTAL                 1053 B     payload efficiency 48.0%
vs JSON control plane                         22.8× smaller
```

Three things follow, and the second one is uncomfortable:

1. **The 3-byte state delta is real.** Decoded from captured bytes, a
   `StateDelta` averages **3.2 B** on the wire and a `Ping` is exactly
   **1.0 B**. The encoder does what the tests claim.

2. **A dense protocol alone does not produce a dense connection.** At one
   frame per message, 60 bytes of IP+TCP ride every 25-byte payload and the
   end-to-end win collapses from 57× per-frame to 3.4× overall. The header
   is the product when the payload is small. Any claim quoting only the
   per-frame ratio is telling a true fact misleadingly.

3. **Coalescing is therefore not a micro-optimisation, it is the mechanism.**
   Moving from 1 to 12.38 frames per message nearly doubles payload
   efficiency and takes the JSON comparison from 3.4× to 22.8×, with no
   change to the encoding at all.

That is why the client and server both hold a **15 ms coalescing window**.
Fifteen milliseconds is below the threshold at which a UI state change reads
as anything other than instant, and it is long enough to capture the bursts
real interaction actually produces.

---

## 2. Shape

One process. One binary. One database.

```
                      ┌──────────────────────────────────────┐
   browser  ──────────┤  cmd/aether                          │
   ├ wire.js          │                                      │
   ├ rtc.js           │  internal/modules/     (vertical)    │
   ├ chat.js          │    identity   auth, RBAC, sessions   │
   └ app.js           │    audit      hash-chained record    │
                      │    scheduling meetings, invitations  │
      WSS /rtc  ──────┤    messaging  persistent chat + hub  │
      WSS /ws   ──────┤                                      │
      HTTPS     ──────┤  internal/     (engine, not modules) │
                      │    wire       binary control codec   │
      SRTP/DTLS ──────┤    sfu        allocator, BWE, router │
                      │    ledger     per-byte accounting    │
                      │    room       roster, speaker rank   │
                      │    signal     WS gateway, coalescer  │
                      │    modular    lifecycle kernel       │
                      │                                      │
                      │  internal/platform/                  │
                      │    database   one handle, migrations │
                      │    httpx      JSON, middleware, RL   │
                      └──────────────────────────────────────┘
```

**Why a monolith.** The distance between "a packet arrived" and "the packet
was forwarded" is the product. Every process boundary on that path is a
serialisation, a scheduler hop and a queue. Splitting signalling from media
means a mute toggle crosses three networks before the person who needs to see
it does.

**Why modules inside it.** A monolith without enforced seams rots. Each
feature is a vertical slice — `domain.go` (types + ports), `repository.go`,
`service.go`, `handler.go`, `module.go` — owning its own tables, exposing a
narrow interface at its package root, and importing no other module's
internals. The dependency graph is declared, and the kernel refuses to start
on a cycle.

Cross-module wiring is by interface, resolved in `main.go`:

- `identity` holds an `audit.Recorder`; `audit` holds an `audit.Guard` that
  `identity` happens to satisfy. Neither imports the other.
- `messaging` needs to resolve users, so it declares a `Directory` interface;
  `identity` satisfies it.
- `identity`'s directory reports who is online, which only `messaging`'s hub
  knows, so `identity` declares a `PresenceSource` and `main.go` connects it.

---

## 3. The media plane

### Two peer connections per participant

The publisher connection is always offered by the client; the subscriber
connection is always offered by the server. That asymmetry makes
renegotiation glare **structurally impossible** rather than something to
survive — each connection has exactly one possible offerer. The cost is a
second ICE and DTLS handshake at join, paid once, in parallel, before anyone
is looking.

### The forwarder is where an SFU is won or lost

Each simulcast rung is an independent encode with its own sequence and
timestamp space. The receiver believes it is watching one stream. A forwarder
that relays whichever layer is selected produces, on the first quality
change, a sequence jump of thousands, a jitter buffer that concludes it lost
an enormous burst, and a freeze until the next keyframe.

`sfu.Forwarder` maintains its own outbound space and translates every packet
into it. Switching happens only on a keyframe of the target layer, and the
outbound sequence advances by exactly one across the switch — a gap would be
read as loss and trigger NACKs for packets that never existed.

```
--- PASS: TestManySwitchesStayContiguous
    6 switches, 117 packets, zero discontinuities
```

Genuine upstream loss still shows through as a gap, because the receiver's
concealment logic depends on seeing it.

### Allocation: never send what cannot be seen

`sfu.Allocate` is a marginal-utility greedy over layer upgrades. The rules
that matter:

- **Spatial utility saturates at the viewport.** A tile painted 104 px wide
  is served a stream sized for 104 px. Exceeding it earns nothing.
- **Invisible means zero, not low.** A tile scrolled out of view or in a
  background tab receives nothing at all.
- **A marginal-utility floor** (`DefaultMinUtilityPerMbps = 1.0`) stops the
  allocator buying 30 fps for a thumbnail at four times the bitrate. Spatial
  saturation alone misses this, because the temporal term keeps earning after
  the spatial one flatlines.
- **Unspent capacity is not idle, it is loaded** — it becomes burst headroom.

```
--- PASS: TestGalleryCostIsBoundedByWhatIsPainted
    12-person gallery: aether=1005000 bps  viewport-blind=4200000 bps  (4.2×)
```

The client reports painted size in device pixels from a live measurement of
the element, not a guess from a layout name — "gallery tile" is 90 px on a
phone and 400 on a monitor.

### Congestion control: capacity memory

Standard GCC has excellent detection and poor recovery. After a transient —
a microwave, a wifi roam, one dropped burst — the estimate collapses in one
multiplicative step and then crawls back over many seconds, during which the
user watches a degraded picture on a healthy link.

`sfu.Estimator` remembers the highest rate the path recently sustained. Below
that remembered ceiling it ramps back multiplicatively; only above it does it
probe cautiously, because only there is it making a new claim.

```
--- PASS: TestRecoversFromTransientInUnderOneSecond
    dip to 1020000 bps; recovered in 400ms, GCC-style would take 12.6s (32×)
```

The subtlety that took two attempts to get right: **a single overuse event
must not erase the memory.** Slamming the ceiling down on every overuse
destroys the memory exactly when it is most valuable and reduces the
controller to the crawl it exists to replace. It takes two corroborating
events inside a window to write the ceiling down, which learns a real
capacity drop within two cycles while a lone glitch costs nothing lasting.

### Bursts are bounded by measured capacity

`MaxBurstRatio = 1.25`. This is not timidity, and it is where an intuition
about "no limits" meets physics: pushing 8 Mbps into a 2 Mbps path does not
deliver sharp motion, it parks 6 Mbps in an upstream queue 1.5 seconds deep,
adds latency, and trips our own overuse detector. The cap is **physical, not
policy** — on a link that genuinely carries 50 Mbps the estimate climbs there
and bursts take it. What the token bucket buys is the right to spend reserved
headroom *instantly* when a scene changes, instead of waiting for the next
allocation cycle.

Verified free in aggregate:

```
--- PASS: TestBurstsDoNotRaiseTheAverage
    60s with a 9 Mbps burst every 4s: average 1091917 bps
```

---

## 4. The control plane

A binary protocol, not JSON. Design rules in priority order:

1. **The common case is one byte.** A heartbeat is 1 B of payload — measured
   at exactly 1.0 B on captured wire.
2. **Nothing is spelled twice.** Identifiers are interned per direction; a
   36-byte UUID costs 36 bytes once and 1 byte thereafter.
3. **Booleans are bits.** Six presence flags and a 2-bit role pack into one
   octet — the whole `StateDelta` is 3 bytes against 172 as JSON.
4. **Numbers are variable width.** LEB128 magnitudes, zigzag deltas,
   timestamps always relative to a session epoch.

### The dictionary hazard

Interning is cross-frame state, and that is dangerous in a specific way worth
recording. A handle means "the string at index N", and both peers derive N by
counting literals in arrival order. Lose one frame carrying a literal and
every handle afterwards resolves to the wrong string — silently, plausibly,
permanently.

Two consequences are load-bearing:

- **Two dictionaries per connection, one per direction.** A single shared
  table desyncs when two frames cross in flight, because both claim the same
  next index. `TestDirectionalDictionariesDoNotDesync` encodes this.
- **The protocol REQUIRES a reliable ordered transport.** There is no cheap
  repair for a desynchronised table; the only fix is a full resync, which is
  the heaviest thing the protocol can do, triggered at the exact moment the
  path is least able to carry it.

That last point is a general rule, learned the expensive way in a sibling
project where a delta-coded screen codec — 24× cheaper per frame on paper —
spent 13–21 MB repainting one screen after a single drop:

> **Never introduce cross-frame state unless recovery from its loss is cheap.**

Standard video codecs may have inter-frame dependency precisely because they
ship with decades of loss-recovery engineering. Hand-rolled schemes may not.

---

## 4a. Screen capture for remote control

`internal/screen` is where that rule is applied rather than merely stated.
Every frame is a **complete, self-contained JPEG of one rectangle**. It
decodes with no reference to any other frame, so a dropped frame costs exactly
one rectangle and never corrupts what follows. What the package optimises is
not the encoding of a frame but *which rectangles are worth sending at all*.

### What a minute actually costs

1920×1080 at 20 fps, a support session that is 70% reading, 25% typing, 5%
scrolling — the shape of real remote assistance:

| | before | after |
|---|---|---|
| total for one minute | 2,075,371 B | **1,423,604 B** |
| sustained | 277 kbps | **190 kbps** |
| whole-screen frames | 49 | **1** |
| captures that sent nothing | 71% | 68% |
| CPU per capture | 1.16 ms | 1.40 ms |

31% fewer bytes. The two remaining columns that got *worse* are both honest
costs of being more correct, and are covered below.

### Three findings, each measured

**1. Escalating to a full screen is worse than bounding the change.**
The first design sent the entire screen once enough tiles had moved. But the
commonest event in real use is repainting one window, and a 1440×760 window on
a 1080p screen is 53% of the area — over the threshold. So every window
repaint shipped the other 47% of the desktop, unchanged, at full quality.
Collapsing to the **bounding box** of the change instead is never worse and
usually far better: measured, one such repaint went from a 206,158 B
whole-screen frame to 24,438 B, avoiding 43% of the screen.

**2. Periodic full frames were 81% of all traffic, and insured against
nothing that could not be covered exactly.**
A whole-screen resync every 10 seconds cost 1.24 MB of the original 1.54 MB.
It existed to repair three different failures, and each turned out to have a
precise, cheap fix:

| failure | blind repair | exact repair |
|---|---|---|
| subsampled hashing misses a tiny change | resend everything, ≤10 s late | rolling exact verifier: **831 B, ≤900 ms** |
| the byte budget refuses a frame | resend everything, ≤10 s late | `Detector.Retry` keeps the tiles dirty until one goes out |
| loss on the wire | resend everything, ≤10 s late | sequence gap → `ForceFull`, already present |

The verifier is the interesting one. Rather than *resending* a region in the
hope it is stale, it **hashes the region exactly** (every pixel, not every
4th) and compares against the last exact hash of what was sent. Matching
regions cost nothing at all. Only a genuine difference produces bytes. It is
therefore free on a static screen, and it closes the blind spot roughly ten
times faster than the mechanism it replaced.

`FullFrameEvery` still exists at 60 s, purely as defence in depth against a
bug in one of the three mechanisms above — because the failure it guards is
the worst kind, a viewer quietly showing the wrong screen with nothing to
indicate it.

This is why 68% of captures are silent where 71% were before: the verifier
caught **58 real changes per minute** that the old design left visibly stale
for up to ten seconds. The extra 0.24 ms of CPU buys those.

**3. A frame larger than the burst is not slow, it is impossible.**
Adding retry-on-refusal reproduced the sibling project's failure in miniature,
and the tests caught it: a 206 kB whole-screen frame against a 60 kB burst can
*never* be admitted, so re-offering it whole meant it was refused forever —
4.3 MB refused, zero bytes sent, CPU burned re-encoding an impossible frame on
every capture. Recovery had become the most expensive operation and was being
retried under congestion, which is precisely the trap this package exists to
avoid. Two fixes, both physical rather than policy:

- **Split to fit.** Rectangles are subdivided until their estimated cost fits
  the burst. Safe here for exactly the same reason dropping is safe: half a
  region is a complete, decodable picture of half a region.
- **Pace the retry.** A refused region is not retried until the bucket could
  actually hold it. Until then captures are skipped before any hashing or
  encoding, so a saturated link costs no CPU.

Measured after: an entire screen delivered through a 60 kB burst in 27
captures, 23 of them skipped for free.

### One incidental bug worth recording

`Budget` read `time.Now()` internally while the encoder took an injected
clock. A test could advance twenty simulated seconds inside a third of a real
one; the encoder judged a refused region ready to retry while the bucket was
still empty. The retry path looked correct and was silently dead. Threading
the clock through `Allow`, `Wait` and `Available` made the whole package
deterministic — **a pacing mechanism that cannot be run on a synthetic clock
cannot be tested.**

---

## 4b. Teardown

Close paths get less attention than open paths because they are hard to
observe: nothing looks wrong, the process eventually exits, and the damage
shows up as a slow leak or a corrupted session that nobody can reproduce. So
they were tested deliberately rather than incidentally, with real websockets
rather than fakes -- a fake cannot get close wrong, so a test built on one
proves nothing about the code that can.

Eight defects came out of it. Six were latent; two were reproducible on demand.

### Encoding and queueing must be one step

The interning dictionary requires that frames reach the wire in the order
their strings were interned. Both socket paths violated it, in the same shape:
encode under one lock, queue under another.

| | chat Hub | meeting Conn |
|---|---|---|
| before | encode under `s.mu`, queue after unlocking | `Encode` under `writeMu`, `Send` under `mu` |
| gap | a few nanoseconds | two function calls |
| reproduced | 7 rounds in 400 | corrupted by round 43 |

The consequence is not a reordering artefact. Interning assigns a string the
next free index and emits the binding **once**; every later frame emits only
the index. A receiver that meets an index before the frame defining it has no
way to recover and no way to notice -- there is no checksum on a string index
and no resynchronising message. Every interned string on that connection
decodes to the wrong value, or fails outright, for as long as it stays open.

Both are now single operations under a single lock: `sock.send` and
`Conn.Emit`. `Conn.Encode` was removed rather than fixed, so the two-step
version is not merely discouraged but unavailable.

The reproduction is worth recording because the first attempt at it **passed
against the broken code**. Every push in that version used a unique string, so
every frame emitted a literal and no frame ever referenced an index: the
corruption was real but latent. Making every push share one string -- first
frame defines, all others reference -- turned a silent bug into an immediate
failure. A concurrency test that cannot fail is worse than no test, because it
is read as evidence.

### Closing must never wait for the peer

A graceful websocket close writes a close frame and then blocks until the peer
echoes one. Both paths closed a socket *because it had stopped reading*, which
is exactly the condition under which no echo ever arrives, so every one of
those closes ran to the library's five-second timeout -- on the caller's
goroutine, inside a loop.

| path | caller | before | after |
|---|---|---|---|
| chat, one wedged socket | sender's HTTP request | 5.0 s | 512 µs |
| chat, four wedged tabs | sender's HTTP request | 20.0 s | 535 µs |
| meeting fan-out, four wedged | announcing goroutine | 20.0 s | 1.06 ms |
| gateway shutdown, 8 participants | shutdown | ~40 s | 0 s |

The meeting case is the worst of them: `announce` and `Room.Broadcast` walk
every participant, so roster updates, speaker changes and SDP for an entire
meeting stalled behind one participant whose laptop had slept. Server shutdown
was worse still, holding the gateway lock while it waited -- a node with a
hundred participants would have taken over eight minutes to exit, well past any
orchestrator's grace period, at which point it is killed and unflushed audit
events go with it.

Teardown is now immediate. The cost is that the peer sees an abnormal closure
rather than a status code, which costs nothing today because neither client
reads close codes; both reconnect with a widening backoff regardless. If a code
ever must reach the client, the fix is to run the graceful close on the
socket's **own** goroutine, never the caller's.

### The rest

- **`audit.Service.Stop` panicked on a second call** -- a bare
  `close(s.stop)`. Shutdown is the worst place in a program to panic: it runs
  while the process is already going down, often on an error path, and it
  aborts the remaining teardown, here the flush of unwritten audit events. The
  package's own test helper already registers `Stop` as cleanup, so any test
  that also stopped explicitly took the package down with it. Now `sync.Once`.
- **`audit.RecordAsync` after `Stop` lost events silently** -- queued into a
  channel with no drain goroutine left. It now writes synchronously, which is
  slower and does not lose the one kind of data whose disappearance must never
  be quiet.
- **`sfu.Registry.GetOrCreate` after `Close` started an unowned goroutine** --
  an allocation loop nothing held a reference to and nothing would ever stop.
  Reachable in ordinary shutdown, since modules stop while the HTTP server is
  still draining. It now returns nil and the gateway answers "server shutting
  down".
- **`sfu.Router.Start` raced `Stop` on `cancel`** -- written without the lock
  it was read under, and a second `Start` would orphan the first loop. Both are
  now under `r.mu` and idempotent.

### What is asserted now

Across `internal/signal`, `internal/modules/messaging`, `internal/modules/audit`
and `internal/sfu`: double close, concurrent close, close racing a write, close
of something never started, send after close, write-pump exit on both context
cancellation and peer death, queue overflow bounded rather than buffered, timer
cancellation on close, and goroutine-count return to a settled baseline after
every teardown. All under `-race`.

The leak checks measure against a *settled* baseline rather than a single
sample, which was itself a correction: the WebRTC media tests leave goroutines
winding down for a while after they finish, and a baseline taken immediately is
inflated enough to make every assertion measured against it vacuous.

---

## 4c. Remote control

Two packages. `internal/rc` is the input codec, the platform injection and the
safety rules that keep a machine usable; `internal/modules/control` is the
consent lifecycle. They are separate because they answer different questions:
*how* input reaches a machine, and *whether it may*.

### The consent model

Six rules, stated in full in `internal/rc/doc.go`. The three that constrain
the code most:

**Only the person at the machine can grant control.** No role can do it for
them — an administrator is refused, and the refusal is itself audited, because
an admin attempting to take control of somebody's machine is exactly what a
security review needs to see. `TestAdminCannotGrantControl` is the
load-bearing test of the feature: every remote access product that became a
liability got there by having an authorisation path that did not end at the
person sitting in front of the machine.

**Granting and revoking are deliberately asymmetric.** An administrator *can*
kill a session. Being able to stop control is safe; being able to start it is
not. The subject may always revoke, alone, and nothing in that path consults
the controller or the network.

**View and control are different grants.** Escalating from watching to driving
does not upgrade the session — it ends it and opens a fresh request, so the
subject answers a new question rather than having something they already
accepted quietly acquire the ability to type on their machine.

Grants expire on their own, capped at 30 minutes whatever was asked for; an
expired grant cannot be extended back to life, only requested again. Every
transition — request, grant, denial, escalation, extension, expiry, revocation
— is on the tamper-evident chain with both identities, because "who did this"
has two answers for remote control and both matter.

### Nothing is ever left held down

The single most important function in `internal/rc` is `releaseAllLocked`, and
the reason is that its failure is invisible and unbounded. If a session ends
mid-drag — network dropped, grant expired, user revoked — the button or
modifier stays physically down as far as the operating system is concerned. A
stuck Alt makes the keyboard useless. A stuck mouse button turns every
subsequent movement into a drag. The person at the machine has no way to know
why, and the instinct that would fix it is the one thing they will not think
to try.

So every path that stops injection goes through it, errors are pushed past
rather than returned (a partial release is worse than none), and it is safe to
call repeatedly. `TestNothingIsEverLeftHeldDown` runs 3,000 random sessions
ended at random by revoke, pause or expiry, and asserts against what the
**operating system** would still consider pressed rather than against the
session's own bookkeeping — a counter consistent with itself proves nothing.

Two details that follow from the same instinct: a click carries its own
absolute coordinates and the injector is moved there first, so a click never
lands where a lost move left the cursor; and pausing for local user activity
releases everything first, so the person who just grabbed their own mouse does
not inherit whatever the operator was holding.

### Why input is not optimised for size

The rest of this system counts bytes carefully. This one deliberately does
not, and the contrast is the point. Screen frames are idempotent — a wrong
frame paints briefly and the next repairs it. Input is the opposite: a click
at the wrong coordinates is an irreversible action taken as the user. There is
no repair frame for a confirmed dialog.

So every event carries absolute coordinates rather than a delta. Measured, a
minute of active control is 12,096 bytes of payload, 1.6 kbit/s — **0.85% of
the screen feed for the same minute**. Certainty about where clicks land costs
under one percent.

| event | bytes |
|---|---|
| mouse move | 5 |
| mouse button | 6 |
| mouse wheel | 9 |
| key | 4 |
| text, one character | 3 |
| release everything | 1 |

Against the same events as JSON: 8.0× smaller.

### What it costs on the wire, measured

`tools/rccheck.sh` captures a real session with tshark and accounts for every
layer. The payload numbers above are not what the user pays for:

```
20 s of active remote control, 15 ms window, loopback
  link framing            7,076 B    8.0%
  IP headers             35,380 B   40.1%
  TCP headers            35,404 B   40.1%
  WebSocket framing       5,644 B    6.4%
  input payload           4,800 B    5.4%
  total on the wire      88,304 B
```

**5.4% payload efficiency.** A 5-byte mouse move rides roughly 90 bytes on the
wire, and no amount of encoding work changes that: 80 of those bytes are IP
and TCP headers. The cost of input is set by how often it is sent, not by how
much is in it, and the relationship is linear:

| window | on the wire |
|---|---|
| 15 ms | 35.3 kbit/s |
| 30 ms | 17.6 kbit/s |
| 45 ms | **12.0 kbit/s** |
| 60 ms | 9.1 kbit/s |

Sharing the connection with the screen stream was measured too, on the theory
that an input message could ride a TCP segment already being sent to
acknowledge incoming screen data:

| | on the wire |
|---|---|
| screen alone, no input | 517,870 B |
| screen + input, one socket | 604,972 B |
| **incremental cost of input** | **87,102 B** |
| input alone, own socket | 88,304 B |

It saves 1.4%. On loopback the acknowledgement is immediate so there is nothing
to piggyback on; a real network with delayed acknowledgement would do better,
but it is not a lever worth designing around.

So the window is chosen against latency instead. `rc.InputWindow` returns
**15 ms while a button is held** — a drag is the most latency-sensitive thing
in remote control, where delay shows as the object trailing the cursor — and
**45 ms otherwise**, which is 22 cursor updates a second, reads as smooth, and
measured **2.95× cheaper**: 12.0 kbit/s against 35.3.

One correction worth recording: the first version of the shared-socket
benchmark reported 175 kB on the wire for 480 kB of screen payload, which is
impossible. The client was never reading, so the server's writes stalled in the
socket buffer and the capture measured a wedged connection. The number being
arithmetically impossible was the giveaway; a plausible wrong number would have
shipped.

### What SendInput cannot do

Injection is `SendInput`, which enters the same queue as real hardware, so
applications cannot distinguish it. It deliberately cannot send Ctrl+Alt+Del
or interact with the secure desktop — UAC prompts, the lock screen, the login
screen — which are guarded by the Secure Attention Sequence and unreachable
from any user-mode process by design. Reaching them needs a SYSTEM service in
session 0, which is a much larger attack surface and not something to add
casually to a meeting product. This will arrive as a bug report; it is a
property, not a defect.

Text is carried as literal UTF-16 through `KEYEVENTF_UNICODE` as well as by
virtual key code, because a scancode means "the key in this physical
position": an operator on a Danish layout sending scancodes to a US-layout
machine types the wrong letters, and no care at the sending end fixes it.
Arrow keys, Home, Delete and the right-hand modifiers carry
`KEYEVENTF_EXTENDEDKEY` or they arrive as their numeric-keypad twins, which is
the most common bug in hand-rolled Windows injection.

### Still missing

The agent binary that runs on the controlled machine, and the client UI for
requesting, granting and revoking. The server-side lifecycle, the codec, the
injection and the safety rules are complete and tested.

---

## 5. Security

- **Argon2id** at OWASP parameters, PHC-encoded so cost can be raised without
  invalidating existing hashes.
- **Server-side sessions**, `HttpOnly` cookie, only the SHA-256 of the token
  stored. A database dump does not hand over live sessions.
- **CSRF** by double-submit: the companion cookie is deliberately readable so
  the page can echo it in a header — a cross-origin attacker can make the
  browser *send* cookies but never *read* them.
- **Account enumeration is closed** on both login and signup. Every failure
  returns one identical message, and an unknown address still pays the full
  argon2 verification so the timing matches. The true reason goes to the
  audit log.
- **Authorisation is one readable table** — four roles, an explicit
  permission map, every check resolved server-side on every request.
- **Privilege changes evict immediately.** Role change, suspension, and
  password change all revoke every session for that user.

### The audit log is tamper-evident

Every entry carries the SHA-256 of the one before it. Three distinct attacks,
all detected by `Verify`:

| attack | detected because |
|---|---|
| edit a row | contents no longer hash to the stored value |
| delete a row | the next entry links to a hash that is gone |
| edit **and** recompute that row's own hash | the *next* entry still commits to the old value |

The third is the one that matters: forging requires rewriting every
subsequent row, which is what makes an externally pinned head hash worth
having. Field encoding is length-prefixed, not delimiter-joined, so an
attacker-controlled field cannot be used to forge a collision.

---

### 5a. Penetration testing, and what it actually found

Probed against a running server rather than read off the source, because the
question is what the deployed thing does.

**Fixed: the origin check was off by default.** `-dev` defaulted to `true`, and
development mode sets `OriginPatterns` to `"*"`, which disables the WebSocket
origin check entirely. A flag that must be remembered in order to be safe will
eventually not be, so the default is now `false` and development mode logs a
warning when it disables the check. Running locally never needed it: with no
patterns configured the check requires `Origin` to match `Host`, which it does
when the page and the socket come from the same server.

Demonstrated both ways, with a real session cookie and a forged `Origin` header:

| | `-dev=true` (old default) | `-dev=false` (new default) |
|---|---|---|
| `/rtc`, `Origin: http://evil.example` | **ACCEPTED (101)** | REJECTED (403) |
| `/ws`, `Origin: http://evil.example` | **ACCEPTED (101)** | REJECTED (403) |
| either, same origin | ACCEPTED (101) | ACCEPTED (101) |
| either, lookalike host `…:9443.evil.example` | ACCEPTED (101) | REJECTED (403) |
| either, no `Origin` header | ACCEPTED (101) | ACCEPTED (101) |

Accepting a handshake with no `Origin` at all is deliberate: only a browser
sends one, and only a browser can be made to mount this attack. A non-browser
client with a stolen cookie is a different problem that an origin check was
never going to solve.

Honest severity: this was defence in depth rather than a live hole. Session
cookies are `SameSite=Lax`, and browsers do not attach Lax cookies to a
cross-site WebSocket handshake, so the forged-origin request would have
arrived unauthenticated and been refused at the session check. The test above
attaches the cookie explicitly, which a real attacker's page cannot do. It is
still worth fixing, because a defence that only works because a *different*
defence is also present is not one you get to keep -- and `-secure-cookies`
is itself off by default.

**Probed, no finding:**

- *Path traversal on the static server.* Seven encodings tried against `/`
  (`../`, `..%2f`, `%2e%2e/`, `....//`, backslash separators, and a traversal
  nested under a real prefix). Every one cleaned to a rooted path and returned
  404; none served a byte outside the web directory. `http.Dir` rejects paths
  containing the OS separator and `http.FileServer` cleans before opening.
- *Session fixation.* Login always mints a fresh 256-bit token server-side and
  stores only its SHA-256. No session identifier is ever accepted from the
  client, so there is nothing to fixate. (A pre-existing session is not
  invalidated on re-login, which is session accumulation, not fixation.)
- *Privilege escalation, member to admin.* `PATCH /users/{id}/role` is guarded
  at the route by `PermUserWrite` and again in `SetRole`, behind CSRF. Changing
  your own role is refused outright, no actor may grant a role outranking their
  own or act on someone above them, and the last administrator cannot be
  demoted.
- *Unauthenticated API access.* `/api/identity/users` and `/api/settings`
  return 401 with no session.
- *Security headers.* CSP is `default-src 'self'` with `connect-src 'self'`,
  `frame-ancestors 'none'`, `object-src 'none'`; plus `X-Content-Type-Options`,
  `X-Frame-Options: DENY`, `Referrer-Policy`, and a `Permissions-Policy`
  scoping camera, microphone and display-capture to `self`. HSTS is correctly
  gated on the same flag as Secure cookies, since sending it over plain HTTP
  is at best ignored.

**Not tested, and therefore not claimed:** TURN credential handling (not
deployed), the media path itself beyond DTLS-SRTP defaults, denial of service
under adversarial load, and anything requiring more than one account colluding.

## 6. Measuring

```bash
# Start the server, then:
node tools/bitcheck.mjs --mode interop   # worst case, one frame per message
node tools/bitcheck.mjs --mode burst     # realistic, coalesced

go run ./cmd/bitbudget -n 4 -minutes 15  # the analytic model
go test ./...                            # 67 tests
```

`bitcheck` captures real packets with tshark, decodes them with the same
`wire.js` the browser ships, and reports the per-layer breakdown. It exits
non-zero if it captured nothing — a green run that measured zero packets is
worse than a red one.

---

## 6a. Returning to a backgrounded tab

A hidden tab reports every tile invisible, and the allocator stops sending
video to it. Audio is deliberately exempt (`sfu/router.go`: audio is not
layered and is not subject to the video budget), so a backgrounded tab goes
quiet visually and keeps hearing the meeting.

Coming back was the problem. The allocator resumes immediately, but the
forwarder holds every packet until a keyframe arrives, and an encoder nobody
has asked produces its next one whenever it happens to. The tiles stayed black
long enough to read as the video having broken. The client now asks, on
`visibilitychange` back to visible, for a keyframe on every track it is
watching.

Measured with tshark on the real connection, four subscribed tracks, Npcap
loopback (DLT_NULL, IPv4 -- 4 + 20 + 20 = 44 bytes of framing, not the IPv6 40
that applies elsewhere in this document):

| layer | bytes |
|---|---|
| loopback framing (DLT_NULL) | 4 |
| IPv4 | 20 |
| TCP | 20 |
| WebSocket header + client mask | 6 |
| protocol payload | 37 |
| **total on the wire** | **87** |

The payload decomposes exactly: 1 byte of `OpBatch` header, a 23-byte viewport
report with its 1-byte length prefix, and four keyframe requests of 2 bytes
each (one control byte plus a one-byte interned track id) with a length prefix
apiece -- 1 + 24 + 12 = 37.

**Payload efficiency 42.5%** (37/87). The keyframe requests themselves cost 12
bytes and, because they are queued in the same 15 ms coalescing window as the
viewport report the return already triggers, they add **no packet at all**.

The comparison that matters is against sending them uncoalesced, which is what
a per-message client would do. Each frame then pays its own 44 bytes of headers
and 6 of WebSocket framing: the viewport alone measures 73 bytes on the wire
(observed: `tcp.len == 29`), and each keyframe request 52. Five packets, 281
bytes, **11.0% payload efficiency**. Coalesced it is one packet, 87 bytes,
42.5% -- a 69.0% reduction for the same information.

That is the same lever as the 27.1%-versus-48.0% result recorded above, and it
is worth repeating that the encoding is not what earns it. Two-byte keyframe
requests are dense; five of them sent separately would still have been 96%
header. Coalescing is the decisive one.

What is not measured: the recovery latency itself. The request reaches the
publisher as a PLI and the keyframe that follows is a full intra frame, which
is expensive and is not captured in the numbers above. What is claimed here is
the cost of asking, not the cost of the answer.

## 6b. Audio, and why it sounded thin

Reported as "audio poor". The cause was that Opus was never configured at all:
the browser was left to negotiate, which lands at roughly 32 kbps mono. That is
enough to be understood and audibly thin for a voice somebody listens to for an
hour. Two changes: an explicit `maxaveragebitrate=64000` with `useinbandfec=1`
on every Opus fmtp line in both the publisher offer and the subscriber answer,
and `maxBitrate` on the audio sender's encoding -- the fmtp line is a hint to
the far end, the sender parameter is what actually governs this browser's
encoder.

A second, quieter bug in the same path: choosing a specific microphone built
its constraints as `{deviceId: {exact: ...}}` and named none of the processing
flags, so selecting a headset could sound worse than not choosing one, with
nothing in the interface to explain why. Echo cancellation, noise suppression
and gain control are now always named.

Measured with tshark, two participants, cameras off so every RTP packet on the
wire is speech, 15 s capture on Npcap loopback:

| | measured |
|---|---|
| packet rate | 54.3 pps (50 pps Opus at 20 ms + RTCP) |
| dominant packet size | 227 bytes on the wire |
| loopback + IPv4 + UDP framing | 32 bytes |
| SRTP (RTP header, extensions, payload, auth tag) | 195 bytes |
| total for the direction | 94.0 kbps |
| Opus target | 64 kbps |

**Payload efficiency 68.1%** (64 kbps of Opus in 94.0 kbps on the wire). The
remaining 32% is per-packet header, and unlike the signalling plane it cannot
be coalesced away: audio is one packet per 20 ms frame, and batching two frames
per packet would halve the header cost while adding 20 ms to every word. For
speech that is the wrong trade, and it is the one place in this system where
"no wasted packets" loses to something more important.

What this does not claim: that 64 kbps is the right number rather than merely a
much better one than 32. It was chosen because it is transparent for speech,
not because 96 or 128 was measured against it and rejected.

DTX was considered and not enabled, which is a deliberate exception to "idle
must cost zero". Opus DTX clips the onset of speech after silence, and the
defect being fixed here was that the audio sounded bad. The saving is real and
unmeasured; enabling it belongs with a capture of what it costs at the start of
every sentence.

## 6c. Meeting controls, and the half that was missing

A host decides per meeting what attendees may do: unmute, turn on video,
present, use chat. Four booleans, held in `room.Permissions`, changed with
`OpPermissions` (2 bytes of opcode plus four packed bits) and broadcast to
everybody.

Enforcement is the server's and only the server's. `room.May()` answers the
question at the moment of the action; `Router.Permit` consults it on every
forwarded track, `Router.ApplyPermissions()` re-checks the tracks already
flowing, and the gateway refuses `OpChat` outright. A host is exempt from
every control, or a host who turned everything off would have no way to reach
the switch again. An unrecognised capability is refused rather than allowed, so
a typo in a caller cannot quietly open something up.

What was missing was the client's half, and it was reported as a bug: "if admin
closed attendees can turn on video then how my video is on". The server was
behaving correctly -- it had stopped forwarding the track the instant the
switch was flipped, and nobody in the meeting could see the camera. The client
had only greyed the button out. The camera stayed on, the local preview kept
playing, and the uplink kept carrying a stream with no audience. Every signal
available to the person said it was working.

That is worth stating as a rule, because it is not obvious: **disabling a
control is not the same as stopping what it started.** Greying a button out
describes what it will do next time. It says nothing about what is already
running, and what is already running is what the person can see.

Chat had the same shape with a different ending. The server refused the frame,
the composer accepted the text and cleared itself, and nothing ever appeared --
a message that vanishes with no explanation anywhere. The composer now says
why.

The client's rule lives in `web/src/permissions.js`, deliberately split from
the code that acts on it, mirroring the way `room.May()` is separate from the
router that consults it. It fails closed: an absent or malformed permission
counts as refused. That direction is the recoverable one -- an unnecessary stop
is visible and one click from undone, while carrying on sending into a server
that is dropping it is silent and looks exactly like success.

Tested end to end with two real participants rather than only in unit tests:
the host revokes video, the attendee's camera stops, the button disables with
a reason, clicking it again does nothing, and the host's tab drops from one
remote video track to zero. Restoring the permission lets the attendee turn it
back on. `web/src/permissions.test.mjs` covers the rule, and -- per the
standing requirement that a test which has never failed is not evidence -- it
was run against the original behaviour, where the answer was always "stop
nothing". Four of its cases fail there, including the named regression.

## 6d. Making a LAN meeting actually connect

Everything above assumed the media reached the other side. On a real network
between two machines it did not, and the failures were a chain -- each one
uncovered by fixing the last, each captured with `internal/sfu` ICE logging
and tshark rather than guessed. Recorded here in the order they were found.

**1. ICE chose the WSL virtual adapter.** A development laptop has many
interfaces -- the real Wi-Fi, a Hyper-V/WSL switch on 172.24.64.1, several
unplugged NICs holding 169.254 link-local addresses. pion offered a host
candidate for every one, and ICE, preferring whatever connected first, picked
172.24.64.1 for a phone on Wi-Fi. That subnet exists only inside the laptop.
`AddressPolicy` (netpolicy.go) now restricts candidates to the interface the OS
routes to the internet through, found by opening a UDP socket to a TEST-NET
address and reading back the local address the routing table chose.

**2. ICE then chose IPv6.** With the virtual adapter gone, the next-fastest
candidate was the interface's global IPv6 -- a `2409:40d4::` carrier prefix on
a phone hotspot, where every device has its own carrier-routed address and none
can reach another's. Media is now IPv4-only.

**3. The server used STUN.** An SFU knows where it is; only clients need STUN.
Given a STUN server anyway, the server gathered server-reflexive candidates
from ephemeral ports, and two browsers on one laptop connected by hairpinning
out through the college NAT and back -- unreliable, and never touching the
fixed media port. The server now uses no ICE servers; `-stun` is for clients.

**4. Media used a random port.** This is the one that matters for "no
configuration". WebRTC opens a fresh random UDP port per connection by default,
and a firewall cannot allow a port nobody can name. Captured: a participant
exchanged 44 kB of TCP signalling and sent zero UDP packets that reached the
host. All media now rides ONE fixed port -- default 8801, Zoom's, already open
on many networks -- via pion's UDP mux, with a TCP listener on the same port as
fallback for networks that drop UDP entirely (transport.go). One port, both
protocols, is the whole firewall story.

The bind widens loopback deliberately: a browser never offers a 127.0.0.1
candidate, so a server bound only to loopback can never pair with one. A
`-addr 127.0.0.1` server therefore still binds media to every interface, and
announces only the default-route address the policy allows -- one socket per
allowed address, because a wildcard mux announces every interface and brings
the virtual adapters straight back.

**5. Audio was never forwarded.** The forwarder refused to begin any stream on
a non-keyframe -- correct for video, whose decoder needs a reference frame, and
fatal for audio, which has no keyframes. Every audio packet was refused at the
start gate; no participant ever heard another, while signalling, ICE and DTLS
all reported success. Audio now uses a forwarder that starts on its first
packet (`NewAudioForwarder`). Confirmed on the wire: 1000 Opus packets (PT 111)
per subscriber where there had been none.

**6. The first subscriber offer raced its own transport.** The gateway set the
Peer's `SendSDP`/`SendICE` callbacks *after* `AddPeer`, but AddPeer subscribes
the newcomer to every existing track, and each subscription fires a
renegotiation immediately. The first subscriber offer went into a nil callback
and vanished; the negotiation flag stayed set, nothing retried. Signalling is
now passed *into* `AddPeer` (`sfu.Signalling`), attached before any
subscription can fire, and a subscriber offer with no signalling attached is a
logged error that clears the flag rather than a silent permanent wedge.

**What is not validated here.** Media *playback* was not confirmed through the
browser-automation harness, which cannot: its tab group is permanently
`document.hidden`, so viewport gating correctly suppresses video, and its tabs
have no real microphone, so audio is silent at the source. What is proven is
that the correct bytes leave the server on the named port with the negotiated
SSRCs -- verified with tshark. End-to-end playback needs two real devices, and
that test belongs to whoever has them.

## 6e. The relay, for networks that block UDP

The fixed UDP port made calls work when UDP got through. On a campus network it
did not always get through: a wire capture showed both browsers offering ONLY
TCP ICE candidates and no UDP ones at all -- Chrome's behaviour when it cannot
get a UDP packet out. With no relay, TCP-only clients have nothing to pair with
and every call fails, silently, after the ICE timeout.

This is the case TURN exists for, and it is what Zoom and Teams do: media tries
UDP first (Zoom 8801, Teams 3478-3481) and falls back to a relay over TCP/TLS
443 when UDP is blocked. Teams' "Transport Relays" bridge TCP to UDP, which is
a TURN server by another name. Aether now embeds one (`internal/relay`, on
pion/turn):

- **UDP 3478, TCP 3478, TCP 443.** 443 is the one that matters -- almost never
  blocked, because blocking it breaks the web. Plain TCP, not TLS: a self-signed
  certificate fails the browser's validation of a `turns:` server, and the
  networks in question block UDP, not TCP, so TLS would cost the connection for
  no security gain. With a real certificate this should become TLS.
- **Ephemeral credentials.** Each config fetch mints a fresh
  HMAC-SHA1(secret, expiry) username/password (the TURN REST scheme), valid 12
  hours. The secret is random per process and never leaves the server; a
  relay address that leaks is worthless once it expires.
- **The public IP is the default-route address**, the same one the SFU
  announces -- not `LANAddresses()[0]`, which returned the WSL virtual adapter
  and pointed every relay URL at a subnet no other device can reach. That was a
  real bug, caught and fixed before it shipped, and it has a test.

The client fetches these from `/api/identity/config` on join and passes them to
every `RTCPeerConnection` with `iceTransportPolicy: 'all'` -- so a direct UDP
pair is still preferred and the relay is used only when no direct path forms.
On a network that works, the relay costs nothing.

**ICE restart** was added alongside it. A connection that reaches `failed`
re-runs ICE (`createOffer({iceRestart: true})` on the publisher; a keyframe and
viewport nudge on the server-offered subscriber), which re-gathers candidates
-- now including relay candidates that a first, direct-only attempt never tried.
This is what lets a call recover after a tab is backgrounded long enough for the
network to drop the binding, or after a Wi-Fi handover, instead of going dark
permanently. `disconnected` is given a few seconds to recover on its own before
a restart is forced, because a restart is disruptive and most blips heal
themselves.

**Not validated here.** The relay path itself needs a genuinely UDP-blocking
network between two machines to exercise -- the automation harness has neither.
What is tested: that it binds, that its URLs point at a reachable address, and
that the credential it advertises is the signed, time-limited one its own auth
handler will accept (recomputed independently, and shown to fail if the secret
is ignored). Whether the relay actually carries media when UDP is blocked is a
test that belongs to the two real devices on the real network.

## 6f. Media over the WebSocket: the guaranteed path

Three tests running, on the campus network, WebRTC did not connect between two
real machines. Not for want of a path: the remote browser reached the TURN
relay on all three transports including TCP/443, offered relay candidates, and
its ICE sat in "checking" until the participant gave up. The pion relay-only
test passes on the same code. Whatever Chrome could not pair on that network,
it could not be observed from the server, and three rounds of inference did
not find it.

Meanwhile the WebSocket never failed once. Chat, signalling, and the TURN
allocations themselves all rode TCP to the server and all succeeded, every
time. A WebSocket rides the HTTPS connection to the site; it cannot be blocked
without blocking the site. Zoom's web client shipped media over WebSockets
for exactly this reason. So Aether now carries media over its own signalling
socket when WebRTC cannot (`web/src/wsmedia.js`, `OpMediaFrame` 0x116).

**How.** WebCodecs -- Chrome's hardware encoder/decoder -- not MediaRecorder,
which is a file container with seconds of latency. VP8 at 600 kbps with a
forced keyframe every 2 s; Opus at 48 kbps. One WebSocket frame per encoded
chunk, batched by the existing 15 ms coalescer like every other frame. The
server never decodes: it reads the kind to apply the meeting's controls (a
fallback that bypassed "attendees may not share" would make the control a
lie), prefixes the sender's interned id, and fans out to everyone else -- the
same path chat takes. Decoded video is drawn to a canvas and captured back as
a MediaStream; decoded audio goes through Web Audio into a
MediaStreamDestination. The result is an ordinary MediaStream, so every tile,
stage, mirror and audio-sink path renders it with no knowledge of which
transport fed it.

**Rules it obeys.** Every keyframe is self-contained and one is forced every
2 s, and on every roster join: a late arrival or a dropped frame costs at most
two seconds of waiting, never a repaint -- the standing rule against expensive
recovery of cross-frame state. The encoder's queue is bounded at four chunks;
past that, whole frames are dropped, never degraded. The socket read limit
was raised from 64 KB to 1 MB because a VP8 keyframe is routinely 50-150 KB
and the first one would have closed the connection with a size error -- still
a bound, and the encoder is configured far below it.

**When.** Forced by `?wsmedia=1`. Otherwise armed on join: if the WebRTC
subscriber leg has not connected within 8 s, or either leg reaches `failed`,
the client switches. Checked against live state at fire time, so a connection
that comes up in the meantime cancels it. WebRTC stays the fast path -- lower
latency, no head-of-line blocking -- on every network where it works.

**Tested.** The frame layout round-trips through the real Go encoder and
decoder for every kind/key combination and a 150 KB keyframe, and the test
fails on a one-bit slip in the decoder (verified). The Go and JS opcode
tables are held equal by the existing parity test. Full suite under -race.

**Not yet measured.** The bit-level cost of this path on the wire, per the
standing rule, awaits a live session on the hostile network -- which is the
only place it can be measured honestly, and the only place it matters.
End-to-end playback likewise: the automation harness cannot pass a self-signed
certificate and has no microphone. That test belongs to two real devices.

## 6g. After the first working call: what broke next, and what was added

The first two-device call to actually carry audio surfaced the next layer.

**The fallback tripped on an idle subscriber.** Joining alone, the subscriber
connection has nothing to receive, never negotiates, and sits at ICE `new`
forever -- and the 8 s fallback timer read that as failure and moved a
perfectly good WebRTC session onto the slow path. That is why the host's
screen share "buffered": it was riding the fallback at camera bitrate for no
reason. The trigger now requires evidence: the publisher (always negotiated)
reaching `failed`, or a subscriber that was actually *offered* a description
and still did not connect. Alone in a room is not a network problem.

**"X is presenting" outlived the presentation.** The wire omits `screenTrack`
when `sharing` is false (to keep the 3-byte state delta), so the remote's copy
of the old id survived, and the still-open, muted screen track fell through
the tile builder's `else` and became the presenter's *camera* -- a frozen
screen shown as their face. Fixed where sharing flips off: the known screen
track is forgotten and the builder ignores any track that is not live.

**Screen share had no fallback path at all.** `startShare` fed WebRTC only.
It now also feeds the WebSocket sender when that is the path in use, flagged
in a spare bit so the receiver draws it to its own surface and the server
applies the "share" control (not "camera"); the forwarder now passes spare
bits through instead of zeroing them. Screen gets 2.5 Mbps to the camera's
600 kbps: on the TCP path bandwidth is not the constraint, legibility is.

**The ledger panel is gone.** The counters stay -- the wire measurements in
this document depend on them -- but the panel was engineering telemetry in a
product surface.

**Recent meetings** (`scheduling_attendance`, §6 of the schema). The calendar
knows what was scheduled; it did not know what happened. The gateway now
records one row per account per visit, opened on join and closed on leave
however the socket ends, and the dashboard's "recent" is built from that:
ad-hoc rooms included, one entry per room, one click to rejoin. Guests leave
no row; there is no account to show it to.

**Calling.** A ring on the persistent chat socket, the way Teams does it:
every device the callee is signed in on shows a dialog, plays a tone, and
raises a system notification if the tab is in the background. Accept opens the
room; decline tells the caller; thirty seconds unanswered is a missed call on
both sides. Rings are delivered only between accounts that already share a
direct conversation -- the contact list's own rule -- because a ring from a
stranger is a nuisance vector. Two opcodes (`OpRing` 0x117, `OpRingAnswer`
0x118); the meeting code never learns about it, chat.js hands rings to the app
as DOM events.

**Scheduling** was working server-side all along (verified with a direct POST:
201, listed). The dialog was hardened anyway: a submit guard against double
presses, empty times fall back to the suggested slot rather than refusing, the
server's exact error is shown, and both home and calendar repaint after.

**Tested.** Attendance on a real SQLite file (`attendance_test.go`): a visit
opens and closes, guests leave no row, closing twice is idempotent, history is
per-account and most-recent-first, and a scheduled meeting's title resolves for
its room. Calling at the hub with real sockets (`ring_test.go`): a ring reaches
every device the callee holds and nobody else's, and the answer reaches the
caller -- shown to fail when the push is mis-targeted (the callee's devices
receive nothing). Full suite under -race. **Not yet validated on two devices:**
the fallback trigger's new condition, WebSocket screen share, calling, and
recent. The harness cannot pass a self-signed certificate; those tests are the
user's.

## 6h. The WebSocket is the default transport, not the fallback

Every two-device test on the user's networks -- a campus LAN, then a phone
hotspot -- ended the same way: the remote's WebRTC publisher failed to form a
pair, even on the same /24 as the server, even with relay candidates offered,
while its subscriber connected and the WebSocket never dropped once. Starting
on WebRTC and switching when it failed therefore meant switching on nearly
every call, and the switch is where the glitches lived: a black tile while ICE
decided, then audio torn as two transports briefly overlapped.

The user's requirement was put plainly: failover must be near-impossible.
The only way to make a transition near-impossible is to not need one. So the
WebSocket carries media from the first frame by default. The WebRTC plane is
constructed but never started -- no ICE, no relay allocation, no publisher
that can fail -- and there is nothing to fail over from. `?media=webrtc` opts
a session back into WebRTC-first with the automatic fallback, for a network
that is known to carry UDP.

What this costs: latency, against a working UDP path (TCP head-of-line
blocking under loss), and some server CPU (the socket path fans frames out per
recipient with per-connection interning, where the SFU forwards RTP). What it
buys is the property that was asked for. When WebRTC is made to work on these
networks -- the remaining mystery is why a client-offered connection fails
where a server-offered one succeeds on the same subnet -- the default can flip
back on evidence, the same way it flipped forward.

Alongside: the handover, when it does happen in WebRTC mode, is now clean --
the WebRTC publisher is abandoned (senders emptied, no ICE restart) so exactly
one transport carries the outbound; the fallback fires at 6 s rather than
waiting ~30 s for ICE to say "failed"; and WebSocket audio sits behind a 150 ms
jitter buffer that resyncs only on a real gap, in place of a scheduler that
clicked on every burst.

## 6i. Calls as conversation, not as meetings

A ring is part of a conversation, so it is logged in messaging
(`messaging_calls`, migration 0027) and shown in the thread between the two
people -- direction, kind, outcome and length, in time order with their
messages, the way a phone does it. It is deliberately excluded from the
dashboard's "recent" meetings: a call log in a meeting history reads as
clutter. Duration is read at query time from the room's attendance rows (first
join to last leave), a cross-table read inside one SQLite file rather than a
second copy of the fact. Audio calls are tagged by suffixing the ring's room
with `?audio=1`; the stored room is the clean one so it matches attendance,
and both ends start camera-off.

The call control moved to the right of the thread header as a video icon with
a menu (video / audio), where every other messaging product puts it. A live
meeting's participants tab gained "Add people": the direct-conversation
contacts not already present, one button to ring each into the current room.
Recent shows five.

Backpressure was the fix behind "my voice is not going" and a 6-7 s screen
lag: the WebSocket send buffer had no bound, so video at 2.5 Mbps on a link
carrying 1 Mbps queued megabytes ahead of every audio chunk. Video now drops
whole frames past a pending-bytes threshold (screen first, camera second) and
forces a keyframe after; audio is never dropped. The link readout in the
header -- up, down, transport, and queued bytes when the socket is backed up
-- was restored from the same counters the wire measurements use.

## 6j. An invitation is the permission, and a meeting ends when the room empties

Two separate corrections to the same mistake: letting the calendar decide
something the room already knows.

**The invitation.** A scheduled meeting with `knock` admission used to make
everybody wait, invitees included, so the organiser had to admit people they
had personally named a week earlier -- and if the organiser was late, nobody
got in at all. The organiser already decided who was coming at the moment they
were actually thinking about it. `scheduling.Service.InvitedToRoom` answers
organiser-or-invitee in one indexed SQL query and the join path takes it as
permission (`signal.Gateway.Invited`). Three properties, each with a test:

- It is checked **after** the lock. A lock is a door the host closed a moment
  ago; it outranks a list written last week.
- It matches on address, case-insensitively, and on account id. Invitations
  name addresses, never accounts -- `normaliseInvitees` drops the id
  deliberately, because you invite people who have not signed up yet -- so the
  join path hands over both and the address is what matches. A test documents
  this rather than asserting the id alone works.
- A cancelled meeting invites nobody. The link still resolves and the invitee
  rows are still on disk, and neither is permission to walk into whatever is
  in that room now.

The waiting room keeps its original job: whoever has the link but was never
asked.

**The end of a meeting.** Nothing ever cut a meeting off at its scheduled end,
and nothing does now -- but the dashboard was reading the clock, so a meeting
booked for an hour showed "Ended" at sixty minutes with eight people still
talking, and showed "in progress" for the fifty minutes after everybody left.
Two facts replace the clock: `Registry.Occupancy(room)`, stamped onto every
meeting read as `live`, and `CompleteRoom`, which the gateway calls when the
last participant leaves. `CompleteRoom` only closes a **scheduled** meeting
that has **already started**: people who gather early and disperse have not
held the meeting, and a mis-click at 9 a.m. must not quietly complete the
3 p.m. review. Occupancy outranks the stored status, so somebody rejoining
after the room emptied shows live again. The decision is in `web/src/meeting.js`
so it can be tested without a browser; six cases cover overrunning, completed
inside its window, rejoined-after-completion, cancelled, and the clock-only
fallback for a meeting nobody has reached yet.

**Certificate reissue.** Found while restarting on LAN: `ensureCert` reused any
certificate that existed, so a laptop that changed networks served one naming
yesterday's address. That is not the ordinary self-signed warning -- it is a
name mismatch, which on a colleague's device reads as the server lying about
who it is and is much harder to wave past. It now reissues when the stored
hosts do not cover the machine's current addresses.

## 6k. The meeting chat panel is the permanent chat

The panel in a meeting used to hold one ephemeral log that died with the room.
It now writes to one of two places, chosen in the composer:

- **Everyone** -- the meeting's own broadcast (`OpChat`), which still ends with
  the meeting, because a remark to the room belongs to the room.
- **One person** -- their permanent conversation, the same thread as the
  sidebar, opened by account id and stored on the server. In a two-person call
  this is selected automatically: a call with one other person *is* a
  conversation with that person, not a broadcast to an audience of one.

Delivery for the private half is the chat push socket that was already open,
so an idle conversation still costs zero. A message said to the room while you
are writing to somebody is marked unread on the selector rather than lost.

**What this cost on the wire, measured.** The panel needs an identity to
address, so the roster entry gained the account behind each participant. It
carries the **id, not the address**: broadcasting everyone's email to everyone
in the room, guests included, to make a chat panel work would be a disclosure
the feature does not need, so `OpenDirectByID` resolves id to address on the
server, from an id the server itself put on the wire.

Captured with tshark on the loopback adapter (`\Device\NPF_Loopback`) while
the interop harness ran a real two-participant join against the real server:

| | |
|---|---|
| Server to client, whole join | 912 B on the wire, 406 B of WebSocket payload |
| Payload efficiency | 44.5% |
| Account id, first mention | 17 B (1 B length + a 16 char id) |
| Account id, later mentions | 1 B (per-direction dictionary index) |
| Measured delta, one client's whole join | 193 B in, against 175 B without |

The 18 B delta is 17 + 1 because the harness signs both participants in as the
same account, so the id is interned once. Two distinct accounts cost 34 B. The
honest way to state it: **17 B per distinct participant per meeting**, on join
and leave only. It rides no per-frame path, so idle still costs zero and a
frame of video is unchanged. The 44.5% payload efficiency is in line with the
48.0% previously measured for coalesced control traffic, and far above the
27.1% of one frame per message.

Cross-language parity re-proved by the interop harness (25 assertions, all
passing) after the roster format changed. Two of its assertions were stale and
had been failing before this work: it asserted a client could round-trip its
own `role`, which the server correctly overrides -- so it now asserts the
override -- and it hard-coded a 13 byte batch that has been 14 B since the
screen-track id was added to the state delta, so it now derives the expected
size from the frames and checks the envelope is bounded instead.

## 6l. The people directory, and why it was an email harvester

Found by asking the right question: if this runs somewhere public, whose names
does "add people" show?

**What it was.** `GET /api/identity/directory` was open to any signed-in
account. With no query it returned the twenty most recently created accounts,
**with their email addresses**. With a query it matched `LIKE '%q%'` against
display name and email. So one request for `q=@` returned every account on the
deployment, and paging through fragments returned the rest. On a server
strangers can sign up to -- which is what "globally running" means -- that is
an address book of the entire user base handed to whoever registered most
recently. It survived earlier review because it looks like a feature.

Demonstrated rather than asserted: a mutation that restores the old code makes
`TestNoQueryEnumeratesTheDeployment` print the leak query by query, and against
the running server a freshly signed-up stranger recovered every address with
`q=@`.

**What it is now.** A search reaches two places and no further.

- **People you already share a conversation with.** Browsable and filterable,
  because you can already see all of them. Supplied by
  `messaging.Service.ContactIDs`, injected into identity as a port so identity
  imports neither module.
- **One exact email address, matched in full.** Somebody tells you their
  address and you can reach them, which is how a first conversation starts
  everywhere. It discloses nothing: you had to know the address to type it, and
  a wrong guess and an unregistered address return the same empty result, so
  the directory is not the account-enumeration oracle that the login path was
  hardened against.

There is no query that returns an account you had no way to name, and no way to
page through the deployment.

**Why not scope it by email domain.** The obvious "same organisation" fix was
tried and rejected on this deployment's own data: the five accounts on it span
four domains, two of them gmail.com. Treating an email domain as an
organisation would hide a colleague behind a different domain while making
every gmail.com account in the world mutually discoverable -- the same leak,
with a step in front of it.

**Why a shared meeting is not a connection.** `ContactIDs` is a mutual thread
only. If being in the same meeting counted, one 200-person all-hands would make
every attendee permanently discoverable to every other. Nothing is lost:
people in a meeting already see each other in the roster and can message each
other from the meeting panel, which addresses accounts directly and never
touches the directory.

**The escape hatch.** `directory.scope` in the settings console: `connections`
(default) or `everyone`. `everyone` is a published address book, which is right
for one organisation and wrong for anything the public can reach; the help text
says so. Anything unrecognised resolves to `connections`, matching how
`NormaliseAdmission` fails closed. `DirectoryByIDs` returns nothing for an
empty id set, so a missing or failing contacts port shows an empty directory
rather than the whole one -- tested.

## 6m. Searching for a person, everywhere

The pickers disagreed with each other: the new-conversation picker listed
accounts the moment it opened, "add people" showed a fixed handful with no
search at all, and the schedule dialog was a bare text box. `web/src/peoplesearch.js`
is now the one behaviour, used by all three: results appear as the name is
typed, and nothing is listed before it.

What is in the module is the part worth testing, and it is tested (13 cases):

- **Ranking.** Exact address, then name prefix, then any word of the name
  ("kumar" finds "Ambuj Kumar"), then address prefix, then contains. Ties break
  on name so the list does not reshuffle between keystrokes, which is the thing
  that makes a live list feel broken.
- **Stale-answer rejection.** You type "am", then "ambuj"; the first query is
  slower and its answer lands second. Every run carries a sequence number and
  anything stale is dropped. Tested with deliberately inverted latencies.
- **Debounce.** Three keystrokes are one request.
- **Highlighting.** Escape first, insert the marker after, so somebody named
  `<script>` is shown and not run. Tested.

Each picker searches two sources and merges them: your contacts, ranked in
memory on every keystroke so the list is never empty while a request is out,
and the server for anybody else. A directory that cannot be reached loses the
remote half and keeps the local one.

## 6n. One person, one face; the thread you keep with yourself; emoji

Three reports from one session, and the first two turned out to have the same
root.

**The avatars disagreed with themselves.** The chat sidebar drew a bare
`<span class="avatar">` with no tone attribute at all, so everybody in the
conversation list was the same colour while the same people were individually
coloured in the meeting and on the dashboard. And the letters came from
whatever text a call site happened to hold: a display name here, an address
there, so `7298.divyanshi@gmail.com` reduced to "7D" in one place and
"Divyanshii" to "D" in another, and `ambuj.kumar@ddecor.com` picked up its
domain and became something different again.

Both halves had the same cause -- identity was being inferred from whatever
string was on hand -- so `web/src/avatar.js` now takes the person and derives
both. Colour hashes on a stable key (account id, then address, then name, all
lowercased), never on the visible text. Letters prefer a real name, and from
an address use only the local part, skipping leading digits, so the domain
never contributes and two colleagues who share one are still told apart. Both
local implementations were deleted; there is no `class="avatar"` left in the
client that is not a placeholder or a "+3" counter. Ten tests, including that
the same account described three different ways is one colour, and that a
person named `<img src=x onerror=1>` is escaped.

A name that begins with an emoji is shown as that emoji rather than reduced to
a letter it does not have.

**Messaging yourself was refused.** `ErrSelfDirect` rejected your own address,
which was the wrong answer to the right question: it is where a link, a code or
a thought goes on its way between two machines, and it is the most-used thread
in every product that has one.

Modelled as **one member, not two**. A self conversation of "you and you" would
have to survive every query that counts distinct members, and the first one
that did not would show somebody their own notes as a chat with a stranger.
`SelfConversation` is therefore its own query keyed on a member count of
exactly one, and `decorate` names it after you and marks it, so the list does
not fall through to "Conversation".

One test in this group was weak and the mutation run found it: relaxing the
count from `= 1` to `>= 1` still passed, because the notes thread happened to
be the older row and the query takes the first match. A second test now creates
the two-person conversation first, so row order works against the lookup
instead of for it, and that mutation fails as it should. `ErrSelfDirect`
survives for its one honest caller: opening a direct chat with yourself by
account id, from a meeting roster, where clicking your own tile and landing in
your notes would be a surprise rather than a feature.

**Emoji, in both composers.** `web/src/emoji.js`: a curated list, keyword
search over it, recents in `localStorage`, and the insertion arithmetic. No
library and no image sprites -- the platform already has the glyphs, and
shipping a megabyte of PNGs to draw characters the font can render is the kind
of thing this codebase measures and then deletes. Nine tests cover the parts
that are actually easy to get wrong: a selection is replaced rather than pushed
aside, the caret lands after what was inserted, search matches the start of a
word so "th" finds both thumbs-up and thinking while "umb" finds nothing, and
recents stay unique and bounded.

A message that is nothing but emoji is rendered large, the way every messaging
app does it -- a lone thumbs-up reads as a gesture, not a word. `isOnlyEmoji`
deliberately refuses a bare year, since digits are only emoji inside a keycap
sequence, and refuses a wall of twenty, since that is a message.

**Nothing new on the wire.** Self chat reuses the existing conversation and
message paths; emoji are UTF-8 in a message body that was already
length-prefixed; avatars are client-side rendering. The measured per-message
cost is unchanged.

## 6o. The visual system, rebuilt

The report was that the interface looked cartoonish. It did, and the causes
were specific rather than a matter of taste, which is why they could be
removed one at a time.

**What was actually wrong**

| | |
|---|---|
| Hues in the token set | ~20, including 8 full-spectrum avatar tones |
| Page background | three overlapping pastel radial gradients |
| Largest corner radius | 26px on panels, 34px on the sign-in card |
| Largest shadow blur | 64px |
| Sign-in illustration | ~160 lines of flat-vector clipart: a person at a desk with pot plants |

Each of those is a consumer-software signal on its own. Together they said
"friendly app" everywhere the product needed to say "instrument".

**The direction.** Quiet, dense, precise. In a meetings product the video is
the content, so the chrome recedes; credibility comes from restraint,
alignment and typographic hierarchy rather than from colour.

**Colour.** One accent, deepened from a 64%-lightness mint to `#0b7a62`
(`#23997a` on dark). The old brand was bright enough to be the first thing the
eye landed on wherever it appeared, so a "New meeting" shortcut outshouted the
meeting you were late for. Everything else is a slate neutral ramp with a faint
cool bias; amber, violet, sky and danger survive as semantics only and are
desaturated. Cards are separated by a hairline on a near-white (or near-black)
ground rather than floating on wide shadows, which at five panels a screen was
all the eye saw.

**Avatars.** The gradient disc became a tinted chip: a low-alpha wash of the
person's hue with the letters in a saturated shade of the same hue, inverted on
dark. The eight tones no longer span 360 degrees -- a rainbow, so a roster of
six looked like a bag of sweets -- but sit inside a 150-degree arc of blues
through teals to one clay, so a group reads as a set while individuals stay
distinguishable.

**Shape and type.** Radii capped at 12px outside true pills, and every
invented 13-20px value pulled onto the scale. Shadows tightened to
`0 1px 2px` and `0 4px 12px`. A type scale replaces per-component sizes, the
display size comes down from 34px to 26px (a dashboard heading that large is a
poster, and it pushed the content off the fold), and digits are tabular
wherever they are compared.

**The sign-in.** The clipart and its blue blob panel -- an organic asymmetric
radius, `0 46% 46% 0 / 0 22% 22% 0`, that read as a sticker -- were replaced by
a brand panel: a deep flat ground, the mark, one line of positioning, and a
low-contrast concentric motif taken from the logo. A motif belongs to the
product; an illustration belongs to a stock-art library. The submit button lost
its gradient fill, its circular badge, its 24px coloured glow and its
capitalised 0.1em tracking. The wordmark came down from 33px/800/-0.045em, and
the logo itself stopped being a mint gradient app-icon tile.

**Copy.** "Up late" at 1 a.m. is charming once and wearing by the tenth time.
"Nothing today. A clear afternoon is allowed to just be a clear afternoon."
also said *afternoon* at half past midnight. A dashboard reports; it does not
reassure.

**One regression, found by looking.** The token rewrite dropped `--rail` and
`--side` from the end of `:root`, and `.shell`'s three-column grid resolves
against them, so the layout collapsed into a single stacked column. It was
invisible in the diff and obvious in the first screenshot -- which is the
argument for rendering a design change rather than reasoning about it. Both
themes were then checked at the sign-in and the dashboard.

No behaviour changed and nothing new goes on the wire; the client-side test
suite (74 cases) and the Go suite under `-race` are unaffected and green.

## 6p. The company address book

The directory was closed in §6l so it could not be harvested, which was right
and left a gap: inside one organisation, finding a colleague by typing their
name is the entire point of having a directory, and requiring an exact email
address for it is a worse experience than the hole it fixed.

Both are now true at once, because the organisation is a list the deployment
**declares** rather than a property inferred from whoever is searching. That
distinction is the whole design. "Your own domain is your organisation" is the
obvious rule and it fails in the case that matters: on a server people sign up
to with personal addresses it makes every gmail.com account in the world
mutually discoverable, which is the exposure §6l exists to prevent. An explicit
list cannot make that mistake, and a short guard list of public providers is
refused even when one is typed into the console.

**How far outside the organisation you can reach is a setting.** `directory.scope`
is a ladder of three, and the strictest rung is what a locked-down company
tenant does:

| scope | reaches |
|---|---|
| `organisation` | colleagues only; an outside address finds nothing |
| `connections` (default) | the above, plus your contacts, plus one exact outside address |
| `everyone` | the whole account list, browsable |

Under `organisation` the restriction is **enforced where conversations are
created**, not only where the directory draws its results. `Service.MayMessage`
is injected into messaging from identity, and `OpenDirect` refuses before it
looks the address up -- so a closed deployment cannot be used to ask whether an
outside address is registered, and the refusal reads the same for a registered
address and an unregistered one. Group creation skips an excluded address
rather than discarding the group: one outside guest among five colleagues
should leave the five. Calling needs no separate guard, because a ring already
requires a shared direct conversation and that is now what cannot be created.

The rule is symmetric. An outsider cannot reach in either, or the restriction
is one-way and therefore not one. And with no organisation declared, the
strictest scope permits nothing new rather than everything -- fail closed.

Both halves were proved against mutations: removing the guard from `OpenDirect`
leaves the search box filtering while the API obliges, and making the domain
check one-way lets an outsider reach in. Each fails the corresponding test.

So a search reaches, at most, three places:

1. **Your organisation**, if your own address is in one of its domains --
   searchable by name, browsable, the way a company address book works.
2. **People you already share a conversation with.**
3. **One exact email address**, matched in full, for anyone else.

Somebody signed in with a personal address on a company deployment is a guest:
they get 2 and 3 and not 1. The staff list is not theirs to browse.

**It configures itself, once.** On a deployment that has never been told, the
domain is guessed from the founding administrator's address -- a company
installing this signs in as themselves, and their address says which company it
is. `SeedOrgDomains` writes it only if the setting has never been written, so a
restart never overwrites a choice, and an administrator who deliberately clears
it to close the address book does not have it reopened. A public provider is
never seeded.

Verified against a copy of a real deployment's database, where the seed picked
up `ddecor.com` from the administrator and a colleague could then find people
by first name, by surname and by a fragment of either, while `divyanshi`,
`gmail` and `@` returned nobody outside the organisation and a full outside
address still resolved.

**Domains match wholly, and only what is listed counts.** The comparison is on
the part after the last `@`, against the list, case-insensitively and with no
suffix or stem matching of any kind. Two domains that look related are two
organisations: `ddecor.com` and `ddecor.in` share a label and nothing else, and
they can be two companies, two legal entities, or one company and whoever
registered the other ccTLD. Merging them because the middle word matches would
hand a stranger the staff list on the strength of a domain registration, which
is not a credential. `mail.ddecor.com` is likewise separate from `ddecor.com`,
and `notddecor.com` is not swallowed by it.

An administrator who wants two domains treated as one organisation lists both,
and then they are one. Nothing else combines them, and the automatic seed
writes exactly one domain -- the founding administrator's -- never a family of
them. A mutation that adds suffix or stem matching fails
`TestDomainsMatchWholly`.

One existing test had to be relaxed and is worth naming: `TestEverySettingResolves`
asserted that every setting has a non-empty default, and the organisation list
defaults to empty on purpose, because empty means "no organisation", which is
the closed answer. The assertion now exempts text settings, and three new tests
pin what replaced it.

## 6q. Live captions and reactions

**Captions** were listed in §7 as "control wired, no recogniser behind it".
The recogniser is now the browser's own, running on the machine that is
speaking. (Superseded by §6bj. Captions now ride `OpRecording`, everyone's
speech is captioned once anyone turns captions on, and each speaker is asked
first. §6bj also corrects the privacy claim below: where the browser cannot
recognise on the device, it sends the audio to its own online service.)

That is the decision worth stating. The alternative is shipping every
participant's audio to a transcription service, which costs money per minute,
adds a network hop to a path this project has spent its whole life shortening,
and sends the contents of private meetings to a third party. Recognising
locally and sending only the resulting text avoids all three, and a sentence is
a few dozen bytes where the audio it came from is kilobytes a second.

`OpCaption` carries the **whole current sentence every time**, never a delta. A
caption assembled from fragments would be permanently wrong if one were
dropped, and repairing it would cost more than resending -- the cross-frame
state rule (§5) applied to text.

The cost is controlled at the client, because a recogniser emits interim
results many times a second and repeats itself while it thinks. Interim results
are throttled to one every 350 ms and suppressed entirely when the text has not
changed; a final result is never delayed. Without that gate a caption track puts
more frames on the connection than a camera does, and the gate is where the
tests are.

Captions from somebody a host has muted are dropped at the server. Muting
somebody must not silence their voice and publish their words.

**Reactions** (`OpReaction`) are one emoji, thrown at the room and gone. They
are deliberately stored nowhere: a gesture during a moment is not a record, and
a log of them is neither wanted nor kept. Rate limited to one every 500 ms, so
a held key is not a frame storm, and gated on the chat permission, since a
reaction is a thing you say to the room without typing it.

Neither costs anything at rest. A meeting where nobody is speaking and nobody
reacts sends no caption or reaction frames at all.

## 6r. The Windows application

`cmd/aether-desktop` is a Windows app: one Go binary, a 4.5 MB executable, no
installer payload beyond itself.

**Why Go rather than Tauri or Electron.** The feature that justifies a desktop
app at all is ringing when nothing is open -- a browser tab must exist to
receive a call, and a tray process does not. That means the app has to hold the
messaging socket open itself and decode the binary wire format. That decoder
already exists here, in `internal/wire`, tested and byte-compatible with the
browser's. A Rust or JavaScript shell would need a second implementation of the
same format, and this codebase has already paid for a protocol implemented
twice: the interop harness exists because two opcode tables silently disagreed,
frames went nowhere, and nothing in either log said why. One protocol, one
implementation.

Electron would also have bundled a second Chromium next to the one Windows
already ships. The window is the WebView2 runtime, present on this machine at
version 152, and it loads the same client from the same server that a browser
loads -- verified: the window fetched `index`, every ES module including
`avatar.js`, `captions.js`, `emoji.js` and `meeting.js`, and the identity
endpoints. There is no copy of the interface inside the app, so a deployment
upgrades by restarting the server rather than by asking several hundred people
to install something.

**What is built.** The window, a remembered server, and a settings file under
`%APPDATA%\Aether` written atomically -- a half-written settings file is a
program that will not start again. The WebView2 profile lives beside it, so
signing in survives a restart without disturbing the user's own Edge profile.

`NormaliseServer` accepts every spelling of the same address, because the first
screen anybody sees is "which server?" and refusing four spellings because the
fifth is canonical is how software feels hostile before it has done anything: a
bare host, a host and port, a full URL, one with the meeting path still on it,
one with a trailing slash, and `wss://`. HTTPS is assumed rather than demanded,
since the browser refuses camera and microphone access anywhere but HTTPS and
loopback, and typing a bare host and silently getting a window with no camera
is the worst of both outcomes. Plain HTTP is kept when it is explicitly asked
for, which is what a loopback development server serves.

**Three defects found by running it, not by reading it.**

*"It feels like a virtual machine."* Exactly right, and the cause was not
colour. A process that has not declared itself DPI-aware is told the screen is
96 DPI; it renders at that size and Windows bitmap-stretches the result to the
real scaling factor, so every glyph edge and hairline goes through a resampling
filter. Soft, slightly washed out, and indistinguishable from a remote desktop
session -- because that is a scaled bitmap too. `SetProcessDpiAwarenessContext`
with per-monitor v2, called before any window exists, and the softness is gone.
Per-monitor rather than system-wide, because a laptop docked to a second screen
at a different scale has to re-render rather than re-stretch when the window is
dragged across. The title bar is also painted to match the application, since a
dark page under a light frame is the other half of "this looks like a browser
in a box".

*"Refused to connect."* The saved server was unreachable and the window showed
a WebView2 error page: the browser's failure message, inside something that is
not a browser, with no address bar and nothing to act on. The address is now
probed before a window opens, and the app explains itself in its own words --
naming the address, distinguishing "nothing is listening there" from "that name
does not resolve" from "it did not answer", because those lead to different
actions. The probe skips certificate verification deliberately and narrowly: it
reads no body, carries no credentials, and exists only to tell "nothing is
there" from "something is there".

*Two windows.* Launching twice opened two, each with its own WebView2 profile
lock and its own session. A named mutex now claims the instance and a second
launch raises the window that already exists. A mutex rather than a lock file
because Windows releases it when the process exits **including when it
crashes**, and a lock file left behind by a crash is a program that will not
start again until somebody deletes a file they have never heard of.

**The first frame.** WebView2 shows nothing until a page commits, and on a cold
start that is the runtime initialising, a TLS handshake and the client's
modules arriving -- an empty rectangle for all of it. An empty rectangle is
what a web view looks like; it is not what an application looks like, and it
was the whole reason this still felt like a browser opening a page. The window
now paints a splash before anything is fetched: the same mark, the same ground
and the same motif as the sign-in panel, so the splash and the first real
screen are one product rather than two handing over. It is inline and
references nothing, because it has to render before the network is touched.
The client then fades in over 260 ms, in the browser too, so the handover is
one motion rather than a snap.

**What is not built yet, and why it is staged.** The ring listener is present
but not armed: without a device token there is nothing to authenticate the
background socket with, and a socket that reconnects forever against a 401 is a
busy loop rather than a feature. The token needs a server endpoint and a way
for the signed-in window to hand it to the shell. That is the next stage, and
it is the stage that makes the app worth installing. After it: the tray icon,
start at login, and a global push-to-talk key.

## 6s. The brand mark, cut for both grounds

The supplied artwork is one square on white: a glass "A" with an orbital ring,
the wordmark under it, and the line "People closer anywhere". Shipping that
file as the logo is what makes an interface look homemade -- at 30px in a rail
the wordmark is three grey smears, and on a dark ground the white background is
a white box.

**Cutting it out was the hard part, and two obvious methods both failed.**

*Unmultiply against white.* Glows and soft edges survive perfectly. But this
artwork uses white as its LIGHT SOURCE -- the speculars along the glass are
near-white -- so they turn transparent and the mark goes dull and lifeless on
a dark ground. Proved by looking at it.

*Cut at the saturated silhouette.* The highlights stay bright, but the pale
outer glow is faintly tinted: a fixed chroma threshold keeps it and leaves a
white fringe on dark, and a relative threshold drops it together with the
highlights, so the mark comes apart.

**Both at once is what works,** and it is what `tools/build-brand.py` does. The
artwork body is found by chroma *relative to lightness* -- the glow is about
0.13 of its own brightness against 0.77 for the glass -- then morphologically
closed, so highlights that run out to the edge of the glass end up enclosed and
get filled. Inside that silhouette the original pixels are kept exactly, so the
glass keeps its light. Outside it, alpha comes from distance to white, so the
glow fades instead of ending in a hard pale edge. One asset, correct on a
near-black panel and on a near-white page.

The wordmark and tagline are flat artwork, so they are recoloured per ground
rather than cut twice; the mark is not, because repainting shaded glass
flattens it into a sticker.

**Sized for where it is used.** The rail tile takes a 128px mark; the sign-in
panel takes the full lockup; Windows gets a multi-resolution `.ico`, because it
picks a different size for the taskbar, alt-tab and the title bar and an icon
carrying only one is resampled into mush at the others. The desktop splash
carries the mark as a data URI in the binary, since it has to render before the
network is touched.

The sign-in panel now shows the lockup and the brand's own line instead of a
slogan written for it, and `--wordmark` swaps the cut between themes with one
token.

## 6t. Trusting one server, not switching off trust

The app's first real launch showed "Your connection isn't private" instead of a
sign-in screen. The server signs its own certificate, which is normal on a
local network, and a full-page interstitial inside a window with no address bar
and no padlock to inspect reads as the product being broken.

The tempting fix is `--ignore-certificate-errors`, and it is the wrong one: it
disables verification for every host the window ever reaches, which is a
browser with its safety removed.

What is done instead is a pin. The server's certificate is fetched once, and
only if it does not already validate -- a deployment with a real certificate
gets ordinary, unmodified verification and no switches at all. Its public key
is hashed and passed to the runtime through
`WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS` as
`--ignore-certificate-errors-spki-list`, naming that one key as the single
exception. Every other host still validates.

The hash is over the SubjectPublicKeyInfo rather than the certificate, so the
exception survives this server reissuing its certificate when the machine's
address changes (§6j) without being widened.

Verified from the server's own log rather than by looking at a window: after
the pinned relaunch the app fetched the page, every ES module and every brand
asset. Had the interstitial still been showing, Chromium would have blocked the
navigation and none of those requests would exist.

## 6u. The window on a scaled display, and the notification area

**"Resizing misplaces everything" was not a layout problem.** The window
library takes its width and height in PHYSICAL pixels; the page inside is laid
out in CSS pixels, and on a 200% display there are half as many. Asking for a
1280 pixel window on this laptop gave the client 653 CSS pixels -- permanently
below its own 940px breakpoint -- so it reflowed into the narrow arrangement at
every window size and looked broken always, not only when dragged small. The
requested size is now multiplied by the window's own DPI, and a minimum is set
so the layout cannot be squeezed under the breakpoint on purpose either.

Worth recording how this was found: the window measured 653x456, which looked
like a resize bug until `GetDpiForWindow` returned 192. Two separate
DPI mistakes in one app -- not declaring awareness (§6r) and then not scaling
the size -- with completely different symptoms.

**The notification area, and what minimise is for.** Minimising was briefly
hijacked to hide the window into the tray, and that was wrong: minimise means
"put this on the taskbar where I can see it is still running", and an app that
vanishes instead has taken a decision away from the person using it. Teams does
the same -- minimise goes to the taskbar, and only closing goes to the tray,
and only because it is still ringing. Minimise is now left entirely alone; the
tray icon stays, so it can be clicked to raise the window and so there is
somewhere for background call notifications to appear once they exist.

The brand mark appears in the title bar, the taskbar and alt-tab.
The icon is embedded in the binary and written beside the settings, so no
resource compiler is needed in the build, and it is multi-resolution because
Windows picks a different size for each place it draws.

The tray icon reports to a **message-only window of its own** rather than to
the WebView2 window. Reading tray clicks from the WebView2 window would mean
replacing that window's procedure, which works until the library reorders
something and then leaves a window that ignores the mouse.

**Two bugs on the way to that, both worth recording.** The first version hid
the window on minimise while the tray icon carried no `NIF_MESSAGE` flag, so it
delivered no clicks: the app went into the notification area and could not be
brought back -- worse than the behaviour being fixed. The rule that came out of
it is that a tray icon and the way out of the tray are one feature, not two.
The second was the hijack itself, reported immediately and correctly as "it
should remain in the taskbar but it is closing".

Both were found by running it. A second launch also raises the window, which
remains a way back that does not depend on the tray at all.

## 6v. Two products, two bars

The desktop app strips the system caption (`StripCaption`, one style bit:
WS_CAPTION) and the page draws the window's title bar in its place: mark,
name, search, theme toggle, account, and the three window buttons. That bar
leaked into the browser — a 46px band above a page that already had a tab to
name it and a rail to carry the mark — and the report was exact: "web is
different, app is different".

The two are now separate on purpose. `inDesktopShell()` is a capability check
(`window.aetherStartDrag` is a function, not a user-agent string) and it
decides which of two bars exists:

- **App:** the fixed `#titlebar`, `body.has-titlebar`, window buttons and drag
  handled by the shell bindings.
- **Web:** `#web-bar`, a grid row inside `.home` above `.home-main`: search on
  the left, theme toggle and account together on the right. No fixed band, no
  brand in a caption, nothing that pretends to be a window. Empty, it takes no
  room (`:empty { display: none }`), so the desktop layout is untouched.

Four things the split exposed, all found by running both products:

1. **The bar was drawn before anybody was signed in.** `renderTitleBar()` ran
   once from `wireHome()` at boot, with no session, and never again; the app
   showed a theme toggle and nothing else. It is now drawn again in
   `enterHome()` once the account is known.
2. **A stale binary renders the web layout.** With the exe predating the
   bindings, `inDesktopShell()` is false and the app draws `#web-bar` under a
   system-looking caption — which looks like a design regression and is a build
   one. Check the exe's mtime against `agent.go` first.
3. **The dropdown was a drag handle in the app.** It hangs inside the title
   bar, which is `[data-drag]`; its rows are `role="button"` divs, not
   `<button>`s, so `isDragTarget` let a mousedown start a window move and the
   click never arrived. Fine in a browser, dead in the app. `[role="button"]`
   and `[data-no-drag]` on the panel are excluded now, with a test that fails
   against the old selector.
4. **`height: 100%` of an auto row is `auto`.** `.home` has rows
   `auto minmax(0,1fr)`; with the web bar absent the main was auto-placed into
   the first, content-sized row and the chat shell came out half-tall in the
   app while filling the browser. `.home-main { grid-row: 2 }` pins it.
   Measured with the bar present and removed: main 0→609 of a 609px viewport,
   shell ending 30px above the fold, nothing scrollable, in both conditions.

A fifth was older and hidden: `renderThread` set `onclick` on a "Start a
meeting" button that only group threads render, so opening any direct thread
threw *after* drawing it. Every caller that awaited `open()` saw a rejection
— "Could not open that chat" over a chat that had opened.

**Search is split the same way.** The chat column's field filters the chats
you already have and nothing else — no dropdown. The top-bar search is the one
place to find *new* people: local contacts rank on every keystroke, the
directory's answer (`directory.search`, inside the org-domain rules of §6q) is
merged when it lands, stale replies are dropped by `createSearch`, and the
list carries a "message yourself" row when the query is plainly you, matching
meetings, and audio/video call buttons on hover. Choosing a person opens the
thread. The chat "+" button and its dialog were removed; group creation has no
entry point until one is designed for it (§7).

**The emoji panel that would not close** was a cascade bug, not a listener
one: `.emoji-panel { display: flex }` outranks the user-agent `[hidden]` rule,
so `panel.hidden = true` did nothing visible. `.emoji-panel[hidden] { display:
none }` is the whole fix. Verified in the browser: open, click the thread,
closed.

**The emoji panel that closed after every emoji** was the mirror image.
Choosing a cell redraws the grid, so by the time the click bubbled to the
document the cell was no longer in the panel and `panel.contains(target)`
answered no — an inside click counted as an outside one. `composedPath()` is
computed at dispatch and still knows. Verified: two emoji into one message,
panel open throughout.

**Messages: no avatar, one header per run.** A face beside every line in a
two-person thread is a column of the same two letters; the thread header
already names the other person. Each run now opens with
`Name · Day · Time` (`web/src/when.js`: Today / Yesterday / weekday inside a
week / "4 Sep" beyond, then the clock) tight against its first bubble, and
the rest of the run sits directly beneath. `when.js` is separate from
`chat.js` so it is testable without a DOM; the midnight boundary is one of the
tests. The thread also re-scrolls to the newest message after layout settles:
measured 56px short with a single synchronous scroll.

**Runs are about gaps, not clock minutes** (`web/src/runs.js`). Your own
messages carry only `Day · Time`; the other person's carry their name too —
you know who you are, and the side of the thread says it. A message continues
the run before it when it is the same sender, neither is deleted, the gap since
the *previous* message is under five minutes, **and** the run has not been
going for more than twenty minutes since its *first* message. The first two
are the obvious Teams rule; the third is what stops a slow, steady stream (a
line every four minutes for an hour) from never showing a time again — the
test for it fails against the gap-only rule. Somebody who starts typing at
10:50:55 and sends at 10:51:05 has not started a new thought, and there is a
test for that too. Your bubbles now hang from the right edge with their header
(the bubble was left-aligned inside a right-aligned body, leaving the gap
reported as "a great margin"): measured body-right = bubble-right = 1209px.

**The app's title bar carries a sidebar toggle where the mark was.** The rail
already shows the brand; the caption's job is to control the window and the
page. The toggle sets `body.rail-collapsed`, which sets `--rail: 0px` — both
rails (home and in-call) are sized by that one variable — and the choice is
remembered in `localStorage` (`aether.rail`). Browser only ever sees the web
bar, which has no toggle. One trap: a grid item's automatic minimum is its
content width, so a `0px` track was still held open at 68px by the buttons
inside it (computed `grid-template-columns` read `68px …` with the variable at
`0px`); `min-width: 0` on the collapsed rail lets the track win.

**Two deletes, two questions** (migration 0033, `messaging_hidden`). "Delete
for everyone" is about the message: sender only, soft-delete on the row, every
member sees the tombstone (`Service.Delete`, unchanged). "Delete for me" is
about one reader's view: a `(user, message)` pair, the same shape as saving
one, applied when a page of history is read (`withoutHidden`), available to
any member on any message. Tests hold the line between them: hiding is
invisible to the other member; tombstoning is visible to both; a non-member
cannot hide what they cannot read. The client offers both from the trash on
every message (the second only on your own), in a small menu that replaces the
action bar; edit puts the text back into the composer and Send saves it.

**The action bar hangs from the bubble.** It used to be positioned against
the full-width row — for your own messages that put it at the top-left of the
thread while the bubble sat at the right, and the pointer left the message on
the way over, which is why "react is not working". It is now inside
`.msg-body`, directly under the message and touching it, on the bubble's side;
the log keeps 52px under the newest message so the bar is not clipped there.

**A meeting is the whole screen.** `.app` had an 18px margin around a rounded,
shadowed `.shell`, which read as a smaller window floating inside the real one
— reported as "a sub window". Padding, radius and shadow are gone; the call
runs edge to edge in both products.

## 6w. Chat, made to hold: the push tail, forwarding, files, and the harness that proves it

The report was "reactions do not work". They did — the server recorded them
and a reload showed them — and that is exactly the kind of bug a unit test
cannot see. The push frame (`OpConvMessage`) stopped at the body. A reaction
arrived at every client as a bare copy of the message, which replaced the
decorated one the client already had, and the chip somebody had just clicked
vanished a few milliseconds later. **Rule: whatever the thread draws from a
message rides on the push, or the push is a regression of the REST view it
overwrites.** The frame now carries, after the body:

| field | bytes on a plain message | notes |
|---|---|---|
| replyTo (interned str) | 1 | quote sender/deleted/body follow only when set, body capped at 200 |
| reactions | 1 | per emoji: interned emoji, count, up to 12 interned reactor ids — the viewer derives "mine", so one encoding serves every member |
| attachments | 1 | per file: id, name, type, size |
| forwarded | 0 | one of the six padding bits after deleted/edited |

Three bytes on a message with nothing to say (`TestPushTailCostsThreeBytes…`,
measured against the old encoder, not assumed). `TestPushCarriesReactionsAndTheQuote`
reads the frame off a real socket and fails against the old encoding at the
reply marker — verified by mutation before it was trusted.

**Two more bugs found on the way, both real.** Two messages sent inside one
millisecond sorted arbitrarily (`ORDER BY created_at` alone), which showed up
as an intermittent test and would show up as a reordered thread; `rowid` now
breaks every tie. And the API throttle's 429 carried no `Retry-After`.

**Forwarding** (`Service.Forward`, migration 0034): new messages, from the
forwarder, marked `forwarded`, body and files copied (files share the stored
bytes, new rows), reply link dropped. All-or-nothing on membership — every
target is checked before anything is written — source must be readable and
not a tombstone, at most ten targets. Client: a forward button on every
message; one search field over your chats first, then directory people,
opening a chat for a person on the way.

**Files in chats** (`attachments.go`, migration 0035). Bytes live on disk
under a server-generated 32-char key (`<db dir>/files/<k[:2]>/<key>`), written
to a temp name and renamed; the sender's filename is a display string that
never reaches the filesystem (`SanitizeFileName`: separators, control
characters, quotes, leading dots). The type is **sniffed from the bytes**;
the declared type survives only when the sniffer is blind (zip/plain/octet)
and the declaration is one of a short document list. On the way out,
`ServeType` renders inline only a fixed list (png/jpeg/gif/webp, pdf, mp4,
webm, mpeg/ogg/wav, plain text); everything else — HTML, SVG, scripts — is
`application/octet-stream` + `attachment` + `nosniff`, because an uploaded
page rendering on this origin would run with this origin's session.
Membership is checked **before the first byte is accepted** (`CanPost`) and
again on download; a message deleted for everyone takes its files with it.
Caps: 25 MiB per file (one byte over is refused and the partial blob
removed), ten files per message, the whole multipart request bounded.
`ServeContent` gives Range for media. A "Shared" view lists files (newest
first, via the `(conversation_id, created_at)` index) and links (URLs
extracted from the 500 newest bodies on read — links are rare, and a second
table would have to follow every edit and delete).

**The end-to-end harness** (`tools/e2e/messaging.e2e.mjs`) runs against a
live server: throwaway accounts (created once, reused — registration is rate
limited and the harness must not switch that off), the real cookies and CSRF
header, and a hand-rolled TLS WebSocket client so the session cookie can be
sent (Node's built-in cannot), decoding every push with the browser's own
`wire.js`. 213 assertions: authentication and CSRF, push delivery and the
sender's non-echo, reactions on the push to everybody including the reactor
and "mine" from each viewer's side, reply quotes, cross-conversation quote
refusal, forwarding rules, both deletes, a stranger refused on every verb,
input limits, order under a 40-message burst, two people sending 50 each in
parallel with nothing dropped or duplicated on either socket, the per-account
budget (60 burst / 20 per second, 429 with Retry-After, B unaffected by A's
burst), uploads with path-stripping and sniffing, member/stranger/anonymous
downloads, Range, shared view, the caps, delete taking files, forwards
carrying files. Latency on loopback: list p95 1.3 ms, history p95 2.0 ms,
send p95 1.4 ms.

**Scale** (`scale_test.go` in messaging and identity; skipped under `-short`
and `-race`). Messaging: 6,000 conversations, 360k messages, one person in
200 threads. History page p95 1 ms; older page 1 ms; shared links 0.5 ms.
The conversation list was the slow path: 14 ms row-at-a-time (two queries per
conversation), 33 ms after a first batching attempt (join-and-group for
unread, a window function for last messages — both visited every message of
every conversation), 10 ms now: a correlated unread count that walks the
`(conversation_id, created_at)` index from `last_read` forward, and last
messages by joining on the conversation's own `last_message_at` (one index
seek each). The fixture is the worst case — every message unread. Identity:
50,000 people in one domain, a keystroke of directory search p95 28 ms
(`LIKE '%q%'` over the domain; below the 140 ms client debounce; an FTS index
is the next step if a deployment is larger).

## 6x. The directory at fifty thousand: an index lookup, measured

The user's rule (now `CLAUDE.md` §7): every search is an index lookup,
microseconds to single milliseconds at 50,000 people. The directory was
`LIKE '%q%'` over the table: 28 ms a keystroke at that size, and the same 28
for a query that matches nobody, because a scan does not know that.

Three shapes now (`directory_query.go`, migration 0036), tried in order and
each bounded by LIMIT: a range on the generated lower-cased `name_key`
index (already in name order — the first twenty rows *are* the answer,
nothing is sorted); the same on `email_key`; then, for three characters or
more, the trigram FTS5 table over name and address, capped at 500 candidates
before the join and sort. Triggers keep the FTS table in step with every
insert, rename and delete. A generated `email_domain` column with an index
replaces the `substr(instr())` the organisation filter computed per row.

Two planner lessons, both measured rather than guessed:

1. **`INDEXED BY` is load-bearing.** At a one-domain company the domain index
   matches every row, and the planner chose it anyway, evaluating the name
   predicate row by row: 59 ms for a one-letter prefix. Pinned to the name
   index: 0.5 ms.
2. **`CROSS JOIN` pins the FTS candidates as the outer loop.** Left alone,
   the planner scanned the users table and probed the (empty) candidate set
   per row: 13 ms for a query with no match. Pinned: 0.5 ms.

| query at 50,000 people | before | after (p95) |
|---|---|---|
| first-name prefix "pri" | 28 ms | 0.55 ms |
| rare full name | 30 ms | 0.52 ms |
| no match "zzzz" | 28 ms | 0.54 ms |
| one letter "a" | 25 ms | 0.54 ms |
| outsider by full address | 29 ms | 0.71 ms |

Semantics changed in one place on purpose: one and two characters are a
prefix, not a substring ("ma" no longer returns every Sharma, Verma and
Kumar), which is what a first keystroke means and what keeps it indexed.
Query text is quoted as one FTS phrase with quotes doubled and control
characters stripped, so nothing typed can become an operator; the test feeds
it `pri" OR "ver`, `NOT`, `*`, column filters and a NUL.

Also in this pass: the reaction bar's "+" opens the full picker beside the
message (one at a time; closes on choice, outside click and Escape — each
verified in the browser), and Shared is a tab beside Chat under the person's
name, as the reference lays it out, with Files and Links counts.

## 6y. Channels, groups, the unread counter, and what one person decides about a chat

**Unread is a maintained counter** (migration 0037, `messaging_members.unread`).
The list read it as a count per membership on every open — 10–14 ms for a
person in 200 threads at the worst case. The counter is bumped on send (for
everybody but the sender whose cursor is behind the message), **recomputed
exactly** on mark-read, and adjusted on delete-for-everyone and
delete-for-me, all with one predicate (`unreadTruth`) that is also what
`UnreadCount` still computes. `unread_test.go` runs a seeded random schedule
of 400 sends/reads/deletes/hides across two conversations and two people and
checks counter == count after every single op. It found two bugs on its first
two runs: a message stamped in the same millisecond as a cursor was counted
by the bump and not by the count (the bump now uses the cursor predicate);
and SQLite evaluates every `SET` expression of an `UPDATE` against the row's
*old* values, so a recompute written against `messaging_members.last_read`
counted from the cursor being replaced — the new cursor is now bound
explicitly. Membership events are never unread.

**Channels and groups** (`channels.go`). Both are conversations underneath:
same messages, pushes, files, reactions. On top: a membership that changes
(any member adds; owners remove and rename channels; anyone leaves; an owner
who leaves hands the role to the longest-standing member), **membership
events as system messages in the thread** (stored, pushed to everybody
including the actor's other devices, previewed, flagged so the client draws
them as events), and for channels a browsable name. Public channels are
browsable and joinable by anybody who could message the creator — the
organisation rule, so an outsider never sees a company's channel list;
private ones are invisible and invitation-only; names are unique
case-insensitively via a partial unique index on `title_key`. **A channel of
two thousand ships its member count, not its members**: the list loads
members only for conversations under 40 people, the people panel pages, and
fan-out uses `MemberIDs` (ids only) rather than loading rows — a send to 501
members is tested under 200 ms including the pushes.

**What one person decides about a chat** (migration 0038, `prefs.go`):
favourite (pinned under a Favourites heading), muted (out of the badge
total), hidden (out of the list until somebody else writes — your own
message does not bring it back), mark as unread (cursor to just before the
newest real message, counter recomputed from the same truth), and delete for
me (hide + every message so far hidden for you; it returns with only the new
messages when somebody writes). All on the membership row; nothing touches
anybody else's view. A first version built the `UPDATE`'s columns from a map
and sorted the columns afterwards, so `hidden` could receive `favourite`'s
value — the end-to-end harness caught it ("hidden: out of my list" failed),
and a test now sets all eight flag combinations and reads back the columns.

**Client.** Pills over the list — All · Unread (n) · Chats · Channels; the
pencil menu — New group / New channel / Browse channels; a members panel
(add people with the same live search, remove for owners, rename/topic,
leave); a right-click / ⋯ menu on every row with the five decisions above
(delete asks twice); channel rows and headers wear `#`; membership events
render centred as events and break message runs on both sides; `Chat |
Shared | Photos` tabs, Photos being the images shared in the conversation as a
grid. Verified in the browser and by the harness: **278 assertions, 0
failed**, including a channel created, browsed, joined, posted to, renamed
and left; private stays private; owner rules; a group where a member adds and
renames; and ownership passing on leave.

## 6z. Groups, the rest of the way

`groups.go`, migration 0039, opcode `0x11b`. What a group of people needs
that a pair does not:

- **A name that writes itself.** An untitled group is displayed as its other
  members — full names while there are three or fewer, first names and a
  count beyond ("Priya, Rahul and 6 others"), the count alone for a big group
  that ships no members. Computed on every read, so it follows renames and
  departures; a real title wins.
- **History on joining.** The adder chooses none / last N days / all. Applied
  as delete-for-me rows on the newcomer's membership (`HideBefore`), with the
  unread and mention counters recomputed from the same truth, so the record
  and everybody else's view are untouched.
- **Mentions.** The client sends member ids with the message; the server
  keeps only members (dropped, not refused — a stale member list is not a
  request to reject), bounded at 20, stores them, bumps a per-member
  `mentions` counter under the unread predicate, and carries the ids on the
  push (interned; a regular is a byte). Read clears it; delete-for-everyone
  and delete-for-me adjust it. The random-schedule test now names the other
  person on every second send and holds this counter to its own truth too.
- **A picture.** Members set it; sniffed image types only (an HTML file
  declared as PNG is refused); 5 MiB; served to members with `nosniff`; the
  previous blob is removed. Not on direct conversations.
- **One pinned message** per conversation, from this conversation, not a
  tombstone or an event; any member pins (a shared bookmark, not moderation);
  the list carries the pinned quote.
- **Read cursors, live.** Mark-read pushes `OpReadCursor` (conversation, who,
  when) to the members — the actor's other devices included — so "Seen" /
  "Seen by Priya, Rahul" is true rather than eventual. Typing and read
  cursors go only to a **small audience** (≤ 40 members): a thousand-member
  channel gets none of that traffic. Typing now also checks membership, which
  it did not before — anybody could have made "X is typing" appear anywhere.

Client: an `@` picker in the composer for groups and channels (members
filtered as you type, arrow keys, Enter/Tab, click; the chosen names are
remembered for the send and only the ones still in the text are sent),
mentions lit in bubbles and an accent when you are named, `@` badge on the
row, a pinned bar above the thread with jump and unpin, a pin action on every
message, "Seen" under your newest read message, the picture in list and
header, "Change picture" and the history choice in the members panel, and
groups/channels in the title-bar search.

Harness: **305 assertions, 0 failed**, including the auto-name from both
sides, mentions on send/push/list, the read-cursor push observed on the other
socket, no-history joining, picture refusal/acceptance/serving, and pin /
foreign-pin / unpin.

## 6aa. The meeting chat, and the rest of the chat feature set

**The meeting chat** (`meetingchat.go`, migration 0040, gateway `OnRoster`).
Two people in a room are a call: what they type goes to the direct
conversation they already have. Three or more signed-in accounts (devices
and guests do not count) are a meeting, and the moment the third arrives the
server creates a group conversation for the room — one per room name, so a
recurring meeting accumulates in one place — with everybody present as
members; later arrivals are added and the thread says so. A scheduled
meeting's title names it; an ad-hoc room's chat is named after its people.
The gateway calls `OnRoster` after every join with the room's people (ids
and names only; the signal package never imports messaging); a unique
partial index on `meeting_room` is the race arbiter when two joins try to
create it at once (eight concurrent joins → one chat, tested). Inside the
meeting the "Everyone" tab becomes "Meeting chat" and its messages are the
conversation's — kept, listed, searchable afterwards with a camera avatar.
Only people who were in the room can look it up (`GET /meeting/{room}`).

**Composition** (`compose.go`, migration 0041): a priority (`important` /
`urgent`, two words and nothing else), a **scheduled send** (30 s to 30 days;
stored at once, the author's alone and marked "Scheduled", invisible to
everybody else and uncounted until `RunScheduler` delivers it — stamped,
counted, pushed, placed at delivery time; deleted before then, it never
arrives), and a bounded opaque JSON `meta` for cards (praise today). Both
ride the push: **six bytes** on a plain message for the whole tail now, still
measured.

**Search over messages** (`SearchMessages`): trigram FTS5 over bodies, kept
by triggers (edits re-index, deletes drop, membership events never enter),
candidates bounded at 2,000 before the membership and delete-for-me
filters, `CROSS JOIN` pinning the candidates as the outer loop. Over 200k
messages: common word (25k matches) p95 7.5 ms, rare phrase 2.2 ms, no match
0.8 ms. Three characters minimum; hostile text is a phrase, never an
operator.

**Client** (`format.js`, tested): **bold / italic / strike / inline code /
fenced code with line numbers / bare URLs as links** — applied to escaped
text, emitting only its own tags (the first test is `<img onerror>` staying
text); a formatting bar that appears on focus with Ctrl+B/I/E; a priority
toggle; **send later** (an hour, tomorrow 9:00, Monday 9:00, or a picked
time) with a "Scheduled" tag and Cancel on the message; **praise** cards;
**video clips** recorded from the camera (MediaRecorder, ≤ 60 s) and sent as
files, videos playing inline; **undo send** for six seconds; a per-message
menu with **copy text, copy link to message, read aloud, open chat in a new
window**; **links to messages** (`/?chat=…&msg=…`) that open the thread and
scroll there, loading older pages if needed; **drafts** kept per conversation
and shown in the list; **suggested replies** for questions; **compact /
comfortable spacing**; **Ctrl+K** to search; **desktop notifications** for
what arrives while you are not looking (urgent ones insist; muted chats stay
quiet unless you are named); quick reactions that are **your most-used
six**; and **message search** in the title-bar dropdown. The reply glyph is
now the mirror of forward.

Not built, and why: **translate** and **immersive reader** need a language
service this server does not have; **GIF/sticker galleries** need a
third-party catalogue; **loop components** and **text prediction** are
products in themselves. Everything else on Microsoft's own chat feature list
for Teams is here.

Harness: **326 assertions, 0 failed**.

## 7. What is not built

Stated plainly, because an architecture document that implies completeness is
a lie.

| | state |
|---|---|
| Live captions | **built** (§6q): the browser's own recogniser, text on the wire |
| Recording | opcode reserved, no encoder or storage |
| Remote control / take control | **capture path built and measured** (§4a); input injection, consent/audit session and agent binary not started |
| Breakout rooms | opcode reserved, no handler |
| Reactions | **built** (§6q) |
| External calendar sync | not started |
| Password reset | placeholder — directs to an administrator |
| E2E encryption | not started; speaker ranking already anticipates it, since energy is reported by the client so the server never needs to listen |
| TURN | **built**: an embedded relay on udp/3478, tcp/3478 and tcp/443 |
| SFU-to-SFU relay | single node, capped at 500 participants |

### Known work items

- `MinUtilityPerMbps` is fitted to two hand-worked cases, not field telemetry.
- `Estimator.Observe` uses a fixed exponential filter; a trendline or Kalman
  estimator would classify overuse earlier.
- Layer resolutions are seeded from a conventional ladder rather than read
  from the publisher, so the spatial term is approximate until a rung is
  measured.
- Sessions are single-factor.
- **`-secure-cookies` is off by default** because local development has no
  TLS. It must be on before this is reachable from anywhere else.

### 6ab. Search indexes what is read; the hover bar belongs to the bubble

**Plain-text index.** The first message index (0041) was filled by SQL
triggers from the raw body, so "ship tod" did not find "ship **today**": the
trigram saw `p **t`. People search for what they read on screen, not for the
marks around it, and SQL cannot strip the marks. Migration 0042 drops the
triggers and empties the table; `indexMessage` in the repository now writes
`plainText(body)` (a Go mirror of `web/src/format.js plainText`, the two held
equal by the same examples in both test suites) inside the same transaction
as every insert, edit and delete; system events and empty bodies are not
indexed. `ReindexIfNeeded` refills an empty index once at start-up -- 2,835
messages on the dev database, in one transaction, before the first request.
`TestSearchSeesThroughFormatting` covers the match through marks, that the
marks themselves are not searchable, edit re-index, delete un-index, and the
rebuild running exactly once. The scale figure is unchanged (7.5 ms p95 over
200k messages; the index is the same size or smaller).

**Hover target.** The message action bar showed on `.msg:hover`, and `.msg` is
the full-width row, so pointing at the blank space beside a bubble raised the
bar. The trigger is now `.msg-body:hover`: the content-sized column that is
the bubble plus its name line, reactions and seen row. Measured on a short
message: row 742 px wide, body 200 px, bubble 147 px; the bar appears at the
bubble and not at the row's empty three-quarters. The bar is a child of the
body, so moving the pointer down into it keeps it open.

**Formatting toolbar.** Was a row of letters and emoji fading in on a passing
pointer. Now a card resting on the composer's top edge with icon buttons in
two groups (text styles; priority, send later, praise, clip), a rule between,
tooltips carrying the shortcut, and chips for whatever is set on the message
being written (IMPORTANT / URGENT / "Sends Thu 9:00 AM"). It appears while the
input has focus and can be pinned open with the Aa button (Ctrl+Shift+X,
persisted as `aether.fmtbar`); it no longer opens on hover, which flickered.

### 6ac. The desktop view is an application surface, not a browser tab

Reported: dragging (touch or precision touchpad) past a page edge made the
whole client bounce, and a sideways drag on the home screen navigated
"back" -- to the splash page, a history entry with a logo animation and no
title bar, where it stayed. Three habits of WebView2 that a browser tab wants
and an app window does not, fixed in three layers because each catches what
the others cannot:

- **Shell** (`cmd/aether-desktop/shell_windows.go`). `IsSwipeNavigationEnabled`
  is set false on the created view (the library does not export its browser;
  the field is read by name so a library upgrade fails loudly), an
  accelerator-key hook swallows Browser Back/Forward/Refresh, F5, Ctrl+R and
  Alt+arrows and nothing else (tested key by key), and the runtime is started
  with `--disable-features=OverscrollHistoryNavigation,msOverscrollHistoryNavigation`
  alongside the certificate pin, always.
- **Splash.** It now knows the server address and, reached by going back
  (`navigation.type === 'back_forward'` or a persisted `pageshow`), replaces
  itself with the app at once. The address is JSON-escaped into the script;
  tested against a `</script>` payload.
- **Client.** `overscroll-behavior: none` on the root (inner scroll areas
  chain to it, so one rule holds them all); in the shell the mouse's back and
  forward buttons are consumed on mousedown/mouseup/auxclick/pointerup, since
  Chromium navigates on the *unhandled* mouseup and the shell never sees
  mouse buttons; images, links and SVG are not draggable, so nothing lifts a
  ghost off the page.

Not measurable from this machine: the touch gesture itself. The setting call
returned without error on the installed runtime, which is the runtime's own
confirmation it took effect.

### 6ad. Block, find-in-chat, list filters; the shell behaves like an application

Against the Teams chat screenshot of 13 Sept: presence dots, header call
buttons, "Open in new window" and the Photos empty state already existed.
Built now:

- **Block** (`blocks.go`, migration 0043 `messaging_blocks`, PK
  `(user_id, blocked_id)` WITHOUT ROWID plus a reverse index). One person's
  decision; while it stands the *direct* conversation between the two
  carries nothing either way -- send returns `403 blocked` to both sides
  with the same words, rings are dropped before they are logged, typing is
  not relayed. Groups and channels the two share are untouched (tested).
  The viewer's list marks `blocked: true` on their own decision only. Bound
  at 1,000 (`MaxBlocks`), tested one over; unknown ids and yourself are 404.
  Harness: stranger, anonymous, CSRF, both directions, unblock, idempotence.
- **Find in this chat**: `GET /api/messaging/search?q=&conv=` -- the same
  trigram index narrowed by `conversation_id`; the membership join is what
  keeps a non-member's query empty (tested: somebody else's notes, a
  stranger). Client pane under the header, Ctrl+F, results jump to the
  message.
- **List filters**: Unread, Meeting chats, and a chevron pill for Chats,
  Groups, Channels, Favourites, Muted. Sections (Favourites / Chats) fold,
  remembered in `aether.sections`; a folded section keeps its unread total.
- **Header**: Add people (a 1:1 becomes a group with the other person
  preset), find, and a ⋯ that opens the row menu, now with Block/Unblock.

**Shell** (`subclass_windows.go`, `permissions_windows.go`). Close hides to
the tray; Quit from the tray menu lets WM_CLOSE through. Measured: after a
real WM_CLOSE the process is alive and the window hidden; restore brings it
back. Maximise sets WM_GETMINMAXINFO to the monitor's work area: measured
window (0,0)-(1280,752) against a 1280x800 screen with the taskbar visible,
where before the window covered the taskbar. Permissions are asked once, on
the first run after installing, on a screen in the product's own type and
colours (camera, microphone, notifications; switches on by default), saved
to settings and applied to the web view as WebView2 permission states on
every later start; "Permissions..." in the tray menu shows the screen again.
A ring while the window is in the tray restores it (`aetherShowWindow`).

Not verified from here: whether WebView2 shows the web Notification API as
Windows toasts; calls are covered by the restore-on-ring path regardless.

### 6ae. Attention from the shell; why "search finds nobody" on a public-provider deployment

**Attention.** The client decides when (unread total, a message while the
window is not looking, a ring); the shell decides how, because only a
native process can draw on the taskbar. Notifications go through the tray
icon (`NIF_INFO`), which Windows 10/11 renders as ordinary toasts in the
app's name and icon; a click (`NIN_BALLOONUSERCLICK`) restores the window.
The badge is drawn at run time into a 32-bit DIB -- a red disc with the
count (3x5 digit font, tested pixel by pixel) as the taskbar overlay via
`ITaskbarList3::SetOverlayIcon`, and the same disc composited into the tray
icon's corner. A ring flashes the taskbar button (`FlashWindowEx`) and, if
the window is hidden or minimised, raises a notification. Bindings:
`aetherNotify`, `aetherBadge`, `aetherRinging`. In a browser tab the
Notification API path is unchanged.

**"Searching not working" when making a group or channel.** Server log:
six directory queries from the app at 20:24 all returned `{"people":[]}`.
Cause: the directory reaches (a) people you share a conversation with, (b)
everyone at the organisation's own email domains, (c) one address typed in
full. This deployment's founding account is on Gmail, a public provider,
which `SeedOrgDomains` refuses -- so there is no (b), and a colleague never
chatted with cannot be found by name. That is the security rule working, not
a defect, but an empty list is the wrong way to say it. Now: the picker says
exactly what it reaches and what to type; an admin sees a link to the
console setting (organisation domains, or address book open to everyone);
local matching covers everyone in your groups and channels, not only 1:1s;
and channel creation has a member picker, as Teams does.

### 6af. Reactions as one pill; the shell's exact maximise; every device (rule 8)

**Reactions.** One pill under the bubble: the distinct emoji stacked (three,
then +N) and the total; lit when one is yours. The pill opens a popover with
one row per emoji -- names (You first), count, "yours · click to remove" --
and the quick reactions for adding one. Every path is a toggle: the same
emoji again removes yours. The server keys removal on the actor, so nobody
can take another person's reaction away; the harness now proves it (A's
"remove ❤️" leaves B's standing; two people on one emoji; each removal takes
only its owner's). The lit quick reaction is a soft round tint, not a boxed
outline, after that was reported as a "green rectangle".

**Maximise.** Client area equals the work area exactly: measured
(0,0)-(1280,752) on a 1280x800 screen with a 48px taskbar, frame at -6px off
screen. The frame width is measured from the window itself (outer minus
client) because the system metrics describe a captioned window and were one
pixel over.

**Every device.** `tools/e2e/layout.e2e.mjs` drives the installed Chrome
headless at 390x844 (touch), 820x1180 (touch) and 1280x800, signs in, walks
home, chat list, thread, find-in-chat, new group, members and calendar, and
fails on page overflow, elements leaving the viewport, or tap targets under
44px on touch. First run: the phone showed the rail as a 224px side column
(an old 900px rule rowed the rail but left `.home-main` in column 2) and
every control under 44px. Now: below 720px the rail is a bottom bar, chat is
two screens with a back control, dialogs are bottom sheets, the header keeps
back/avatar/name/call/⋯ with find and add-people in the ⋯ menu, the toolbar
opens only when pinned; 721-1024px keeps the desktop arrangement tightened;
`(pointer: coarse)` sets 44px minimums and opens the message action bar on a
tap. All three widths pass; screenshots in `tools/e2e/.shots/`. Not yet
walked by the harness: the in-call stage and the lobby.

**Meeting flow and the shell, in the layout harness.** The harness now
starts a meeting (Chrome's fake camera and microphone), audits the pre-join
lobby, the stage, its chat and people panels, and leaves; and `--shell`
stubs the Windows shell's bindings so the app's own title-bar layout is
audited at the window floor (940x620) and at 1024x700. Found and fixed on
phones: the controls bar ran off the right edge with Leave out of reach
(now wraps, Leave always on screen), the sub-header's bitrate text and
"Invite people" wrapped into a mess (stats hidden, invite icon-only), and
the pre-join mic/camera toggles, back button, host-control rows and the
side panel's Messages/Participants tabs were under 44px. Every viewport
and both shell sizes pass; the shell already used the desktop arrangement
because its floor is above the phone breakpoint.

**In-call main column (measured bug).** `.main` is a grid with no explicit
column, so its implicit column took the controls bar's intrinsic width: at
the app's 940px floor the main column was 572px wide and the stage 724px,
under the side panel. Fixed with `grid-template-columns: minmax(0, 1fr)`,
and the controls wrap below 1120px with the volume slider hidden. Both
shell sizes and all three device widths pass the harness after the change.

### 6ag. Availability, polls, @everyone, delete channel, voice messages

- **Availability** (`status.go`, migration 0044 `messaging_status`). Four
  states plus a note (120 runes, refused one over, not truncated -- the
  search-query cleaner truncates and was the wrong tool; `cleanLine` is the
  right one) and an expiry up to seven days, after which the row reads as
  available. Pushed as `OpStatus 0x11c` to the same audience as presence
  (everyone who shares a conversation) and to the person's own other
  devices. `GET /status?ids=` answers only for peers and yourself; the
  conversation list carries every listed member's status in one query.
  Client: the profile menu's top section; dots turn red (busy, dnd with a
  bar) or amber (away) while the socket is open; the thread header reads
  "Busy · In a meeting until 3"; do-not-disturb silences notifications and
  the ring's flash and toast (urgent still comes through).
- **Polls** (`polls.go`, `messaging_poll_votes` keyed message/voter/
  option). The poll is the message's meta, validated at compose (2-10
  options, 200/80 rune bounds, refused one over); a vote replaces the
  voter's previous choice, an empty vote withdraws; the tally rides on the
  push (option count, per-option count and up to twelve voter ids, distinct
  total -- one extra byte on a plain message, so the push-tail test is now
  seven bytes). "Mine" is per reader and is not on the wire: the client
  derives it from the voter list, or on an anonymous poll from what it
  clicked. Harness: stranger 403, bad option 400, two on choose-one 400,
  not-a-poll 400, tally on both sides' pushes, a moved vote, anonymous
  names nobody.
- **@everyone**: the client sends `*`; the server expands it to the members
  of a group or channel of 200 or fewer, each counted as mentioned; in a
  1:1 it names nobody.
- **Delete channel**: owner only; messages, files, reactions, votes,
  mentions, members and the row go in one transaction, blobs after commit.
- **Voice messages**: recorded in the composer (Opus/WebM, two-minute cap,
  live level meter), sent as an attachment; audio attachments play inline.

Found on the way: the read-path scale test had been silently broken since
the scheduling tranche (a hand-counted VALUES list shorter than the column
list; prepare failed, the first Exec dereferenced nil) and only ran without
`-race`, so nothing noticed. It now builds its placeholder list from the
column list and runs. Also fixed: an undelivered scheduled message could
become the other person's preview when it shared the conversation's
creation millisecond; previews now exclude pending messages.

### 6ah. Read receipts beside the message

Drawn from the members' read cursors, which the server already keeps and
pushes (`OpReadCursor`, small conversations only): no per-message receipt
rows and no new traffic. Beside each of your messages, on the inner side of
the bubble: one tick while nobody has read it, a filled double tick when
everyone has (in a 1:1, "Seen"), a double tick with a count while some have
in a group; the group icon opens a list of who has read (with when) and who
has not. Rules are pure (`web/src/receipts.js`, tested): never for other
people's messages, deleted or system rows, a scheduled message still
pending, notes to self, or conversations over 40 members. Verified live with
two signed-in browsers: the tick went from sent to seen the moment the other
person opened the thread, and the group receipt read "1 of 2" with the right
names on each side of the list.

The two-browser check also exposed a client accuracy bug: the read cursor
advanced on arrival whenever the conversation was the active one, so a
person on the Meetings tab, or with the window behind another, was "seen"
without having seen. The cursor now moves only while the thread is on
screen in a focused window (`isLooking`), and catches up on
`visibilitychange` / `focus`. Re-checked: the tick stayed at sent while the
other browser was on another tab (their badge read 1), and turned to seen
when they came back.

### 6ai. Channel threads, edit history, announcements, quiet hours

- **Threads** (`threads.go`, migration 0045 indexes on `reply_to` and on
  roots per conversation). Nothing new is stored: a reply is a message
  whose `reply_to` names the root. A channel's feed is its roots
  (`ListRoots`, one index seek), each carrying a summary (count, last reply
  time, up to three most recent repliers, rowid tiebreak for same-millisecond
  replies) from one grouped query over the page; a thread is `Replies`
  (member-checked); a reply to a reply hangs off the root, so threads are
  one level deep. Chats are untouched. Pushes are unchanged: the client adds
  a reply to its root's count once (a re-push for a reaction does not count
  twice) and to the open thread pane. Verified with two browsers: "2
  replies · E2E · 11:57 PM" on the root, two replies in the pane, and a
  bottom sheet on the phone.
- **Edit history** (`messaging_message_edits`): each edit stores the text it
  replaced, keyed by the edit's time; `GET /messages/{id}/history` is member
  only and empty for a deleted message. The "edited" tag opens the versions.
- **Announcements**: a meta card (headline 80, details 200, five colours),
  validated at compose and refused one over. The body is the headline, for
  previews and search; the card is what the thread draws.
- **Quiet hours**: on this device, a window in the day when nothing but
  urgent gets through, in the profile menu beside availability; overnight
  windows wrap.

Harness: 404 assertions. New sections cover roots-only feeds, reply
flattening, stranger/anonymous refusals on threads and history, version
order, and the announcement bound.

### 6aj. Bit-level measurement of the new push frames (tshark, loopback)

Method: a second, plaintext server on 127.0.0.1:8444 (so the WebSocket
frames are readable; `-media-port 8802` so it does not fight the real one),
two fresh accounts, B holding a push socket while A sends a plain message,
sets a status, posts a three-option poll, and B votes
(`tools/e2e/measure.mjs`). tshark on the Npcap loopback adapter with
`-f "tcp port 8444"`, then the server-to-client WebSocket frames read back
with `-e frame.len -e tcp.len -e websocket.payload_length`. Loopback here
was IPv4 (127.0.0.1): 4-byte loopback header, 20-byte IP, 20-byte TCP,
2-byte WebSocket header; every push was one frame in one packet. The JSON
baseline is the REST response body for the same event (what a client would
otherwise fetch) and its whole HTTP response packet.

| Event | Push payload | On the wire | Payload efficiency | JSON body | JSON packet |
| --- | ---: | ---: | ---: | ---: | ---: |
| Plain message ("hello there") | 89 B | 135 B | 65.9% | 197 B | 804 B |
| Status change (dnd + 17-char note) | 26 B | 72 B | 36.1% | 115 B | 717 B |
| Poll message, 3 options, no votes | 102 B | 148 B | 68.9% | 275 B | 882 B |
| Vote (tally: 1 vote, 1 voter id) | 110 B | 156 B | 70.5% | 360 B | 962 B |

Read honestly: the status frame is small enough that headers dominate it
(36% efficiency), which is the coalescing point CLAUDE.md makes; a status
change rarely coincides with other traffic, so it rides alone. The tally
cost eight bytes on the wire over the poll message itself (one count, one
interned voter id, one total). Against JSON over HTTP the pushes are 3x to
6x smaller per event; the comparison assumes the client would otherwise
refetch the message, which is what the pre-push design did.

### 6ak. The app supervises itself; the report that could not be reproduced

Report: "on zooming, the taskbar becomes invisible" and "closing the app
removes it from the tray as well". Measured on the current build, on the
same machine: maximised window (-6,-6)-(1286,758) against a 1280x800
screen, taskbar `Shell_TrayWnd` visible at (0,752)-(1280,800), no auto-hide;
WM_CLOSE and WM_SYSCOMMAND/SC_CLOSE both left the process alive with the
window hidden; Windows' Application log had no crash record. So the exit
that was seen is not reproducible from outside, and the run's output had
been lost with a temporary folder. That is the defect fixed here.

**Supervisor** (`supervise.go`). The executable runs in two roles: the
parent starts itself as the child (`-child`), copies the child's output --
a Go panic's stack included -- to `%APPDATA%\Aether\logs\desktop.log`
(kept under 5 MB), and restarts the child if it dies of anything but a
clean exit: exit code 2 (Go panic/fatal) or a Windows exception status
(negative or over 255). A configuration error (1) or a second copy finding
the first does not restart. Bounded: three restarts in ten minutes, then it
stops and says so. Policy tested with numbers; live: `Stop-Process` on the
app (exit code 4294967295) brought a new app process up 1.5 s later with
its tray icon, and close on the relaunched child still hid rather than
quit.

**Attention paths on a hidden window** (`attention_windows_test.go`): a
real tray window, a real notification-area icon, Notify / SetBadge / Flash
several times over, nil tray, and the overlay against a window that does
not exist -- none of it crashes.

If the disappearance happens again the log will say why; that is the next
thing to read.

### 6al. Four reported defects, fixed and checked

- **Thread pane left the feed narrow.** `with-thread` was added to the
  thread element when a pane opened and never removed, so closing the pane
  or switching chats kept the two-column grid. Now the class follows the
  state both ways. Measured in the browser: log 792 px wide, 432 with the
  pane open, 792 again on close, 792 after switching chats with a pane open.
- **Aa would not close.** The toolbar was a remembered pin that only Aa
  could undo, and it also appeared on focus. Now Aa is a switch: one click
  opens, another closes, a click anywhere else closes (dialogs the toolbar
  opened do not count as elsewhere), and focus no longer opens it. Checked:
  opacity 1, 0, 1, 0 through open / click again / open / click outside.
- **⋯ would not close on a second click.** The document-click closer ran on
  the button's own click and then the button reopened the menu. The opener
  now closes when the menu it would open is already open, and the header
  button stops its click from reaching the closer. Checked the same way.
- **Replies to you notify.** The quote on the wire now carries the quoted
  message's sender id (one interned string), so a reply to something you
  wrote is treated like a mention: it notifies through mute and quiet hours
  (not through do-not-disturb) with "X replied to you in #channel"; a
  mention says "X mentioned you in …"; a message in a conversation the
  client has never seen notifies before the list is refreshed, so a mention
  in a brand-new group is not lost. Harness asserts the quoted sender id on
  the push.

Also: the scheduled-send test let the clock pass its due millisecond, and
the read-path scale test's list p95 (17.5 ms alone, budget 50) had read 79
ms once with the e2e harness and Chrome running beside it -- the number to
trust is the one taken alone.

### 6am. The second report, and the stale copy

Report repeated: maximise hides the taskbar, close removes the app. The
persistent log for the current build showed a clean exit (code 0) at
00:44:59 -- no crash, nothing restartable -- and every message path
(WM_CLOSE, SC_CLOSE, SC_MAXIMIZE) on the current build measured correct
again. What the machine also had: `aether-desktop.exe` at the repository
root dated 11 Sept, before the shell subclass and the supervisor existed
(checked: the old binary contains no `SetWindowSubclass`, the new one
does), carrying the same single-instance lock. Started when the current
app is not running, it behaves exactly as reported. No pin or shortcut
pointed at either copy, so the launch was from the folder. The stale copy
is now replaced by the current build after every build, and the app logs
its own path and build time on start (`exe=`, `built=`), so a log line
identifies the binary a report is about.

### 6an. Automatic status: only a real call sets it

Asked for: the status dot on every avatar (own, header, chat rows), an
automatic status while in a meeting, and two states nobody can pick, "In a
call" and "Presenting". The first design also let the calendar set a status
the moment a scheduled meeting began; that was withdrawn before it shipped,
on the owner's decision: a scheduled meeting nobody has joined is not a
call. What is built:

- **Source of truth is the room registry**, not the calendar. `LiveAccounts`
  is one pass over the rooms: account id → sharing?, guests left out. The
  messaging service folds it into every status read (`effective`):
  presenting, then in a call, when the person is in a room right now;
  otherwise whatever they picked (an expired pick reads available), or
  nothing. Both auto states outrank a manual pick because they are facts
  about the person's microphone and screen, not preferences; the pick is
  kept underneath and is what peers see again the moment the call ends.
- **The change is pushed the moment it happens.** The signal gateway calls
  `OnPresence` after a join, after the leave has been applied to the room,
  and on a state delta that flips `Sharing`; messaging's loop wakes on that
  nudge (one-deep channel, never blocks), diffs the reading against the
  last one, and pushes the effective status to the person's peers and to
  their own devices. A 15 s tick is the safety net only. When nothing was
  picked, leaving pushes "available" out loud so the dot that turned red
  turns back.
- **Manual `incall`/`presenting` are refused** (400) by `SetStatus`; the
  picker offers the four manual states and shows the automatic one as a
  read-only line ("In a call · automatic") when it applies. `OpStatus`
  gained one bit (`auto`) plus 7 bits of padding: one byte.

Measured, tshark on loopback, plaintext server, IPv6 (40 B) + TCP (20 B) +
WebSocket (2 B) + payload:

| Push | payload | on the wire | efficiency | JSON body |
|---|---|---|---|---|
| Status change (dnd + 17-char note), before the auto bit | 26 B | 72 B | 36.1% | 115 B |
| Status change, with the auto bit | 27 B | 73 B | 37.0% | 114 B |

Live, with a real headless Chrome joining an instant meeting and a second
account polling the API (`tools/e2e/livestatus.e2e.mjs`, 11 checks):

| Moment | seen by the peer after |
|---|---|
| Join click → "In a call", automatic | 44 ms |
| Leave click → the pick made during the call (DND), not automatic | 164 ms |

Also checked there: picking DND during the call is stored but still reads
"In a call" to peers and to the person; A's own page holds the state from
the push, not a reload. Unit tests: `effective` precedence and refusal of
the auto values; the loop test drives `RunAutoStatus` with a one-hour tick
and proves the nudge is what delivers (with the nudge case removed from the
select it fails, "waited for 2 pushes, got 0"); the diff function; and
`LiveAccounts` across two rooms with a guest and one person presenting from
a second device. Full suite green under `-race`; harness 405/0; layout
harness green at all three widths.

Not done: the calendar does nothing to a status, by decision; a "presenting"
run through the live check is not automated (headless Chrome has no display
to share), so that state is proven at the unit level and by the same
`Sharing` flag the media plane already trusts.

### 6ao. One status glyph set, everywhere a person is drawn

Asked for: the status visible on every avatar -- chat rows, the chat
header, group members, meeting participants, the directory, the profile
menu -- with Away drawn as a clock and the set made properly.

- **`web/src/presence.js`** is the single source: a filled disc with a
  shape cut out of it in the card colour, so each state reads by shape as
  well as colour. Tick = available; plain disc = busy; bar = do not disturb;
  clock face and hands = away; handset = in a call; screen = presenting;
  hollow ring = offline. `presenceIcon(status, {online, note})` is pure and
  unit-tested (six distinct drawings, offline overrides any status, an
  unknown status draws as available, the note rides escaped in the title).
- **Drawn from one function on the client**, `personPresence(userId)`: the
  person's status while their socket is up, the ring when it is not. Chat
  list and header already used it; the group members dialog now fetches the
  members' statuses in one bounded query (≤200) before painting; the
  directory rows show the person's status rather than a bare online dot;
  meeting participants show in-a-call, or presenting for whoever is
  sharing, from the room state already on screen; the profile menu's four
  options carry the glyphs; the web bar's own avatar had no glyph at all
  (only the Windows title bar's did) and now has one.
- **An accuracy defect found while looking at the screenshots.** A presence
  push only ever said "X arrived". A client that connected *after* its
  peers never heard it, so it drew everyone offline until they reconnected
  -- the hollow ring on a person who was there. The socket accept path now
  sends the new socket one presence frame per online peer (`OnlinePeers`:
  PeersOf ∩ live sockets; to that socket alone, not the person's other
  tabs). Harness: a late socket for A hears "B is online" in 5 ms without B
  doing anything. Cost, from the frame layout measured in §6aj: a presence
  frame is a header, an interned id and a byte, inside 62 B of framing, once
  per online peer per connect -- and only at connect. It is not coalesced
  into one message; the client reads one frame per WebSocket message, so
  coalescing here means a list op, noted as not done.

Checked: web unit tests 123/123 (six new); messaging harness 406/0;
messaging, signal and room packages green under `-race`; layout harness
green at phone, tablet and desktop; screenshots at desktop and phone looked
at: clock on Away, bar on Do not disturb, tick on Available, ring on
offline, in the chat list, the members dialog and the profile menu.

### 6ap. Presence, properly: last seen, appear offline, idle, grace, the card; and the transport

Asked for, after the glyphs: why anyone reads "offline" without choosing
it; last seen, the way Teams does it; the header to show the name and not
the address; a card from any name or avatar; and "everything on the network
super encrypted". Teams' behaviour was read first (Microsoft's presence
page and Q&A): Away after about five idle minutes or a lock, Offline when
every client is gone, "Last seen" shown for Away and Offline, an "Appear
offline" choice that hides activity and last seen, and a profile card from
a name or photo with message / call / email actions.

**What "offline" means now.** A person reads offline in exactly two cases:
no socket of theirs has been up for `OfflineGrace` (10 s), or they picked
Appear offline. Nothing else. The earlier "auto offline" complaint had two
causes, both fixed: a client that connected after its peers was never told
they were online (§6ao), and a reload or a network blip announced a
departure the instant the socket dropped. The grace covers the second:
`hub.Online` says online for 10 s after the last socket closes, every
reader (the status query, the directory, the connect snapshot) goes through
it, and the departure is announced only if nobody has come back by then --
carrying the last-seen time stamped at the close, not at the end of the
grace. A reconnect inside the window says nothing to anybody (harness:
close, 800 ms, reconnect: no departure, no second arrival).

**Last seen.** `messaging_presence(user_id, last_seen)` is stamped when the
last socket closes and when a device reports idle. It rides on the status
read (`lastSeen`, with `online`), on the status push (one bit for online,
then a varint), and on the departure frame -- and is shown for Offline and
for an idle Away, never for an Away somebody picked while typing.

**Away, automatically.** `web/src/idle.js` (pure, tested with an injected
clock): five minutes without pointer, key or touch on this device, and the
client sends `OpIdle`; the first input sends it back. A phone in the
background is idle at once. The server folds devices: a person is idle
when every device is (`hub.Idle`), which reads as Away automatically
unless they picked Busy, Do not disturb or Away themselves. Going idle
stamps last seen, so "Away · Last seen 12 min ago" is the last input, as
in Teams.

**Appear offline** is a fifth manual pick, `offline`: peers are told
"left" (no last seen) when it is chosen and "back" when it is lifted; the
read says offline and nothing more; the directory's Online() says no; the
connect snapshot leaves them out; even a live call reads offline. The
person sees their own pick.

**The header and the card.** A direct chat's header is the name and the
state line ("Away · Back at 3", "Offline · Last seen 2 h ago"); the
address is gone from it. The name and the face there, a message's author
line, a row in the members list and a person in a meeting's people panel
all open the same card: face with glyph, name, state line, address, and
Message / Audio call / Video call (none for yourself). Full-height sheet
on a phone; the header face and name are 44 px targets on touch (the
layout harness caught them at 40x40 and 37x24 first).

**Transport.** The HTTPS server is TLS 1.3 only (`cmd/aether/tlspolicy.go`);
a real handshake test refuses a 1.2 client and serves 1.3 with an AEAD
suite, and the live server answered the same way (`ERR_SSL_TLSV1_ALERT_
PROTOCOL_VERSION` for 1.2; `TLS_AES_128_GCM_SHA256` for 1.3). Media is
DTLS-SRTP by construction; the SFU now prefers `SRTP_AEAD_AES_256_GCM`,
then 128-GCM, with the classic AES-CM/HMAC-SHA1 profile last so an old
Safari is heard rather than silent. What is *not* done, by decision put to
the owner: end-to-end encryption of message bodies. It would take server
search, read receipts on content and multi-device history with it, or a
key-distribution protocol of its own; the database on disk is not
encrypted either. Recorded as not built, not as done.

**Two bugs found on the way.** (1) The group-history hide used `created_at
< joined`: a message sent in the same millisecond as the add leaked to a
newcomer told "no history". Now `<=`. (2) The read cursor at join equalled
the join instant, and the counters count `created_at > last_read`: a
message in the join millisecond went uncounted ("B's mention counter: got
0, want 1", once). The cursor now sits one millisecond before the join;
`samems_test.go` failed against the old code (unread 0) and passes.

**And one in the harness.** `nextWhere` handed a frame to the first waiter
and dropped it if that waiter did not want it; a timed-out waiter stayed
registered and swallowed the next matching frame. For an hour that read as
"the departure never arrives". Rewritten: non-matching frames stay in the
buffer, a timed-out wait withdraws itself, every wait names what did
arrive when it fails.

Measured, tshark on loopback, plaintext server, IPv6 40 + TCP 20 + WS 2 +
payload:

| Push | payload | on the wire | JSON body |
|---|---|---|---|
| Status change (dnd + 17-char note), with auto, online, last seen | 28 B | 74 B | 130 B |
| Presence snapshot to a fresh socket (one peer online) | 5 B | 51 B | -- |
| Arrival announced to a peer | 21 B | 67 B | -- |
| Departure after the grace, with last seen (ms varint) | 10 B | 56 B | -- |

The pairing of the two 5.51 s frames (snapshot to the new socket, arrival
to the peer) is inferred from the layout; both were captured, neither is
attributed by the capture itself.

Checked: Go suite green under `-race`; `go vet` clean; web unit tests
127/127 (idle watch: on-the-line minute, input resets, phone-background;
presence line wording); messaging harness 420 assertions / 0 failed,
including the grace timing (departure 9-16 s after the close, last seen
within 2.5 s of the close), appear offline hiding arrival, departure and
last seen, and the snapshot; the live browser check (join click to "In a
call" at the peer 32-82 ms, leave click to the person's own pick 160-169
ms); layout harness green at phone, tablet and desktop; screenshots looked
at at desktop and phone: header with name and state line, the card, the
picker with Appear offline as a hollow ring, the members list with state
lines.

### 6aq. Live behaviour between two accounts: the tick, the typing, your other devices

Reported from the desk, with the same person signed in on the app and on
the web as two accounts: "X is typing" stayed on screen; the double tick
came only after switching chats; a message sent from one place did not
show up in the other until a switch. Reproduced first with two real
browsers (`tools/e2e/live.e2e.mjs`), then fixed. Three causes, none of
them the one it looked like.

**Read means read, but unread meant "another chat".** The client only
counted a message as unread when it arrived for a conversation other than
the open one. A message into the *open* thread while the window was behind
another was neither read (the cursor moves only while looking, by rule)
nor counted -- so when the window came to the front, the catch-up, which
looks at the count, did nothing. The sender's tick stayed single until the
reader switched chats, which refetched. Now unread means "not yet seen":
the count rises for an open thread too when the window is not focused, and
the catch-up on focus marks it read. Live: tick single while the reader's
window is behind, double 88-93 ms after it comes to the front.

**Typing never lapsed by itself.** The indicator's expiry was checked only
when something else redrew the thread. Redesigned end to end
(`web/src/typing.js`, `typinggate.go`), for the wire first:

- *Sender.* "Typing" at the first character, again at most every 3 s while
  the text keeps changing; "stopped" when the box empties, when the
  message goes, when the typist pauses 5 s, when the tab is hidden or the
  conversation is switched. A burst of keys is one frame.
- *Server gate.* One "typing" per person per conversation per 2.5 s is
  relayed; a "stopped" only for somebody the audience saw start. A client
  that signals on every keystroke -- or a hostile one -- costs the server
  one map lookup per keystroke and the audience nothing (harness: twenty
  signals inside the window, zero relayed). Bounded table, swept. Sending
  a message relays a stop for the sender whether or not the client said
  so, so nobody watches dots after the message arrived.
- *Receiver.* A board per conversation per *person id* (two Sams are two
  people), a 6 s life per signal so a live typist never lapses and a dead
  one lapses on time, one timer for every typist everywhere armed for the
  earliest lapse. "Priya is typing", "Priya and Sam are typing", "3
  people are typing". Painted in place -- the line changes text and
  visibility without redrawing the thread -- and shown in the list row
  ("Priya is typing…") for a conversation that is not open. The line keeps
  its height when hidden so the thread does not jump.

Measured, tshark on loopback, plaintext, IPv6 40 + TCP 20 + WS framing:

| Frame | payload | on the wire |
|---|---|---|
| typing on, client→server, first time in a conversation on this socket | 20 B | 70 B |
| typing off, client→server (conversation already interned) | 4 B | 54 B |
| typing relayed to a peer, on or off (all three strings interned) | 6 B | 52 B |

Read honestly: framing is 90% of a typing packet, so the whole design is
about frame *count*: one per 3 s while typing continues, one to stop, and
a storm reduced to nothing at the server. Live: the indicator appears on
the other side 1 ms after the first key, clears 1 ms after the box empties,
lapses by itself 5.6 s after the last key, and is gone 91 ms after the
message that ends it.

**Your other devices never got your message.** Fan-out skipped the sender
outright, so the app did not show what the browser sent until a refetch.
Each tab now carries a random device id (session storage) on its socket
(`/ws?d=`) and on every request (`X-Aether-Device`); a message is pushed to
the sender's other devices and never to the one that sent it, which has
the response. A client without the header keeps the old contract: nothing
of its own is pushed. The composer places a message once whichever comes
first, the response or the push. Harness: tab 2 has it, tab 1 is not
echoed, B has it, a device-less socket has it.

Checked: harness 432 assertions / 0 failed (typing storm, stop once, stop
on send, phantom stop; the two-tab delivery); live check 20/20 across
message arrival, tick, typing lifecycle, own corner, peer header; unit
tests for the gate (coalescing, stop-once, sweep) and the board and sender
(wording, TTL on the line, repeat, quiet, send); web 131/131; full Go suite
under `-race`; layout harness green.

### 6ar. "I hear my own voice": what was measured, what was fixed, what cannot be

Reported: an echo of the speaker's own voice in a call. Investigated by
measurement before any change, with two real browsers in one meeting
(`tools/e2e/audio.e2e.mjs`), both publishing a microphone (Chrome's fake
device: a tone), reading what actually flows for 3 s and what actually
plays:

| Side | media path | sent | received | sinks | sink playing | own audio played back | mic AEC |
|---|---|---|---|---|---|---|---|
| A | socket | 197 frames / 290 KB | 202 frames / 294 KB | 1 (B's id) | peak RMS 0.254 | no | on |
| B | socket | 201 frames / 295 KB | 197 frames / 281 KB | 1 (A's id) | peak RMS 0.227 | no | on |

The two sinks name different people: had either side been played its own
audio, both pages would have named the same id. No `<video>` is unmuted.
On the server, the SFU never subscribes a publisher to its own track
(`everyonepublishes_test.go`: 12 publishers, 24 tracks, none fed back) and
the socket relay excludes the sender (`mediarelay_test.go`, real sockets:
the peer reads the frame, the sender's socket stays silent; with the
exclusion removed the test fails, "the sender was relayed its own media").
The receiver plays each stream once (Web Audio → MediaStreamDestination →
one `<audio>` per publisher). So the software path does not return a
voice to its owner, before or after this work.

What does: acoustics. A voice comes back only through a loudspeaker
reaching a microphone -- the other person's, or a second client on the
same desk. Echo cancellation subtracts what *the same browser* is playing
from what its microphone hears; it knows nothing of what another
application on the same machine plays. Two clients on one laptop (the app
and a browser, two accounts) therefore always echo each other, in Teams as
here, unless one is muted or wears headphones. That is the exact setup
these reports came from.

Fixed and added, each with a test that fails without it:

- **Echo cancellation as a requirement** (`web/src/echo.js`): the
  microphone is opened with echo cancellation, noise suppression and gain
  control as exact constraints; a device that refuses is used anyway (a
  voice with echo beats no voice) and the person is told once -- "Others
  may hear themselves. Use headphones, or pick another microphone" -- in
  the pre-join notes and as a toast. The applied settings are exposed on
  the document for the checks; both live checks assert `aec=on` in a real
  browser.
- **Same-device advisory**: the room keeps the host part of each
  connection's address; when a newcomer shares it with somebody already in
  the room, one frame (`OpSameDevice`, a header and a varint) goes to the
  newcomer and to each person on that address, once, at join. The client
  says: "Someone else is in this meeting from this same device or network.
  To avoid hearing yourself, mute one of the microphones, or use
  headphones." Real-socket test: a and b on one address are both told
  (count 1); c on another address is told nothing and its arrival tells
  nobody; d on the shared address raises everyone there to 2. With the
  check disabled the test fails ("a: told nothing, want 1 others"). Both
  browsers of the audio check, being one machine, saw the warning 1-2 ms
  after the join.
- **Screen share asks for no system audio.** It did (`audio: true`), which
  put a "share system audio" checkbox in the picker that published
  nothing; had it ever been wired through it would have carried the
  meeting's own loudspeaker back to everyone.

Noted, not changed here: the media plane runs on the socket path by
default (`decideMediaMode` returns `ws` unless `?media=webrtc`); the
measurement above is of that path, at roughly 780 kbit/s per direction per
participant for a camera and a microphone. That default is a decision for
its own section.

Checked: signal and room packages green; web 136/136 (echo.js: strict-then-
fallback, the report's wording, no softening of a denied permission);
audio check 21/21; the in-call live check asserts AEC on and no self sink;
messaging harness, both live checks and the layout harness rerun on the
new build (results below this line are from those runs).

### 6as. Audio, measured: the two transports, the receive clock, and what "fast" means here

Asked: is audio intact after the echo work, and is WebRTC better for audio
-- test, and apply if so. Everything below was measured with two real
browsers in one meeting (`tools/e2e/audio.e2e.mjs`), before any change and
after each one.

**The first measurement was wrong, and that is recorded here on purpose.**
Chrome's fake microphone (`--use-fake-device-for-media-stream`) *beeps*,
with silence between beeps. A continuity check read the silences as
dropouts (25 of 34 windows), and a latency check that waited for sound
after an unmute waited for the next beep -- numbers of 500-700 ms with a
50-950 ms spread that said nothing about the transport. The harness now
replaces the microphone with a synthesised continuous 440 Hz tone (mono, so
the encoder is configured as for a real microphone) and steps its gain to
measure. Every figure that follows is from that harness; the earlier ones
are discarded.

**Socket path, before and after.** The receive clock was a fixed 150 ms
lead with a 400 ms ceiling; it drifted up and sat around 300 ms
(`jitter.js` explains why). It is now adaptive: a 60 ms floor, +20 ms per
underrun up to 400, excess judged by the low-water mark over a 50-chunk
window and worked off at twice real time, decay back after ten quiet
seconds. Pure, unit-tested (steady, bursty-but-normal untouched, underrun
growth, start-up burst absorbed, decay, reset). The first adaptive draft
judged each chunk on its own and tore the audio apart (measured, discarded).

| glass to glass, audio only, loopback | median | spread | receive side of it |
|---|---|---|---|
| socket, fixed 150 ms lead (before) | 348 ms | 343-355 | 305 ms |
| socket, adaptive clock (after) | 170-179 ms | 163-182 | 131-133 ms |
| WebRTC | 80-83 ms | 78-88 | jitter buffer 30 ms |

Sender capture+encode+batching is 19-30 ms and wire+server 15 ms on both
socket runs; the whole gain is the receiver. Both paths play continuously
(0 silent windows of 34-36 over two seconds), zero loss, one sink each,
nobody played their own audio, both told they share a device.

**Unmute.** The mute button swapped the track out of the WebRTC sender and
back; the far side then resynchronised a stopped stream, and unmute-to-heard
measured 58 / 540 / 1002 ms. Mute is now a silenced live track on both
transports (the sender is touched only when it has no track yet):
unmute-to-heard 96 ms on WebRTC, 181 ms on the socket -- the transport
latency plus a click.

**Two robustness fixes found by the harness.** The socket encoder was
configured from the track's *settings*; a track that said mono and
delivered stereo made it refuse the first buffer and close for good --
silence for the rest of the call with one console line. It is now
configured from the audio that arrives. And a browser without WebCodecs
(Safari, iOS, Firefox) was shown a toast and given no media at all; it now
takes WebRTC (`chooseTransport`, tested; the harness emulates such a
browser and measures the same 82 ms).

**The default stays the socket, and here is the reasoning.** WebRTC is
twice as fast here and is the only path every browser has. But the socket
became the default (§6h) because on this deployment's real networks -- a
campus LAN and a phone hotspot -- the WebRTC publisher failed to form a
pair on nearly every call, and a transport that switches mid-call glitches
mid-call. Loopback cannot test that. So: Chromium (Chrome, Edge, Android
Chrome and WebView, Windows WebView2) stays on the socket, now at 170 ms;
every other browser gets WebRTC automatically; `?media=webrtc` opts a
Chromium session in. When two-device tests on the real network show the
WebRTC pair forming reliably (the harness prints the transport in use and
the latency), the default flips on that evidence -- not on loopback.

**Compatibility, since the Android app is coming.** Both transports are
standard web platform: WebRTC everywhere; WebCodecs and
MediaStreamTrackProcessor on Chromium (Android WebView included). Nothing
here is Windows-only; the desktop shell is WebView2 around the same client.
An Android app that wraps the same client in a WebView gets both paths;
one built natively would use the same WebRTC SFU and the same binary
protocol.

Guarantee, stated exactly: what is guaranteed is what the harness asserts
on every run -- continuous playout, one sink per peer, no self-playback,
transport in use, and a latency ceiling -- on the machine it runs on. It
does not, and cannot, guarantee a network it has not been run on.

Checked: web unit tests 143/143 (jitter clock, transport choice, echo
guard); audio harness green on the socket, on WebRTC, and as a
no-WebCodecs browser; in-call live check green; layout harness green at
three widths including the meeting flow on the socket path.

### 6at. Mute, unmute, and a hundred people: measured

Asked: rigorous mute/unmute latency, and what a hundred-person room does
to audio. Measured with the two-browser audio harness (synthesised tone,
§6as) and a new hundred-socket relay test; changed where the numbers
demanded it; measured again.

**Mute and unmute.** Mute is a silenced live track on both transports
(§6as). New: a muted socket sender *sends nothing* -- a disabled track
still delivers frames of zeros, and encoding and relaying them cost every
other participant a frame every 20 ms per muted person. The receive clock
treats the resulting gap as a pause, not an underrun (`GAP`, 250 ms; unit
test: twenty mute/unmute cycles leave the target at the floor). Stress: the
mute button pressed twenty times 80 ms apart, ending unmuted.

| audio only, loopback | unmute → heard, before stress | after 20 rapid presses | playout after |
|---|---|---|---|
| socket | 137 ms median | 69 ms median (64-69) | continuous, 0 silent windows |
| WebRTC | 91 ms median | 93 ms median (93-94) | continuous, 0 silent windows |

The socket figure after the stress is *lower* than the transport latency
because the clock anchors afresh at the 60 ms floor after each pause. No
wedge: the signalling chain that once jammed on quick toggles (`_enqueue`)
holds.

**A hundred people.** `fanout_scale_test.go`: a gateway with 100 real
socket connections in one room, every recipient checked for every frame
meant for it, in order. Two shapes:

| room | frames relayed | deliveries | wall | relay CPU | per recipient |
|---|---|---|---|---|---|
| 3 talking, 97 muted | 300 (2 s) | 29,700 | 2.000 s, real time | 1 µs per frame, ~0% of a core | 11.8 KB/s (94 kbit/s) |
| all 100 talking (ceiling) | 5,000 (1 s) | 495,000 | 1.003 s, real time | 19 µs per frame, 9.4% of a core | 394 KB/s (3.2 Mbit/s) |

Every frame arrived, every recipient, in order, both shapes. The ceiling
is the row that muted-sends-nothing removes from real life: before it, a
room of a hundred with three talking cost each person the 3.2 Mbit/s row
(ninety-seven streams of silence); now it costs the 94 kbit/s row. On the
receive side, every socket receiver used to own an AudioContext -- a
hundred of them in a hundred-person room, more than browsers allow; they
now share one, with a destination each.

**What is not here, and is the next real limit.** A hundred people all
*talking* is not a meeting; three to five at once is. Zoom and Teams
forward only the loudest few speakers' audio to everyone. This server
relays every unmuted sender; a person who is unmuted but silent still
sends (Opus DTX is deliberately off in the WebRTC path because it clips
the first syllable -- see `tuneOpus` -- and the same reasoning applies to
gating the socket sender on a level threshold). Active-speaker selection
at the server, using the speech energy participants already report, is
the next step for rooms where many stay unmuted; it is not built, and the
ceiling row above bounds the cost until it is.

Checked: web unit tests 144/144; audio harness green on both transports
including the stress; hundred-socket relay test green, also under `-race`;
in-call live check and layout harness rerun on the changed client.

### 6au. Everything on the wire: the inventory, measured, with what changed

Asked: check every byte that crosses the network -- messages, status,
video, everything -- for compression, encryption and waste. This section
is that inventory. Each row is a real measurement (tshark on loopback, or
the browser harness's per-message socket log), not a calculation.

**Encryption, by path.** Pages, API, both WebSockets: TLS 1.3 only, AEAD
suites (§6ap). WebRTC media: DTLS-SRTP, AES-GCM preferred (§6ap). Socket
media travels inside the same TLS session as the page. Not encrypted: the
database file on disk; message bodies are not end-to-end encrypted
(decision recorded in §6ap; it would remove search, receipts and
multi-device history).

**Compression, by path, and why some paths must not be compressed.**

| what | compression | measured |
|---|---|---|
| page, scripts, stylesheet | gzip, new; Cache-Control no-cache with Last-Modified, new | first load 645 KB → 210 KB; an unchanged file thereafter: 304, 0 B body (was 645 KB again, under no-store) |
| API JSON | gzip when the body reaches 512 B; held back below that, so a 44 B answer is not made 60 B | list of conversations, statuses: compressed; healthz: raw |
| chat push frames | binary protocol, string interning (an id is 1 B after its first use), 15 ms coalescing | plain message 89 B in 135 B on the wire; status 28/74; typing 6/52; presence arrival 21/67 |
| meeting signalling | same protocol; state delta 3 B | -- |
| Opus, VP8 over the socket | none, deliberately: already compressed, deflate expands them | audio frame 122 B carries 120 B of Opus |
| WebSocket permessage-deflate | off on both sockets, deliberately (same reason) | -- |
| files and images | served as stored; a JPEG is not re-compressed | -- |

**The audio frame, byte by byte, before and after this pass** (48 kbit/s
Opus, 20 ms, client to server; server to client adds one interned byte for
the publisher):

| field | before | after |
|---|---|---|
| frame header | 2 B (opcode above 0x7f) | 1 B (moved to one-byte opcode 14, displacing the rare in-meeting chat op) |
| kind, key, spare | 1 B | 1 B |
| capture timestamp | 1-4 B varint | 0 B: audio chunks are contiguous, the receiver counts them |
| payload length | 1 B | 0 B: the payload is the frame's tail |
| Opus | 120 B | 120 B |
| **frame** | **126-127 B** | **122 B** |
| on the wire (loopback, per packet) | 179-182 B | 171-174 B |

tshark, two directions, 3,200-3,300 frames each: payload efficiency 70.3%
→ 72.4% server to client. What remains is IP + TCP + WebSocket framing --
around 50 B per packet, 30% -- which only fewer packets would reduce.
Opus can pack 40 ms per frame and halve the packet count for about 15% of
the audio bandwidth; that costs 20 ms of latency on a path whose latency
was the complaint, so it is not done, and the number is here for the day
the trade is wanted.

**Video on the socket path.** Camera on, 600 kbit/s VP8, 20-ish fps in the
headless harness:

| | before | after |
|---|---|---|
| keyframe cadence | every 2 s, unconditionally | every 10 s as a safety net; on demand otherwise |
| keyframes in 6 s | 2-3, 10-18 KB each, 3-9% of all video bytes | 0-1 |
| video deltas | 118-120 in 6 s, avg 2.6-2.8 KB, 51-56 KB/s | unchanged |
| late joiner decoding the other's video | yes | yes (asserted on both sides) |

On demand means: a receiver that just joined, whose decoder erred, or that
got a delta while waiting asks the publisher through the server
(`OpKeyframeReq` naming the publisher; the server forwards it to that
socket publisher at most once per 300 ms however many watchers ask; the
publisher's next frame is a key). Real-socket test: three watchers ask at
once, the publisher is asked once; after the window, again; for nobody or
oneself, never. With the forwarding disabled the test fails ("the
publisher was not asked").

**Presence and status, per event** (§6an-§6ap): status change 28 B payload;
arrival 21 B; departure with last-seen 10 B; connect snapshot 5 B per
online peer; typing 6 B, at most one per 2.5 s per typist, none for a
storm. A muted socket sender sends nothing (§6at). Idle: one frame per
state change.

**What is not built, named.** Server-side active-speaker audio selection
for rooms where many stay unmuted (§6at). Content-hashed asset names with
long-lived caching (today: revalidate on every load, 304 when unchanged --
a few hundred bytes per asset). Opus 40 ms frames (above). End-to-end
encryption of message bodies (§6ap).

Checked on this build: wire round-trip tests pin the audio frame at
payload+2 and the video frame at payload+5, on both sides; gzip middleware
tests (compressible only, not upgrades, not small, not already encoded,
hold-back correctness); keyframe request test; web 147/147; two-browser
audio on socket and WebRTC with stress; camera-on run with the late-joiner
check; messaging harness, both live checks and the layout harness rerun;
full Go suite under `-race`.

### 6av. Reactions in a call, and the two chats of a one-to-one call

Reported: reactions not working in a meeting and too few of them; the other
side of a one-to-one call seeing a nameless group chat instead of the
conversation the caller saw; and a wish for both kinds of chat in a call
-- the kept conversation and one that ends with the call. Reproduced with
two real browsers (`tools/e2e/call.e2e.mjs`: A rings B from the chat
header, B accepts in the dialog, both land in the room), then fixed.

**Reactions.** The picker opened on hover, so it did not exist on a phone
and vanished when the pointer moved; six reactions. Now it opens on a
click, closes on a pick, on a click anywhere else and on Escape, offers ten
(👍 ❤️ 😂 😮 😢 👏 🎉 🔥 🙏 👀) in two rows with 44 px targets on touch, and a
reaction rises over the stage with the sender's name, drifting a little
sideways, gone in 2.6 s. Nothing is stored. Measured: the float appears on
the sender's own stage 87 ms after the click and on the other side 1 ms
after that (both are told by the same server frame).

**The one-to-one call chat.** The panel opened on the two of you only when
the room was marked as a call by this client. Two signed-in people in a
room are a conversation whatever brought them there, so it now opens on
them in any two-person room -- the kept thread, with its history, on both
sides (B's panel drew 61 messages of history). A bug hid behind it: the
automatic choice re-applied on every roster redraw, so a person who chose
the call chat was silently switched back and a message meant to vanish
with the call landed in the permanent conversation. The check caught it
("and it is NOT stored anywhere" failed). A choice is now remembered for
the meeting.

**Both chats, named.** The selector in a one-to-one call reads "E2E B ·
kept" and "Call chat · not kept". The kept one is the same conversation as
the sidebar's, stored on the server; the call chat rides the meeting socket
as `OpChat`, is relayed and never written anywhere. Asserted: a kept
message is listed by the API and still there after the call; a call-chat
message reached the other side in 81 ms and is listed nowhere, during or
after.

Checked: 24 of 24 in the call check, on both sides; layout harness at
three widths; web unit tests unchanged.

### 6aw. Voice: why it broke between devices, and what fixed it

Reported: poor audio between the desktop app and a browser, and "both of us
muted and the voice still comes". Four faults were found, each reproduced
before it was fixed and each with a test that fails on the old code.

**1. The media port stayed on an address the machine no longer had.** The
Wi-Fi lease changed twice in one evening (172.24.156.29 -> 172.22.35.50 ->
172.22.189.58). The SFU chose its bind address once at start-up, so every
WebRTC connection after a change sat in `new` with no candidate pair, and
every call fell back to the TCP socket after six seconds. Now
`Registry.WatchAddresses` re-reads the default-route interface every 3 s and
`MediaTransport.Rebind` follows it in place: addresses that remain keep their
sockets (calls on them are untouched), new ones are bound, gone ones closed;
the candidate filter every router holds is refreshed through the same
pointer. Tests: `rebind_test.go` (real sockets on loopback aliases; kept
socket identity, failed bind changes nothing, empty reading keeps the set,
Apply racing Rebind under -race, watcher ends on Close).

**2. The server forwarded each publisher's RTP header extensions.** Its MID,
simulcast RID and audio level, under the publisher's extension ids, went to
every subscriber. Chrome ignores a MID tag while no section of the bundle
maps that id and routes by it once one does -- so the moment anybody turned a
camera on, every subscriber's audio was mis-routed and dropped for the rest
of the call. Measured in Chrome with three people: C turns a camera on, and
A and B stop hearing each other (inbound audio +200 -> +0 packets per 2 s,
while both kept sending). `forwardCopy` strips them. Test:
`TestPublisherExtensionsAreNotForwarded` -- on the old code 50 of 50
packets arrived still tagged. `TestAudioSurvivesAddingTheCamera` covers the
scenario with per-track counts (the old two-track test counted feeds, not
whether the first one kept flowing). The stripped bytes are ones no
receiver could use (rule 2).

**3. The browser dropped the server's ICE candidate.** The server trickles
its candidate before its answer arrives, so the candidate always met a
connection with no remote description and was refused ("The remote
description was null"); the client swallowed the error. On an open LAN the
browser learned the address anyway from the server's checks. Behind a
firewall or NAT that drops those, WebRTC could never connect. `handleICE`
now holds early candidates (at most 32 per connection) and applies them once
the description is set. Test: `icequeue.test.mjs` against the real
`MediaPlane`, with a fake that refuses as Chrome does: 4 of 4 fail on the
old code.

**4. Mute could wait behind the camera.** The mic shared a queue with camera
work, so a camera acquisition that hung held every mute behind it; and a
person who joined with the mic off in the pre-join could never unmute (no
track existed, and unmute returned early). The switch is now applied
synchronously on every audio track, the mic has its own queue, and unmuting
with no live track opens one. The host-mute paths use the same lane.
`mute.e2e.mjs` covers seven scenarios on the other side's loudspeaker, room
and ring flows, both transports; before the fix scenarios 5 and 7 failed.

**Transport default: WebRTC, socket as fallback.** Measured on a speech
recording through `tools/impair` (10 ms delay, 15 ms jitter, 1% UDP loss, a
250 ms stall every 3 s -- Wi-Fi-like), the listener's recording compared
with the clean reference frame by 10 ms frame:

| Path | Lost in clear speech | Longest gap | Envelope correlation |
|---|---|---|---|
| Socket (TCP), 3 runs | 580 / 770 / 700 ms of ~7.5 s | 130-190 ms | 0.80-0.88 |
| WebRTC (UDP), 2 runs | 30 / 30 ms of ~7.4 s | 20 ms | 0.981 |

TCP turns every stall into a hole, because bytes behind a lost segment wait
for the retransmit; RTP loses one packet and conceals it (Opus in-band FEC).
The cost is the jitter buffer WebRTC grew to ride the stalls (~220 ms under
this impairment). Unimpaired on loopback: glass to glass 148 ms on WebRTC
against 239 ms on the socket, both with the noise suppressor on. With UDP
made unreachable the socket fallback takes over after 6 s: continuous,
248 ms.

**Noise suppression.** RNNoise (fullband, 48 kHz) in an AudioWorklet between
the device and the encoder, on by default, with Strong / Standard in the
pre-join and in a new in-call Audio panel (microphone, speaker, noise
cancellation, level). On a speech recording under pink noise at 8 dB SNR, the
listener's gap between speech and pause was 27.8 dB with the browser's own
suppression and 49.3 dB with RNNoise; on clean speech it changes nothing
measurable (30 ms of 5.9 s). Honest costs: about 60 ms of added delay
(179 -> 239 ms on the socket path), and gaps in clear speech under heavy
noise rose from 40 to 90 ms of 6 s. The vendored worklet had two defects,
both fixed: it produced silence (not the input) until its network was
ready, which is forever when the page's policy blocks WebAssembly -- the
LAN server's CSP did, and a call on it had no audio at all -- and its port
was never started. The CSP now allows `'wasm-unsafe-eval'` (compile only; no
`eval`), and the page uses the suppressor only after it reports ready.

Not done: the numbers above are one laptop and a synthetic impairment, not a
real congested Wi-Fi; the desktop app's saved server address still goes
stale when the laptop's address changes (a DHCP reservation or a stable
name fixes that; this machine's name has an underscore, which is not a
valid host name); there is no automated check of a real second device.

**Bit level (tshark, loopback, audio-only WebRTC call, server -> client
audio RTP, old binary vs new):** every forwarded packet carried the
publisher's extensions before (3068 of 3068, 11.8 bytes average) and none
after (0 of 3078). UDP payload per packet 203.9 -> 191.7 bytes. With the
Opus payload at ~170 bytes and Ethernet/IPv4/UDP framing (42 bytes) added,
payload efficiency is 69.0% -> 72.6% at one packet per 20 ms frame.

### 6ax. Voice level, crowds, video codec, one presenter, the call chat

**Voice arrived too quiet.** A voice at -38 dBFS into the chain came out at
-38. `web/src/leveler.js` (an AudioWorklet after RNNoise, in both modes) is
an automatic gain control: target -20 dBFS, +24/-12 dB, acquires after
200 ms of speech, moves 6 dB/s up and 24 dB/s down only while you talk, a
-1 dBFS limiter, no lookahead (no added delay). It follows the nearest
talker only: after acquisition, speech more than 6 dB under your tracked
level is background (re-acquired if it is all there is for 2 s). Without
that gate, in a crowd RNNoise lets bursts of other voices through between
your words and the leveler turned them up (voice-over-crowd 11.3 -> 7.6 dB);
with it, 10.5 dB. Tests: `leveler.test.mjs` (10), each reproducing a fault
before its fix.

**"Voice cutting" was, in the harness, the instrument.** The harness's
recorder was a ScriptProcessor (main thread); under load it dropped input,
which read as 40-210 ms gaps in the call. With an AudioWorklet recorder that
counts every 128-sample block (`recorder: N missed`), at full CPU load on all
24 cores: longest gap 10-30 ms for Strong, Standard and the raw microphone
alike, 0 blocks missed. The voice chain adds no dropouts.

**Scenes, live, 96 calls** (six scenes x Strong/Standard x four engine
pairings, Chrome 153 and Edge 153 = the app's WebView2, two runs each;
voice over what remains in your pauses):

| Scene | Strong | Standard | Longest gap (Strong) |
|---|---|---|---|
| Silent room | 87-91 dB | 90 dB | 30 ms |
| Office (fan, keyboard) | 51 dB | 38 dB | 30 ms |
| Traffic | 56-57 dB | 31-32 dB | 40 ms |
| Traffic, quiet talker | 53-54 dB (voice at -22 dBFS) | 33 dB | 40 ms |
| Crowd (8 talkers, 5 dB below you) | 16.5 dB | 15 dB | 50 ms |
| Packed crowd (16 talkers, as loud as you) | 10.5-11.4 dB | 8 dB | 80 ms |

Every engine pairing is within 0.8 dB of the others: the app and a
browser process a voice identically. Honest limit: a crowd as loud as you
is RNNoise's ceiling -- it separates speech from noise, not one talker from
another. GTCRN (same package, trained on harder noise) was benched and is
worse (turns your voice down 7 dB, 16 kHz only). Firefox and Safari are
not installed here and are untested.

**Video: the receiver got 360p for a 720p tile.** The allocator's utility
floor refused the 360p->720p upgrade in a two-person call (0.34 per Mbps
against a floor of 1.0), so the receiver drew 640x360 stretched over an
820x556 tile with bandwidth to spare. `addsVisiblePixels` exempts upgrades
that put more pixels on a tile that paints them; the floor still stops
frame-rate-only upgrades on thumbnails. `TestVisibleResolutionIsNeverRefusedForWantOfUtility`
fails on the old allocator.

**VP9.** Measured with the browser's own real-time encoders on identical
camera-like footage (luma PSNR, I420 in and out -- an RGB round trip
capped every codec at 31 dB and hid the differences):

| kbit/s | VP8 | VP9 | AV1 |
|---|---|---|---|
| ~250 | 40.0 dB | 42.2 dB | 41.8 dB (228k) |
| ~500 | 41.9 | 42.6 | 42.7 (409k) |
| ~800 | 42.3 | 42.6 | 43.0 (613k) |

VP9 at 250 kbit/s beats VP8 at 800. `preferVideoCodecs` orders VP9, VP8,
then the rest via `setCodecPreferences`; AV1 is last (encoder cost on phones,
older Safari cannot decode). Ladder caps lowered to 150k / 500k / 2M with
`scalabilityMode: 'L1T1'` -- an encoder spends its cap, so the better codec
only saves bandwidth if the cap comes down; 2 Mbit/s is above where VP9
levels off. Live, two people: 1280x720 at the camera's frame rate, 0 freezes
across four stream switches, download 709 -> ~500 kbit/s, upload 1340 -> 1080.

**One presenter at a time.** `room/presenter.go`: the latest share claim
wins (Meet/Teams behaviour), claims serialised by the room lock; `May("share")`
refuses everyone but the presenter, host included, so the replaced screen
stops being forwarded on both transports at once; everyone gets
`OpScreenShare {stopped, by}` (the reserved 0x103). The client asks first
("X is presenting. Present instead?"), the replaced presenter's app stops
its capture and says who took over. Tests: room (200 rounds of eight
simultaneous claims -> exactly one presenter), gateway over real sockets
through the real dispatcher (fails with the rule removed), and
`share.e2e.mjs` (three browsers, both transports): takeover in ~100 ms,
Cancel keeps the presenter, simultaneous takeovers end with one.

**The call chat.** The meeting is its own page, so the conversation the chat
screen had loaded was gone and the panel said "Opening your conversation..."
after join. It is now fetched during the pre-join (the other person is
handed over in this tab's sessionStorage, not the link, so a crafted link
cannot point it anywhere); `call.e2e.mjs` checks the history is drawn the
first time the panel is looked at, and fails with the prefetch removed. The
kept/not-kept dropdown is two tabs in a one-to-one call: "Chat with <name> ·
Saved" and "This call only · Deleted after the call". Groups keep the picker.

**Five people, ten minutes (`tools/e2e/load.e2e.mjs`).** Five real
browsers in one room, sampled every 10 s on every receiver. The first runs
found a bug that every shorter test had missed: some tiles froze for good
and late joiners got no video at all, audio fine, server idle. Keyframe
requests (1) went to the SSRC of whichever simulcast layer arrived first,
whatever layer was asked for -- the peer's closure captured that first
`TrackRemote`; (2) were deduplicated per track, not per layer as the code's
own comment said, so two receivers switching to different layers inside
500 ms lost the second request; (3) were never repeated for a receiver
still waiting. Fixed: `PublishedTrack.LayerSSRC`, a dedup slot per layer,
and `Forwarder.Waiting()` -- a receiver waiting on a keyframe re-asks,
rate-limited to one per layer per 500 ms, until it arrives.
`keyframereq_test.go` (3 tests) fails on the old code. On the fixed server:

| 10 min, 5 people | Receivers complete | Freezes | Audio concealed (worst 10 s) | Down / up per person | Server CPU |
|---|---|---|---|---|---|
| Video only (VP9) | 57/57 samples | 0 | -- | 834 / 1085 kbit/s | 8.1% |
| Audio only | all | -- | 0.00% | 144 / 36 kbit/s | 5.2% |
| Both (VP9) | 57/57 | 0 | 0.00% | 977 / 1121 kbit/s | 14.1% |
| Both (VP8, the baseline) | 56/56 | 0 | 0.00% | 1020 / 1341 kbit/s | 19.1% |

VP9 against VP8 in the five-person call: 16% less upload, 6% less download
(most tiles are 320x180 thumbnails either way), 30% less browser CPU, 26%
less server CPU. Before the fix, the same runs had 21 to 240 freezes and one
"both" run carried no video at all (144 kbit/s down: audio only).

One fixed-server run had 68 freezes, every one inside 25 seconds at which
Windows logged "The system is entering Modern Standby" (Kernel-Power 506 at
15:46:52, exit 507 at 15:47:18): the laptop slept under the test. A laptop
that nobody touches can do that in a real call where people only listen,
so the meeting now holds a screen Wake Lock (`holdWake`), taken again when
the page becomes visible; it works in WebView2.

Not done: the server's bandwidth estimator (`Estimator.Update`) is never
called in production, so every receiver's budget is a fixed 1.5 Mbit/s; the
allocator's choices are therefore driven by viewport and measured layer
rates, not by the path. A real congestion signal (transport-cc feedback)
is the next piece of video work.

**Layout, found by the harness this round.** Profile avatars in the members
list were 40 px touch targets (now 44 on touch everywhere, the avatar the
same size); the "chat history for new people" radios were 13 px (now the
same 44 px custom radios as the audio panel); at tablet width a group's name
in the chat header was squeezed to one word a line and then to "E..." by the
header's buttons -- the title is one ellipsised line at every width and
"Start a meeting" is icon-only below 1024 px. Layout harness: no failures at
390, 820 and 1280.

### 6ay. Teams, read from its own package; spotlight and ending a meeting

**Where the parity list now comes from.** `docs/TEAMS_PARITY.md` was built
from Microsoft's public pages. It is now checked against the shipped Teams
desktop package itself (MSTeams 26276.515.5267.3634, x64), read by
`tools/teams-evidence.mjs` and never executed. The package is a native
WebView2 host (`ms-teams.exe`), a native media stack (`RTM*.dll`,
`skypert.dll`, `SlimCore`), and the whole web client as 7,044 gzipped
chunks. Three layers come out of it: 2,095 named feature modules (what
exists), 64,379 English strings in 102 namespaces (the rules as the screen
states them), and 11,829 compiled default settings in `config-prod` (the
numbers). The numbers are server-overridable defaults (Microsoft's ECS
pushes changes), so they are what ships, not a guarantee of what a tenant
gets.

**Spotlight** (`room/spotlight.go`, `OpSpotlight` 0x11f). Taken as Teams
states it: hosts and co-hosts spotlight; at most seven
(`numberOfAllowedSpotlights = 7`, "No more people can be spotlighted"); the
spotlighted person may "Exit spotlight"; "Stop all spotlights"; "Can't pin a
participant that is spotlighted". Server-held and enforced; the list is sent
whole and only when it changes, so a lost frame is repaired by the next one
(rule 5) and a repeated request costs nothing back (rule 2). Leaving takes
you off. On the stage it ranks after your own pin and after a presentation
(Teams puts shared content on the stage and the spotlighted beside it); of
several spotlighted, whoever is speaking holds the stage and the rest lead
the strip, marked.

**End meeting** (`OpEndMeeting` 0x120). Host only, "You'll end the meeting
for everyone." Each participant is sent the frame, flushed, then closed; the
normal leave path does the rest. Knocks in the lobby are told "ended". The
room's lock, spotlights and permissions are reset, because an empty room
lingers two minutes before the reaper and someone rejoining the link in that
window is starting a new meeting, not walking into a locked one.

**A defect found by the browser harness, not by the unit tests.** Ending
stopped `localStream` and left the microphone on: the published microphone
is the noise suppressor's output, and the device track behind it ends only
when the suppressor stops. `tools/e2e/spotlight.e2e.mjs` counted one live
device track in each browser after the end; fixed, it counts zero.

**Measured** (tshark, Npcap loopback, plaintext server on 127.0.0.1:8444,
three signed-in people on raw sockets, `tools/e2e/measure-meeting.mjs`).
Loopback here was IPv4: 4-byte loopback header, 20-byte IP, 20-byte TCP;
WebSocket header 2 bytes server-to-client, 6 client-to-server (mask). Every
event below rode alone, one frame per packet.

| Event | Direction | Payload | On the wire | Efficiency |
| --- | --- | ---: | ---: | ---: |
| Spotlight on, first use of that id | host to server | 20 B | 70 B | 28.6% |
| Spotlight on (list of one) | server to each person | 4 B | 50 B | 8.0% |
| Same spotlight again | host to server | 4 B | 54 B | 7.4% |
| Same spotlight again | server to anyone | 0 | 0 | -- |
| Stop all spotlights (empty list) | server to each person | 3 B | 49 B | 6.1% |
| End meeting | host to server | 2 B | 52 B | 3.8% |
| End meeting | server to each person | 2 B | 48 B | 4.2% |

Read plainly: these payloads are tiny and the efficiency is poor, because a
host's click coincides with nothing else and so pays the full 46 to 50 bytes
of per-packet headers alone; coalescing cannot help a frame that has no
neighbours. Against the honest baseline, a JSON message on the same socket
(`{"type":"spotlight","ids":["<16-char id>"]}`, 47 B, 93 B on the wire),
the server-to-client spotlight is 46% smaller on the wire and 92% smaller
in payload; the comparison assumes the JSON would also have gone out only on
change. The participant id costs 18 bytes the first time a client names it
and one byte after (dictionary), which is the 20 B to 4 B step above.

Tests: `room/spotlight_test.go`, `signal/spotlight_test.go` (real sockets),
`web/src/spotlight.test.mjs`. Each rule was broken on purpose (the limit off
by one, a member allowed to spotlight or to unspotlight someone else, an
unchanged list resent, leave keeping the spotlight, end keeping the lock, a
waiter told "denied", end closing before flushing, a member allowed to end,
the list returned without a copy) and a test failed every time. Full Go
suite under `-race`: pass. Layout harness at 390, 820 and 1280, including a
new screen scrolled to the host actions: pass, screenshots looked at.

### 6az. The meeting, laid out as Teams; roles, Meeting options, hands and removal

**The layout.** The meeting screen was rebuilt to Teams' structure, read from
its package (View menu code in `unified-bar-container-manager`; labels from
the `calling` strings): one bar across the top with the clock and title,
Chat · People · Raise · React · View · More, Camera · Mic ▾ · Share, and a
red Leave whose arrow (hosts only) holds "End meeting". View offers Gallery
(the default, 9 tiles then "+N more"), Speaker, Large gallery ("View up to
49 videos at once", offered from `minParticipantsInLargeGrid = 10`), Focus on
content (only while something is shared), "Don't show chat bubbles" and Full
screen. Gallery tiles are sized by `web/src/gallery.js bestGrid` to the
largest 16:9 that fits; your own video floats bottom-right; a pin, a
presentation or a spotlight turn the gallery into Speaker as Teams does.
Chat and People open as a right-hand panel (an overlay below 1024 px, a
full-height sheet on a phone) and the meeting opens with it closed. Chat
bubbles show a message for `chatBubblesTimeout = 4000` ms with a
`chatBubbleFadeDuration = 300` ms fade. On a phone the bar splits: the clock
and panel icons stay at the top, Camera, Mic, Share and Leave become a
bottom dock. Aether's own visual system throughout; no Microsoft artwork.
The gallery's cap of 9 is Microsoft's published behaviour; the package
carries no constant for it.

`reportViewport` measures exactly what is drawn (`featuredFor()` is shared
with the renderer), so the allocator is never told a tile is visible that is
not, or the reverse.

**Defects found on the way.**
- The meeting clock never ran: nothing updated it, so it read 00:00 for the
  whole meeting. It now ticks (m:ss, then h:mm:ss).
- A host leaving handed hosting to the next person silently. The server
  promoted them; nobody was told, so the new host's controls stayed grey
  and nobody could admit anyone. `OpSetRole` now announces it, and the
  roles e2e proves it (it fails with the client half of the fix removed).
- Three layout defects visible only in screenshots: older CSS forced
  `display` on elements and beat the `hidden` attribute (a stray back arrow
  and room pill overlapping the phone's top row); the phone's People panel
  was capped at 46dvh by an old drawer rule instead of being a full sheet;
  the tablet's Leave was 36 px tall. And one in the menu: the shared popover
  style anchored `right: 0`, which stretched the person menu across the
  window. The layout harness checks overflow and tap size, not overlap or
  stretching; the screenshots caught all four.

**Roles** (`room/roles.go`, `OpSetRole` 0x121). Organizer (the host, whose
role nobody can change), co-organizer, presenter, attendee. The attendee
controls are Teams' "for attendees": presenters and co-organizers are exempt
(`May`, role ≥ presenter). Hosts get "Make a presenter" / "Make an
attendee" on a person's "..."; the person is told "Your role has been
changed" / "Your role has been changed to attendee". The organizer is marked
by one spare bit in the roster and role frames, so the client never offers
what the server would refuse.

**Remove from meeting** (`OpRemove` 0x122). Host only; never the organizer or
yourself. The removed person sees "You've been removed from this meeting" /
"Someone just removed you from the meeting. You can try to rejoin.", and
their devices are stopped. They can rejoin, as in Teams; a lock or the lobby
is how a host keeps them out.

**Hands.** "Raised hands (N)" lists them in the order they went up, numbered
to `raiseHandsOrderedRankLimit = 20`. The order is each client's arrival
order of the state deltas, which every client sees in the server's order;
someone who joins after hands are up sees them in roster order instead
(ponytail: server-stamped raise times if that matters). "Lower all hands"
(`OpLowerHands` 0x123): each raised hand's owner is told "Your hand was
lowered"; everybody gets the lowered state from the server at once, so a
client that ignored the request still shows the hand down everywhere else.

**Meeting options** (More, hosts only), Teams' questions: "Who can bypass the
lobby?" (Everyone / People in my org / Only organizers and co-organizers =
open / trusted / knock; invitees always come in), "Who can present?",
"Allow mic for attendees?", "Allow camera for attendees?", "Meeting chat",
and "Allow reactions" -- a new control, its own bit in `OpPermissions`, sent
inverted so the zero an older client sends means "allowed"; reactions were
previously gated on chat. "Lock the meeting" and "Manage permissions" sit in
the People panel's "..." beside "Mute all", where Teams puts them.

**Measured** (tshark, loopback IPv4, `tools/e2e/measure-meeting.mjs`, three
people; ids already in the dictionary unless stated):

| Event | Direction | Payload | On the wire | Efficiency |
| --- | --- | ---: | ---: | ---: |
| Make a presenter | host to server | 4 B | 54 B | 7.4% |
| Role changed | server to each person | 4 B | 50 B | 8.0% |
| Hand raised (state delta) | server to each other person | 3 B | 49 B | 6.1% |
| Lower all hands | host to server | 2 B | 52 B | 3.8% |
| Lower request + lowered state, coalesced | server to the hand's owner | 8 B | 54 B | 14.8% |
| Lowered state | server to everyone else | 3 B | 49 B | 6.1% |
| Remove, first time the host names that id | host to server | 19 B | 69 B | 27.5% |
| Removed | server to the person removed | 2 B | 48 B | 4.2% |

Same reading as §6ay: rare clicks that ride alone pay 46-50 bytes of headers
each. The one place two frames coincide -- the hand's owner gets the request
and the new state together -- the coalescing writer puts them in one packet
and the efficiency doubles. Against a JSON message on the same socket
(`{"type":"role","id":"<16-char id>","role":"presenter"}`, 54 B, 100 B on
the wire) the role frame is half the wire size.

Tests: `room/roles_test.go`, `signal/roles_test.go` (real sockets; 10 rules
broken on purpose, each caught), `tools/e2e/roles.e2e.mjs` (three browsers,
33 checks), `tools/e2e/spotlight.e2e.mjs` rewritten on the shared
`tools/e2e/meet.lib.mjs` and the new menus; layout harness with new screens
for the person menu, Meeting options, View and Leave menus at 390, 820 and
1280 -- no failures, screenshots looked at.

### 6ba. Download attendance list

Teams gives organizers and co-organizers "Download attendance list" from the
People panel. Its columns, from the package's strings: a Summary (Meeting
title, Attended participants, Start time, End time, Meeting duration,
Average attendance time) and Participants (Name, First join, Last leave,
In-meeting duration). Aether builds exactly that from the visits it already
records (`scheduling_attendance`, one row per account per visit), as
`GET /api/scheduling/rooms/{room}/attendance` (CSV, attachment, nosniff,
no-store), behind "Download attendance list" in the People panel's "...".

**What is left out, and why.** Teams adds each person's email (UPN). Aether
does not: its directory never hands out a list of accounts' addresses
(§6l), and an attendance file is exactly such a list. Guests leave no
attendance row (no account to attribute it to), so they are not in it.

**Who, and which meeting.** The meeting transport decides, not the caller:
`room.HostSession(account)` is true only for the organizer or a co-organizer
present now, and returns when the meeting began -- the moment the room last
went from empty to occupied, since a room lingers after emptying and its
creation time is not the meeting. So the list covers this meeting and not
the room's earlier ones.

**Accuracy.** One row per person however often they rejoined: first join,
last leave ("In the meeting" while present), and the sum of their visits. A
display name that begins `= + - @` is prefixed with an apostrophe so a
spreadsheet shows it rather than running it (CSV injection).

**Speed** (rule 7). Lists were only ever read by account; reading by room
needed a new index, `scheduling_attendance_room (room, joined_at)`. Measured
in `attendancelist_test.go` over 200,000 visits in 2,000 rooms: the plan is
`SEARCH ... USING INDEX scheduling_attendance_room (room=? AND joined_at>?)`
with no sort afterwards, worst of 50 lookups 5.8 ms under the race detector.
Bounded: 5,000 rows per export, room id ≤ 128 characters (tested one over).

**Security** (rule 7), in `tools/e2e/roles.e2e.mjs` because it needs a live
meeting with a host in it: the host gets the file; an attendee, a removed
person (now a stranger), an anonymous caller and an over-long room id are
refused; a foreign origin is given no cross-origin read. A GET with no side
effects has nothing for CSRF to forge.

**Measured** (tshark, loopback IPv4, plaintext server; three people): the
request is one 384 B packet (most of it the cookie); the response one 1,075 B
packet carrying a 377 B CSV -- 35.1% payload efficiency, the rest HTTP and
security headers every response carries. A download a host makes once per
meeting; not worth trimming the headers it shares with every other response.

Tests: `attendancelist_test.go` (aggregation, formula defusing, access and
session scope, scale), `room/roles_test.go TestHostSession`; 7 mutations,
each caught (co-organizer test widened to presenters, session start never
reset, formula guard off, the session bound dropped from the query, the host
check skipped, the latest name ignored, durations overwritten not summed).

### 6bb. "5 minutes left in your meeting."

Teams' package has two end-of-meeting messages and they are different
things. "This meeting has a time limit of 60 minutes" / "This meeting will
end in 15 minutes" is the free tier's hard cap, which ends the call; Aether
has no cap and a meeting ends when its room empties (§6j), so that one is
deliberately not built. "5 minutes left in your meeting." is a heads-up
before a scheduled meeting's end time that ends nothing; that one is built
(`web/src/meeting.js endNoticeDelay`): shown five minutes before the
scheduled end, at once if the meeting is joined later than that, never with
under a minute to go, never for an ad-hoc meeting.

Wire cost: none of its own. When a meeting is joined straight from a link
the calendar has not been fetched yet, and it is fetched once
(`GET /api/scheduling/meetings`, the request the home screen already makes);
not captured separately here.

Tests: `meeting.test.mjs` (when, and when not), `tools/e2e/endnotice.e2e.mjs`
(a meeting scheduled to end in four minutes shows the notice and carries
on; an ad-hoc meeting says nothing), which fails with the hook disabled.

### 6bc. Breakout rooms

Built as Teams runs them, from its package: "Create rooms" (up to
`maxBreakoutRoomCount = 50`; a suggested count from `breakoutRoomsSizeConfig`,
four people a room), "Automatically" / "Manually", "Assign participants",
"Shuffle", "Rename" ("Room names must be 35 characters or fewer"), "Rooms
settings" ("Set a time limit", "Automatically move people to rooms", "Let
people return to the main meeting"), "Open Rooms" / "Close Rooms", "Make an
announcement", "Delete rooms", "Join room" for the people running them, and
"You can't lock the meeting while breakout rooms are open." A "Rooms" button
in the bar, for hosts, opens the panel.

**A breakout room is an ordinary room.** Media, chat, captions, reactions and
attendance work in it unchanged, because it is one: `<meeting>~br~<nonce>~<n>`,
with a fresh random nonce for every set of rooms. The main meeting's `Room`
holds the breakout state (`room/breakout.go`); the gateway moves people and
decides entry (`signal/breakout.go`).

**Moving is leaving and joining.** The server sends each assigned person a
MOVE ("We'll move you to your assigned room in 10 seconds.", or "Join
breakout room?" when automatic moves are off). The client goes to the room
the way a page load does -- the honest teardown of its peer connections --
carrying its microphone and camera choices, and presses Join itself. Coming
back is the same move in reverse ("We'll move you back to the main meeting in
N seconds"): 10 s after a host's "Close Rooms", 30 s after the timer
(`breakoutRoomTimerExpireScreenAutoJoinDuration = 30000`). In a room a banner
shows its name, "Remaining time" and "Return", and warns "This room will
close in 60 seconds". The 10-second and 60-second figures are Microsoft's
published behaviour; the package fills those strings at run time.

**Security: who may enter a room.** Nobody by its name. Entry is a ticket
the server minted for that room and that account (single-use, two minutes),
or being a manager of the open rooms (the organizer and co-organizers when
they were opened -- kept, because a manager who walks into a room is no
longer in the main meeting to be asked). The lobby, an invitation and the
admission policy do not open a breakout room. A reusable return ticket lets
people back into the main meeting past its lobby, because they were let in
once already. The ticket store is bounded (100,000; a full store refuses to
move anyone rather than grow) and tickets travel as blobs, not interned
strings, so they never fill the per-connection dictionary.

**Lifecycle.** While rooms are open the main meeting is not over when it
empties (no "meeting ended", not reaped), cannot be locked, and "End meeting"
ends every room too. Rooms are defined only while closed (live reassignment
is "Close Rooms", reassign, reopen; Teams also moves people between open
rooms -- not built). Breakout timers stop on shutdown.

**Measured** (tshark, loopback IPv4, plaintext server; host, two attendees,
one of whom enters Room 1 on a second socket):

| Frame | Payload | On the wire | Efficiency |
| --- | ---: | ---: | ---: |
| Set two rooms, host to server | 39 B | 89 B | 43.8% |
| Room list to the host, first time (ids, names spelled out) | 90 B | 136 B | 66.2% |
| Room list after a change (dictionary references) | 18 B | 64 B | 28.1% |
| Open / Close, host to server | 3 B | 53 B | 5.7% |
| Move, to each person (room, name, two 24-char tickets, parent) | 93 B | 139 B | 66.9% |
| Entering the room: welcome, roster, room info, coalesced | 126 B | 186 B | 67.7% |
| Announcement, host to server | 20 B | 70 B | 28.6% |
| Announcement, into the room | 22 B | 68 B | 32.4% |
| Return | 30 B | 76 B | 39.5% |

The tickets are most of a move (48 of 93 bytes) and are worth it: they are
the whole access control. The room list is sent whole on every change (rule
5) and costs 18 bytes once the ids are known. Against JSON on the same
socket, a move (`{"type":"move","room":"…","name":"Room 1","ticket":"…",
"back":"…","parent":"…","closes":0,"prompt":false,"allowReturn":true}`,
about 170 B, 216 B on the wire) is 36% smaller on the wire.

Tests: `room/breakout_test.go` (limits one over, assignment rules, lifecycle,
copies, reaper, End), `signal/breakout_test.go` (real sockets: host-only set
and open, moves with tickets, tickets single-use / room-bound /
account-bound / bounded, manager entry only while open, close by host 10 s
and by timer 30 s, announcements host-only and bounded, no lock while open,
End ends the rooms, 51 rooms refused) -- 16 mutations, each caught;
`web/src/breakout.test.mjs`; `tools/e2e/breakout.e2e.mjs` (four browsers, 28
checks: create, open, both moved, the host's live counts, a stranger with
the link refused, announcement, lock refused, Return, Close, Delete), which
fails when the client stops presenting its ticket. Layout harness: Rooms
create / list / settings at 390, 820 and 1280. The screenshots caught one
defect: an id selector made the People panel show inside every other panel.

### 6bd. "Disable mic" / "Disable camera" for one attendee; presenters no longer muted by the client

Teams' person menu offers, on an attendee, "Disable mic" / "Allow mic" and
"Disable camera" / "Allow camera"; the person is told "Your mic has been
disabled", "Your mic has been enabled. Unmute if you'd like to speak.",
"Your camera has been disabled", "Your camera has been enabled". Built as
the meeting-wide controls are: enforced in `room.May`, so the router stops
forwarding at once (`ApplyPermissions`) and the socket path drops the frames,
whatever the person's client does (`OpBlock` 0x124). Attendees only -- not
the organizer, not presenters, as Teams offers it -- and "Make a presenter"
lifts it. Remembered by account, so leaving and rejoining does not escape it
(guests by connection); gone when the meeting ends. An arrival is told who is
disabled; the People list says "Mic disabled".

**A defect found while building it.** The client applied the attendee
controls to presenters: with "Allow mic for attendees?" off, a presenter who
had unmuted was muted again by their own client the next time the controls
were sent, though the server would have carried their voice. The exemption
now matches the server's (host or presenter); `roles.e2e.mjs` fails with the
old line restored. And a demoted presenter now reads "Your role has been
changed to attendee" rather than having it replaced at once by the
meeting-wide "microphones turned off" message.

**Measured** (tshark, loopback IPv4): host to server 4 B (54 B on the wire),
server to each person 4 B (50 B, 8.0%) -- the shape of a role change (§6az).

Tests: `room/roles_test.go TestBlockedAttendee`, `signal/roles_test.go
TestBlockThroughGateway` (a disabled mic's audio does not reach the room;
another attendee's does); 7 mutations, each caught after one exposed a gap:
the test said a promotion's lifting survived a rejoin and never rejoined.
`roles.e2e.mjs` (Disable mic, the person's notices and greyed Mic, "Mic
disabled" in the list, Allow mic, attendees not offered it).

### 6be. Co-organizers chosen when scheduling; what the older harnesses caught

**"Choose co-organizers."** A meeting's invitees can be marked co-organizers
in the schedule form (column `scheduling_invitees.co_organizer`, migration
0048). On arrival the gateway asks the scheduling module (`CoOrganizerOf`)
and gives them the role before they are announced, so everyone sees
"Co-organizer" and they have the host's controls. Teams' rules: chosen from
the people invited (a name not invited is refused, 400), at most 10
(Microsoft's published limit; the package fills it at run time), never the
organizer, and none once the meeting is cancelled. Teams also requires them
to be in the organizer's organization; Aether does not check that yet.
Wire cost: no new frame -- the role rides in the arrival's roster entry;
the request carries the chosen addresses in the existing scheduling POST.

Tests: `coorganizer_test.go` (refusals, ten and eleven, storage, the
organizer never flagged, a duplicate keeps the choice, an edit keeps it,
cancelled stops it); 6 mutations caught after one exposed a missing
duplicate case; `tools/e2e/coorg.e2e.mjs`, which fails against a server
with the arrival role removed.

**The older harnesses, rerun after the meeting screen was rebuilt (§6az).**
All pass now (messaging 432 assertions, call, controls, share, mute, audio,
video, live, live status). On the way they found three regressions of the
rebuild, all on a phone: the audio panel opened above the top of the screen
and the reaction picker just below the bottom, because the bar's new
"drop below the button" rules overrode their phone bottom-sheet rules (now
limited to wider screens); the reaction sheet then sat under the dock,
inside the top row's stacking context (the row is lifted, the sheet seated
above the dock); and the speaker-volume slider moved into the audio panel
was under 44 px on touch. The call harness itself assumed clicking Chat
opens it; it is a toggle now, as in Teams, and the harness opens it only
when closed.

**Screens the layout harness had never opened.** The schedule form was not
in it; added, its screenshots showed "Ends" running off a phone's edge (a
`1fr 1fr` grid will not shrink below a date-time input -- the columns are
`minmax(0, 1fr)` and stack under 480 px), an empty suggestions bar drawn
under "Invite people" (the `hidden` attribute overridden again), and, on a
desktop, Save and Cancel cut off once the form outgrew the window. Every
dialog now keeps its actions in view while its fields scroll.

### 6bf. Q&A

Teams hosts meeting Q&A in a separate service loaded at run time, so the
package carries only some of its words: "Close Q&A", "Anonymous", "Like",
"Reply", "Pin", "Answered" / "Unanswered", "Dismissed", "Publish",
"Organizers can customize Q&A to allow anonymous posts, review questions
before publishing". Built from those and Microsoft's published behaviour:
a "Q&A" button in the bar opens the panel; anyone in the meeting asks
(optionally "Post anonymously"), likes and replies; the organizer and
co-organizers have "Q&A settings" (review questions before they are
published, allow anonymous posts, Close Q&A), Published / Pending /
Dismissed tabs, and on each question Publish, Dismiss, Pin, Mark as
answered. Recent / Top sorting; pinned first.

**Stored, by room** (`scheduling/qna.go`; tables 0049-0054), so a reload or
a rejoin finds the questions again. Reads are three queries however many
questions there are (questions with like counts, replies, settings), each
on its own index, asserted by `EXPLAIN QUERY PLAN` in the tests (rule 7).
**Who:** the meeting transport answers (`Registry.Member`): only people in
the meeting now, by account, read or post, under the name they use there;
only its organizer and co-organizers moderate. **Anonymous** means no author
for anyone, the organizer included; the author still knows it is theirs.
**Pending** and **dismissed** questions are seen by the hosts and their
author only -- the author is told what became of it rather than watching it
vanish. **Bounds**, each tested one over: 1,000 characters a question or
reply, 1,000 questions a meeting, 200 replies a question, 64 KB a request.

**Live.** Each change is pushed over the meeting socket (`OpQnA` 0x125) as
the whole question, so the panel draws it without asking (the push-tail
rule); a question not yet published goes only to the hosts and its author.
What only one viewer knows -- which are theirs, which they liked -- is not in
a push: the client learns it from its list read and its own actions, so one
serialisation serves the whole room.

**Measured** (tshark, loopback IPv4, plaintext server; a 30-character
question, then a like):

| Event | Payload | On the wire | Efficiency |
| --- | ---: | ---: | ---: |
| Ask, HTTP request (body 58 B) | 504 B | 548 B | 10.6% of it is the body |
| Ask, HTTP response (the question, 227 B) | 809 B | 853 B | |
| Push of the question, to each person | 227 B | 275 B | 82.5% |
| Like, HTTP request / response | 469 / 591 B | 513 / 635 B | |
| Push after a like (the whole question again) | 227 B | 275 B | 82.5% |

Read plainly: the push is JSON, and a like re-sends the whole question
where a count would be a few bytes. Chosen, not overlooked: every push is
self-contained (rule 5 -- a lost frame is repaired by the next), and Q&A is
human-paced, a few events a minute in a busy meeting. A binary question
frame would be about a quarter of the size; worth it if Q&A ever runs at
event scale. The HTTP requests are mostly the cookie and headers.

Tests: `qna_test.go` (strangers and other rooms refused; anonymity for
every viewer; moderation and visibility; closed; likes toggle; replies;
moderation actions; pushes restricted correctly; bounds; index plans) -- 11
mutations, each caught; `tools/e2e/qna.e2e.mjs` (four browsers, 21 checks,
including the rule 7 cases: not in the meeting, anonymous, no CSRF header,
an attendee moderating, one over the length bound), which fails when the
client ignores the pushes. Layout harness: the Q&A panel at 390, 820, 1280.

### 6bg. Call health

More > "Call health" opens Teams' panel with its sections and row names, from
the package: Network ("Round trip time", "Sent bitrate", "Received bitrate",
"Received packet loss"), Audio, Video and Screen Sharing ("Sent resolution",
"Sent frame rate", "Received resolution", "Received frame rate"), and the
Microphone, Speaker and Camera in use. Refreshed each second while it is the
panel showing, and not sampled at all otherwise.

On the WebRTC path the figures are `getStats` (`rtc.js healthSample`): audio,
camera and screen told apart by track id, the largest stream of a kind
standing for it. Rates and loss are over the last interval, never since the
call began (`web/src/health.js`): a call that lost packets ten minutes ago
is not lossy now. A figure that cannot be measured is a dash, never a zero
-- including a counter that went backwards after a renegotiation. On the
WebSocket path the browser keeps no RTP statistics; the panel says so and
shows the bytes this client counted each way. The microphone is named as
the device, not as the noise suppressor's output node (the first run showed
"MediaStreamAudioDestinationNode").

Wire cost: none. It reads what the browser and the client already count.

Tests: `web/src/health.test.mjs` (interval rates, loss over the interval,
resolution and frame rate, dashes, units); `tools/e2e/health.e2e.mjs` (two
browsers on WebRTC: every row measured, Teams' units, refreshing, closing
stops it; on the WebSocket path the note and the byte counts), which fails
ten ways when the panel is fed nothing; layout harness at three widths.

### 6bh. Meeting notes

Teams' meeting notes are a shared Loop page with a fixed template; its
package gives the words: "Meeting notes", "Agenda", "Notes", "Follow-up
tasks", "Add meeting notes", "The organizer hasn't created meeting notes
yet". Built as that template: a "Notes" button in the bar; the organizer
(or a co-organizer) adds the notes for everyone; then anyone in the meeting
adds, edits in place, deletes, and -- for tasks -- ticks off and assigns
items. Stored by room (`scheduling/notes.go`, tables 0055-0057), so they
outlast a reload and the meeting itself.

**Items, not one shared text, and why.** Free text edited by several people
at once needs a merge (a CRDT or OT) to avoid losing someone's words; that
is a subsystem of its own. The template is lists, so the notes are items:
two people editing different items never collide, and two editing the same
item are caught by its version. The second save is refused (409) with the
item as it now is, and the panel shows the other person's text -- nothing
is silently overwritten, which is rule 7's accuracy, held at the server.
A stale delete is refused the same way. (Character-level co-editing of one
item: not built.)

**Live.** Each change is pushed whole over the meeting socket (`OpNotes`
0x126, through the same helper as Q&A). A push that arrives while someone is
typing in an item does not throw their typing away: the panel redraws once
they finish.

**Bounds**, tested one over: 2,000 characters an item, 100 an assignee, 500
items a meeting, 64 KB a request. Reads by room on
`meeting_note_items_room (room, created_at, id)`, no sort after (asserted).

**Measured** (tshark, loopback IPv4, plaintext server):

| Event | Payload | On the wire | Efficiency |
| --- | ---: | ---: | ---: |
| "Add meeting notes", push to each person | 19 B | 65 B | 29.2% |
| A 13-character agenda item, push to each person | 162 B | 210 B | 77.1% |
| Adding it, HTTP request (body 43 B) / response (body 150 B) | | 541 / 776 B | |

The item push is the whole item as JSON (id, section, text, state,
version, time); a binary frame would be about a third of it. Notes are
typed by people; not worth it.

Tests: `notes_test.go` (create host-only, nothing before creation, sections,
add/edit/tick/assign/delete, the concurrent edit refused with the first
editor's text, stale delete, outsiders, bounds, index plan) -- 8 mutations,
each caught; `tools/e2e/notes.e2e.mjs` (four browsers, 15 checks, including
the conflict and the rule 7 cases), which fails when the client ignores the
pushes; layout harness screens for notes before and after they are added.

**A defect the full suite found here.** `TestHostHandoverOnLeave` failed
once on a full run after passing for days. When a host leaves, hosting goes
to whoever joined earliest, compared by `JoinedAt`; on Windows the clock is
coarse enough that two quick joins read the same instant, and the tie was
broken by map iteration order -- the next host was chosen at random. Rule 7
asks for a total tiebreak on every ordering: arrivals now carry a join
sequence number, used for the handover and for roster order. With the old
comparison restored the test failed 7 times in 50 runs; with the sequence,
50 of 50 pass.

### 6bi. Whiteboard

Teams' "Share" menu offers Microsoft Whiteboard on the main stage ("Start
Whiteboard", "Collaborate on a whiteboard", "Turn off whiteboard"). Teams
embeds a separate Microsoft product; Aether has its own board, with the
same entry point and the same place on the stage: Share > Whiteboard,
drawn where the large video was, people on the strip beside it.

**What it does.** Pen and highlighter in seven ink colours and three widths,
an eraser (tap a stroke or a note), sticky notes in four colours, Undo of
your own last stroke. Whoever started it, or a host, has Clear and "Turn off
whiteboard"; turning it off takes it away for everyone. Starting it needs the
same permission as sharing a screen. A late arrival is sent the board as it
is. The board is room state (`room/whiteboard.go`) and lives as long as the
meeting room; keeping it after the meeting is not built.

**Coordinates.** A 16:9 board of 4096 x 2304 units, whatever the screen;
each client scales to its own canvas, so a line drawn on a phone lands in the
same place on a 4K monitor.

**The encoding** (`OpWhiteboard` 0x127, `signal/whiteboard.go`,
`web/src/whiteboard.js`, pinned to the same bytes by a golden test both
ways): one style byte (tool 1 bit, palette colour 4 bits, width-1 3 bits),
a point count, the first point absolute and the rest as zigzag deltas. A pen
moving a few units between samples costs two bytes a point.

**Live preview.** While someone draws, what is new goes out every 50 ms
(coalesced on the client, as the bar's other controls are on the server),
relayed to everyone else and never stored; each batch repeats the last point
already sent, so the preview has no gaps. When the pen lifts, the whole
stroke goes out once more as the stored, self-contained record. That second
copy is deliberate, and said so at the definition: a stroke assembled from
earlier live frames would be cross-frame state, and a person who joined
mid-stroke would hold a broken copy with no cheap repair (rule 5).

**Bounds**, tested one over: 5,000 strokes, 4,096 points a stroke (a longer
stroke is sent as several), 500,000 points on a board, 200 notes, 500
characters a note, 16 palette entries, width 1-8, every point inside the
board. A frame past any of them is refused whole.

**Measured** (tshark, loopback IPv4, plaintext server, two people in the
room; `tools/e2e/measure-meeting.mjs`). B draws one 200-point stroke over two
seconds, as a 100 Hz pointer would:

| Event | Payload | On the wire | Efficiency |
| --- | ---: | ---: | ---: |
| Start the whiteboard: request / push to each | 3 / 6 B | 53 / 52 B | |
| Live preview, 40 frames in 2 s, both directions, with ACKs | 1,595 B | 8,947 B (160 packets) | 17.8% |
| The stroke, 200 points: from B / back to B / to A | 409 / 411 / 436 B | 461 / 459 / 484 B | 89.5% |
| A 16-character sticky note: request / push | 26 / 27 B | 76 / 73 B | |
| Erase / clear / turn off: request / push | 4, 3, 3 / 4, 3, 3 B | ~53 / ~50 B | |

- 409 B for 200 points is 2.05 B a point. The same stroke as JSON
  (`{"type":"stroke",...,"points":[[x,y],...]}`) is 2,421 B, 5.9 times more.
- The push to A was 436 B, not 411: the server's 15 ms coalescer put B's
  last live frame and the stroke in one message.
- The live preview is the expensive part, and per-packet cost is why: 40
  small frames each way, and 82 of the 160 packets are bare TCP ACKs
  (3,608 B, 40% of the bytes). Points ride twice, 798 B live then 409 B
  stored. Fewer, larger live frames would cost less and look worse; the
  50 ms cadence is kept for the preview (rule 3), and idle costs nothing:
  no frame is sent unless the pen moved.
- Control frames (start, erase, clear) are 3-6 B of payload under 44 B of
  headers each, so 3-5% efficient. They are one-off and cannot be
  coalesced with anything.

Tests: `room/whiteboard_test.go` and `signal/whiteboard_test.go` (who may
open, clear and close; bounds one over; codec golden; a read past a bound
refused; through the gateway: the preview reaches others but not the drawer
and is never stored, nothing is sent on a closed board) -- 12 mutations,
each caught;
`web/src/whiteboard.test.mjs` (golden bytes, bounds, screen to board);
`tools/e2e/whiteboard.e2e.mjs` (three browsers, 14 checks, reading the
canvas pixels: the line appears for A while B is still drawing, notes, the
eraser, Undo, the late arrival, who has Clear and Turn off), whose live
preview check fails when the relay is removed; layout harness screens for
the board open and closed at every width.

**The meeting bar at tablet width.** The layout harness now also fails on
any two meeting-bar controls that overlap (it previously checked only
overflow and tap size). It found the clock under Chat on a phone and under
Chat and People at 820 px. Below 1024 px, Q&A, Notes and Rooms now move
under More, with a dot on More when one of them has something new.

**A defect the full suite found here.** Under load, an audio fan-out test
reported frames arriving out of order. The cause was in `Conn.flush`: it
took the queue and encoded the batch under `writeMu`, then released it
before writing. Two flushes at once (the coalescing timer and an
`EmitNow`, or two timers behind a slow write) could reach the socket in the
opposite order. Frames were reordered, and a later batch could refer to a
dictionary string first defined in an earlier batch the peer had not yet
been sent. `writeMu` is now held through the write.
`TestFlushesReachTheWireInQueueOrder` (eight writers, a third of them
flushing at once) failed on every run with the old code (5 of 5) and passes
10 of 10 with the fix. A second test, which inspects the queue directly,
was flaky under load for an unrelated reason: the coalescer drained the
queue under it. It now holds the timer.

### 6bj. Recording, transcription and live captions

Teams' package gives the words and the shape: More > "Record and
transcribe" > "Start recording" / "Start transcription"; "Only meeting
organizers and presenters can control transcription"; "This meeting is
being recorded."; "{{initiatorName}} owns the recording. It'll be saved to
their computer"; "Recording not started. Your IT admin has disabled
Recording."; "Show transcript", "Download transcript". Its config gives the
caption timings: `defaultCaptionMessageDisplayTimeMs = 15e3`,
`clearCaptionsTimeoutMs = 5e3`, `closedCaptionsWidthPercentage = 80`.

**The recording is made on the recorder's computer**, Teams' local-recording
model, not on the server. The organizer's or presenter's browser draws the
stage as their screen lays it out (tiles, video, the whiteboard, names) into
a 1920x1080 canvas, mixes everyone's audio, and the browser's own encoder
writes WebM (VP9 where it can). Where the browser can stream to a file
(`showSaveFilePicker`), the person picks the file at "Start recording" and an
hour never has to fit in memory. Otherwise it is held in memory up to 1 GiB
(`ChunkSink`), then stopped and saved, rather than taking the tab down.
Leaving the meeting, or the meeting ending, saves it first; the recorder
leaving stops it for everyone, since nobody else is making it. The server
holds only who is recording and since when, and tells everyone (the banner
and a red dot by the clock). Server-side recording, kept by the deployment
and linked in the meeting chat, is not built.

**The deployment decides.** "Recording allowed" (settings, off by default,
a decision made before this feature) is enforced at the server: a start is
refused, and the person who asked, and only they, is told why, in Teams'
words. Every start and stop of recording or transcription goes in the
hash-chained audit log (`meeting.recording.*`, `meeting.transcription.*`).

**Captions and the transcript come from the speaker's own browser.** §6q's
captions sent your words only if you had captions on yourself, so you saw
someone's words only if they had chosen to see captions. Teams' "Turn on
live captions" shows everyone's speech. So turning captions on now tells
the room, and while anyone wants captions, or the meeting is transcribed,
each speaker's browser turns their own speech into text. Each line arrives
attributed, with no guessing who spoke. `OpCaption` (§6q) is retired; its
rule that a speaker a host has muted publishes no words is kept, extended to
self-mute, and enforced at the server.

**Consent, and a correction to §6q.** §6q said the audio "never goes
anywhere it was not already going". That is true of this server, but not of
the browser: where Chrome cannot recognise on the device, its recogniser
sends the audio to Google's speech service. With captions now started by
someone else, that would mean a third party hearing you because another
person pressed a button. So:

- On-device recognition (`processLocally`) is used whenever the browser
  offers it.
- Each speaker is asked before anything is recognised: "Turn your speech
  into text, using your browser's online speech service?" (or "on this
  device"), with Allow and Not now. The choice can be changed in the
  transcript panel.
- Someone who says Not now is not transcribed. The e2e harness fails if the
  gate is removed: C's words reach the server.

**On the wire** (`OpRecording` 0x102, `signal/recording.go`,
`web/src/recording.js`):

- A caption frame is the whole current sentence (never a delta, as in §6q)
  plus the speaker's utterance number, so a partial is replaced in place.
- Text is a blob, not an interned string. One-off text in the dictionary
  is what §6q's frame did, and it filled the table with sentences.
- Partials go only to people with captions on. Final lines go to everyone
  while the meeting is transcribed (the transcript panel is live). The
  speaker is never sent their own line back.
- 500 bytes a line, valid UTF-8, and a per-person allowance of 6 frames a
  second (burst 12). Each is tested one over.

**The transcript** is kept per meeting session: the room's epoch, new each
time a room fills from empty (`scheduling/transcript.go`, table 0058). Each
line is numbered inside its INSERT, so two speakers at once never share a
number and the 20,000-line bound holds atomically (tested with eight writers,
then one over). Readable by anyone in the meeting, and afterwards by anyone
who attended it (index 0059 on attendance (room, user)); a stranger gets 403
and an anonymous caller 401. Downloads are `.vtt` (Teams' format: cues timed
from the session start, the speaker as a voice tag, text escaped so a line
cannot forge a cue) and `.txt`, always as attachments. Teams' `.docx`
download is not built.

**Measured** (tshark, loopback IPv4, plaintext server, two people). A says a
47-character sentence over three seconds; the client sends the whole current
text at most every 350 ms, then the final line:

| Event | Payload | On the wire | Efficiency |
| --- | ---: | ---: | ---: |
| Start transcription / captions on: request / push to each | 3 / 10 B | 53 / 56 B | |
| "Start recording" refused: request / reply to the asker only | 3 / 3 B | 53 / 49 B | |
| The sentence: 8 partials and the final, both directions, with ACKs | 569 B | 2,225 B (36 packets) | 25.6% |
| The final line alone: from A / to B | 52 / 53 B | 102 / 99 B | |

- What the server sent B for the sentence is 289 B, against 945 B as JSON
  with the speaker's id (3.3 times more).
- Per-packet cost dominates: 18 data packets and 18 bare ACKs, 44 B of
  headers each. Partials 350 ms apart cannot be coalesced with anything; the
  floor is §6q's, kept because a caption that lags speech by more than that
  reads as broken.
- Idle costs nothing: no frame unless someone speaks, and none at all while
  nobody wants captions and nothing is transcribed.
- The recording costs the network nothing: it is made from media this
  machine already receives. A 30-second test recording of the fake camera
  was 656 KB of WebM.

Tests:

- `room/recording_test.go`: who may record and transcribe, leaving, captions
  wanted, the allowance and its cap.
- `signal/recording_test.go`: through the dispatcher, the refusals and who
  hears of them, the deployment ceiling, who is sent partials and finals,
  self-mute and host-mute, the bounds, flooding, the audit.
- `scheduling/transcript_test.go`: order, sessions, who reads, eight
  concurrent writers, the bound one over, index plans, the VTT escaping.
- 27 mutations, 26 caught. The one not caught is equivalent: removing the
  empty-actor guard on reading leaves both the live check and the attended
  check refusing an anonymous caller.
- `web/src/recording.test.mjs`: the bytes, the refusal, UTF-8 cutting, the
  memory bound.
- `tools/e2e/recording.e2e.mjs`: four browsers and an administrator, 45
  checks, including the refusal, the consent gate (proven able to fail),
  mute, the late arrival, stranger and anonymous reads, the saved file, and
  the recorder leaving.
- Layout harness screens: the More menu, the banner with the ask, and the
  transcript panel before and after it stops.

Headless Chrome has no recogniser, so the e2e replaces the browser's speech
API with one the test speaks into. Everything from the recogniser's result
onward is real.

**A test defect found on the way.** `TestAllMediaRidesTheNamedPort` failed
once on a full run: "bind: An attempt was made to access a socket in a way
forbidden". The helper found a port free for TCP on loopback, and the media
listener then bound UDP on every interface. Windows reserves UDP ranges that
TCP can still use. The helper now returns only a port both can bind.

**A client defect the older harnesses found.** Re-running `call.e2e.mjs`
against this build failed 2 runs in 3, with the old server and the old
client alike, so it was older than this batch. In a one-to-one call, B's
panel showed the "Chat with A" tab selected over the call chat's empty log,
and stayed like that. `renderChatTo` chose the conversation when the roster
named the other person, but did not redraw the log under it. It now redraws
whenever the choice changes. Also, the person you called or answered is
known before the roster arrives, so the chat opens on your conversation with
them straight away. Before: the history never appeared without another
redraw. After: 4 of 4 runs pass, and the messaging harness passes all 432
assertions.

### 6bk. Video effects

Teams' words, from its package: "Video effects", "Background effects",
"None", "Standard blur", "Portrait blur", "Add new", "Filters", "Soft
focus", "Adjust brightness", "Mirror my video", "Background filters". In the
meeting they sit under Camera's arrow (and More); in the pre-join, behind
"Background filters".

**On this device, and private by construction.**

- The camera's frames are segmented by MediaPipe's selfie segmenter
  (vendored, Apache-2.0: `web/vendor/mediapipe`, version 1.1.1, the SIMD
  WebAssembly build and the 250 KB float16 model).
- They are composited on a canvas, and the canvas's stream is what gets
  published. The room never receives the unprocessed picture.
- An image you add is scaled to 1920 wide and kept in this browser
  (IndexedDB, at most 12, 8 MB each as picked). It is never uploaded. Teams
  uploads custom backgrounds to its cloud. The e2e harness checks that no
  request carries it.
- A background you asked to hide is never shown. Until the segmenter has
  loaded (the first time, a 4 MB gzipped download), or if it cannot load,
  the whole frame is blurred rather than sent as it is.

**No effect costs nothing.** With None and no filters there is no pipeline:
the camera's own track is published. The camera passes through
`camTrackFor` wherever it is acquired, as the microphone passes through
`micTrackFor`. Stopping the published track stops the pipeline and the
camera behind it. Every place that turns the camera off therefore turns its
light off without knowing about effects. The harness checks this, and fails
when the camera release is removed.

**Filters** need no segmenter. "Adjust brightness" lifts a dim picture in
proportion to its mean luma, up to 1.8x, and leaves a bright one alone.
"Soft focus" lays a light blur over the sharp picture. "Mirror my video"
changes only your own view; others always see you the right way round.

**Cost on this machine** (corrected in §6bn; Chrome headless, one page in
front, the real pipeline on a drawn person, `tools/e2e/measure-fx-cost.mjs`):

| | Per frame (median) | A segmentation, with its read-back | Frame rate |
| --- | ---: | ---: | ---: |
| GPU on (Intel UHD Graphics) | 8.8 ms | about 18 ms, every frame | 30 fps |
| GPU off | 17.4 ms | about 30 ms, every 2nd frame | 30 fps |

(A first reading, inside the two-browser e2e, said 16.4 and 26.9 ms. There
the camera's page is a background tab and Chrome throttles it.)

- The background is blurred at a quarter of the size and scaled up: a blur
  has no fine detail to lose, and it is a sixteenth of the pixels.
- Where a segmentation costs more than 12 ms, the mask is made every other
  frame. The smoothed mask carries the edge between, and the picture itself
  stays at full rate.
- The cost is the segmentation, not the compositing. A WebGL2 compositor
  was built and measured, and bought nothing (§6bn).

**On the wire.** A's camera over the socket media path, 12 s per window,
VP8 at its 600 kbit/s target (tshark, loopback IPv4, plaintext server,
`tools/e2e/measure-effects.mjs`; A's payload):

| Window | A's camera |
| --- | ---: |
| None | 376 kbit/s |
| Standard blur | 120 kbit/s |
| Portrait blur | 113 kbit/s |
| A plain background | 146 kbit/s |
| None again | 374 kbit/s |

A blurred background costs a third of a sharp one: the encoder has less
detail to describe and undershoots its target. (A first reading of this
capture said the opposite. It had taken the busiest client port, which was
B's unchanging camera, for A's. Corrected the same day; §6bl has the
per-port reading.) The fake camera shows no person, so this measures the
encoder against a blurred or flat frame, not a person against a room.

Tests:

- `web/src/effects.test.mjs`: settings never trusted, which settings need a
  pipeline or the segmenter, blur radius across resolutions, the soft mask
  edge, temporal smoothing, the brightness gain.
- `tools/e2e/effects.e2e.mjs`: two browsers, 32 checks, read on the pixels B
  receives. Blur takes A's detail away, a background shows its colour, your
  own image shows and is never uploaded, None brings the camera back, the
  filters, mirroring, camera off releasing the camera, and the choice kept.
- Layout harness: the panel in the meeting, and the pre-join sheet, at
  every width.

**A defect found on the way.** The pipeline's `stop` called the track's
`stop`, which the app had wrapped to call the pipeline's `stop`: a stack
overflow on "None" and on camera off, caught by the first e2e run. The
pipeline now keeps the browser's own `stop`.

### 6bl. Constant quality on the socket path: VP9 by quantizer

Rule 3: "a static screen should cost almost nothing and a burst of real
motion should be allowed to spend what it needs". A bitrate target does the
opposite: it spends roughly the same on a picture that does not change as on
one that does. The socket media path encoded the camera with VP8 at
600 kbit/s and the screen at 2.5 Mbit/s, both as targets. (The WebRTC path
already negotiates VP9; its rate is the browser's congestion control.)

**The study first** (`tools/e2e/codec-study.mjs`). The same 150 frames at
1280x720 were encoded under each configuration, decoded, and compared with
the originals (PSNR on luma, from RGB). Equal PSNR is equal quality.

| Picture | VP8 600k (before) | VP9 q20 | VP9 q28 | VP9 q36 | VP9 q44 |
| --- | ---: | ---: | ---: | ---: | ---: |
| moving (the fake camera) | 601 kbit/s, 48.9 dB | 660, 55.1 | 588, 54.9 | 531, 54.7 | 479, 54.7 |
| still (one frame repeated) | 579, 49.9 dB | 81, 55.1 | 81, 54.9 | 59, 54.8 | 49, 54.7 |
| flat (a plain background) | 25, 94.7 dB | 5, 99.0 | 5, 86.2 | 5, 98.9 | 5, 79.6 |

A still picture under the VP8 target cost 579 kbit/s, for nothing. Under
VP9 at a constant quantizer it costs 49-81 kbit/s, at higher quality.

Chrome 156 offers quantizer mode for VP9 and AV1, not VP8 (probed with
`VideoEncoder.isConfigSupported`), hence the codec change.

**What changed** (`web/src/wsmedia.js`, `web/src/wire.js`):

- The camera is VP9 at quantizer 32, the screen at 24 (finer: it is text).
  There is no bitrate target.
- A browser without quantizer mode keeps VP8 at the old targets
  (`pickVideoConfig`, unit-tested both ways).
- The codec rides spare flag bit 1 of the media frame. The server relays
  those bits untouched, so no server change and no extra byte.
- A receiver whose publisher changes codec starts a new decoder at the next
  key frame and asks for one meanwhile.
- The backpressure bound is unchanged: past 96 KB queued on the socket,
  screen frames drop; past 192 KB, camera frames too. A burst of motion may
  spend more, but never queues without limit.
- The page reports `data-ws-codec`.

**Why quantizer 32, not 44.** On the fake camera, q44 is cheaper than VP8
and still 6 dB better. But that content is synthetic: flat colours and hard
edges, where every quantizer here scores about 55 dB. Real faces separate
the settings far more, and rule 3 puts quality first. q32 is the safer
default until it can be measured on real footage.

**Measured in a call** (two browsers over the socket path, tshark, 12 s
windows, per client port; `measure-effects.mjs`):

| A's camera | VP8 600k (before) | VP9 q32 (now) |
| --- | ---: | ---: |
| None (the moving fake camera) | 376 kbit/s | 455 kbit/s |
| Standard blur | 120 kbit/s | 88 kbit/s |
| Portrait blur | 113 kbit/s | 82 kbit/s |
| A plain background | 146 kbit/s | 167 kbit/s |

B's camera, moving throughout, went from about 478 to 570 kbit/s. So, both
sides of it:

- Constant motion costs about a fifth more. In the study that buys about
  6 dB of quality.
- A blurred background costs a quarter less.
- A still picture costs 86-92% less. That is the common case in a meeting:
  people mostly sitting still.

That is rule 3's trade, and it is the right one for the average meeting. It
is not a free win on every frame. The fake camera never sits still, so the
still-picture number comes from the study, not the call.

Tests:

- `web/src/wsmedia.test.mjs`: VP9 at quantizer where offered, VP8 at the
  old targets otherwise or when asking throws, the screen's finer quantizer.
- `web/src/mediaframe.test.mjs`: the codec bit and the screen bit, together
  and apart.
- `tools/e2e/effects.e2e.mjs` runs over the socket path. It fails when the
  receiver ignores the codec bit: B sees no picture at all.

### 6bm. The README showcase, and what looking at it found

`tools/e2e/showcase.mjs` makes the README's pictures from the real app. It
uses three fictional people on a fresh server: real chats, real scheduled
meetings, a whiteboard sketch, and a transcript spoken through the same fake
recogniser the recording harness uses. Looking at the pictures, as rule 8
asks, found four defects that every test had passed:

- **Captions were never visible in the gallery.** The caption bar sat at
  z-index 2, under the tiles. The harness checked the bar's text, which was
  there, but nobody could see it. It is now above the tiles, and 80% of the
  stage wide (Teams' `closedCaptionsWidthPercentage`).
- **Your own tile read "Y".** Its avatar letters were made from the label
  "You". A standing rule says avatar letters come from the person, never the
  text on screen (`faceOf`).
- **Leave hung off a phone's screen.** First 55 px, once Camera gained its
  arrow; then 11 px from the host's Leave arrow alone, which predates this
  batch. The bar clips rather than scrolls, so the overflow check could not
  see it. The layout harness now fails on any meeting-bar control past the
  screen's edge, and failed on exactly these before the fix. On a phone,
  Camera's arrow is gone (Video effects stay under More) and Leave is Teams'
  icon-only red hang-up.
- **Your own floating tile on a phone** was crowded by its 44 px menu button,
  over your initials and "You". The button is hidden there; the same
  actions are in People.

### 6bn. The GPU compositor that was not worth keeping

§6bk recorded 16.4 ms a frame with the GPU on, and named a GPU compositor as
the next step. One was built: WebGL2 shaders smoothing the mask over time,
blurring a quarter-size copy of the frame and compositing, with MediaPipe
either sharing its context (so the mask never came back to the CPU) or
handing its mask over to be uploaded. Then it was measured. It was not kept,
and the measurement is why.

**Sharing MediaPipe's context was catastrophic.** With the segmenter bound
to the compositor's own canvas, a segmentation took about 2 s and the
pipeline ran at 3-4 fps. That held with the default context attributes too.
Given its own canvas, the same segmenter ran in 17 ms.

**The compositor itself bought nothing.** Measured alone
(`tools/e2e/measure-fx-cost.mjs`: one page in front, the real pipeline, a
drawn person, 12 s, the last 60 frames, Intel UHD Graphics through ANGLE):

| | Canvas2D | WebGL2 compositor |
| --- | ---: | ---: |
| GPU on: pipeline per frame (median) | 8.8 ms | 8.5 ms |
| GPU off: pipeline per frame (median) | 17.4 ms | 17.3 ms |
| Frame rate, both | 30 fps | 30 fps |

The cost is the segmentation (about 18 ms on average with its read-back,
GPU on), not the compositing. A few hundred lines of shaders for 0.3 ms
were deleted; the Canvas2D path stands.

**And §6bk's numbers were wrong.** They were read inside the two-browser
e2e, where one page is a background tab and Chrome throttles its timers,
including the one drawing the test camera. Measured in front, the pipeline
runs at the camera's full 30 fps: 8.8 ms a frame with the GPU, 17.4 ms
without. §6bk is corrected.

**What the work did leave:**

- **A person in the test.** A drawn head and shoulders is found as a person
  by the segmenter; probed against both delegates, the masks agree. The
  effects harness's camera is now that figure against a plain wall, with
  stripes on the right-hand third, so it checks the actual job: the face
  stays a face while the background is blurred, or replaced by a colour.
- **A defect, found by that test.** On a striped wall the segmenter is only
  half-sure of a drawn face, which shows as a face blended with the new
  background. The blur check could never have caught that, because a
  blurred background near a face is skin-coloured anyway. The
  face-in-front-of-teal check can.
- **Cost markers that tell the truth.** `data-fx-ms`, `data-fx-seg-ms` and
  a new `data-fx-fps` are updated once a second instead of every 60 frames.
  At 4 fps, 60 frames never came within a test, and the markers read
  "undefined" without saying why.
