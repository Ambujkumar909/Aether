# Aether

A meeting engine built on one idea: **most of what a video call transmits is not picture or sound, and almost none of what it discards needed to be sent.**

Quality is never traded for bandwidth here. Bits are removed only when they carry zero perceptual information, and the capacity that frees is spent putting quality back.

---

## The claim, and the arithmetic behind it

Every number below is produced by code in this repository and pinned by a test that fails the build if it regresses. Nothing here is asserted.

```
go run ./cmd/bitbudget -n 4 -minutes 15
```

**A 4-person, 15-minute meeting, per participant, send and receive combined:**

| | baseline | aether | |
|---|---|---|---|
| media payload | 320.40 MB | 204.58 MB | AV1 at the same quality target |
| RTP header | 4.74 MB | 2.16 MB | −54% |
| RTP extensions | 7.90 MB | 2.28 MB | −71%, redundant ones dropped once SSRCs bind |
| SRTP tag | 6.32 MB | 2.88 MB | −54% |
| UDP framing | 3.16 MB | 1.44 MB | −54% |
| IP header | 7.90 MB | 3.60 MB | −54% |
| Ethernet + IFG | 15.01 MB | 6.84 MB | −54% |
| **total on the wire** | **365.42 MB** | **223.79 MB** | |
| **sustained bitrate** | **3.25 Mbps** | **1.99 Mbps** | |
| packets sent | 394,887 | 180,116 | |

The overhead rows all fall by the same 54% because they are all *per packet*, and the packet count more than halved. That is the whole trick: fewer, fuller packets carrying better-compressed content, and nothing sent that nobody can see.

### Where the waste actually is

```
ANATOMY OF ONE 20 ms OPUS PACKET AT 32 kbps
  Ethernet + IFG     38 B  ###########
  IPv4               20 B  ######
  UDP                 8 B  ##
  RTP header         12 B  ###
  RTP extensions     20 B  ######
  payload            80 B  ************************
  SRTP tag           16 B  ####
  TOTAL             194 B   payload is 41.2% of the packet
```

**Fifty-nine percent of an audio packet is not audio.** At 50 packets per second, per stream, for the entire call. Move the same codec at the same bitrate to 60 ms packetisation and drop the extensions the receiver stopped needing after the first second:

```
  payload           240 B  *****************************************
  TOTAL             346 B   payload is 69.4% of the packet
```

Identical codec. Identical bitrate. Identical audio. Three packets became one.

### Measured on real packets, not modelled

`tools/bitcheck.mjs` captures live traffic with tshark and decodes it with the
same `wire.js` the browser ships. Two runs of the same protocol:

| | one frame per message | coalesced bursts |
|---|---|---|
| application payload | 27.1% | **48.0%** |
| IP + TCP header | 64.6% | 45.6% |
| vs a JSON control plane | 3.4× | **22.8×** |

Decoded from the captured bytes: a `StateDelta` averages **3.2 B** and a
`Ping` is exactly **1.0 B** on the wire.

The gap between those columns is the most useful finding in the project. **A
dense protocol does not by itself produce a dense connection** — at one frame
per message, 60 bytes of IP and TCP ride every small payload and the
end-to-end win collapses from 57× per-frame to 3.4× overall. Coalescing is
therefore not a micro-optimisation, it is the mechanism: 12.38 frames per
message nearly doubles payload efficiency with no change to the encoding.

Any claim quoting only the per-frame ratio is telling a true fact
misleadingly. Both columns are reported here for that reason.

### Control plane

A participant state change — mute, camera, raised hand, role — is **3 bytes**. The JSON it replaces, taken from a shipping competitor's protocol, is 172. Measured, not estimated:

```
--- PASS: TestStateDeltaIsThreeBytes
    aether=3 bytes  json=172 bytes  ratio=57.3x
```

A heartbeat is **1 byte**. Over 15 minutes at 40 events per minute: 131.9 kB of JSON becomes 11.2 kB. **11.8×.**

---

## Design principles

**1. Delete only bits that were never quality.** There are four kinds of bits in a call. Three of them are free to remove and one is sacred.

| | example | perceptual value |
|---|---|---|
| Overhead | headers, JSON signalling, RTCP churn | zero |
| Undelivered | 720p sent to a 160px tile; audio from silent participants | zero — unperceivable by construction |
| Codec waste | VP8 vs AV1 at equal quality | zero — same picture, more bits |
| **Quality** | what you are actually looking at | **the product** |

**2. No bitrate ceiling. Optimise the average.** Near-CBR is why mainstream video smears during motion: complexity spikes 5–10× for a few hundred milliseconds and a CBR encoder can only respond by raising the quantiser. Aether runs high-peak VBR — quiet at 1.2–2.5 Mbps, unclamped during motion — and lands at a *lower* fifteen-minute average with sharper motion.

The burst is bounded by *measured* capacity, not by policy. Pushing 8 Mbps into a 2 Mbps path does not buy sharpness; it parks 6 Mbps in a queue 1.5 seconds deep, adds latency, and trips our own congestion detector. On a link that carries 50 Mbps, the estimate climbs there and bursts take it.

**3. Never introduce cross-frame state unless recovery from its loss is cheap.** Learned the expensive way in a sibling project, where a delta codec that was 24× cheaper per frame on paper spent 13–21 MB repainting one screen after a single drop. Recovery was the most expensive operation in the system, so loss made congestion worse exactly when it was already bad. The interning dictionary here is cross-frame state, and it is safe **only** because it runs over a reliable ordered transport that guarantees it is never tested. That constraint is documented at the top of `internal/wire/doc.go` and must not be violated.

---

## Security

Authentication, user management, and the audit log are first-class modules, not an afterthought bolted to the side.

**Credentials.** Argon2id at OWASP parameters, PHC-encoded so the cost can be raised later without invalidating existing hashes. Session tokens are 256-bit random values, and only their SHA-256 is stored — a database dump does not hand the reader a set of live sessions. The token lives in an `HttpOnly` cookie, so an injected script cannot read it; CSRF is a double-submit token in a companion cookie the page *can* read and echo in a header, which a cross-origin attacker cannot do.

**Account enumeration is closed.** Wrong password, unknown address, locked account, and suspended account all return one identical message, and an unknown address still pays the full argon2 verification so the timing matches. The audit log records the true reason; the response never does.

**Authorisation is a table you can read on one screen.** Four roles, an explicit permission map, every check resolved server-side on every request. The client receives its permission list only so it can avoid rendering buttons that would fail. Escalation above your own rank is refused, changing your own role is refused, and removing the last active administrator is refused — locking everyone out of their own installation cannot be undone from inside the product.

**Privilege changes take effect immediately.** A role change, a suspension, or a password change revokes every session for that user. A password change is how someone evicts an intruder, so it has to actually evict them.

### The audit log is tamper-evident, not merely append-only

Every entry carries the SHA-256 of the entry before it. Altering, deleting, or reordering anything breaks the chain from that point forward, and the break is detectable by anyone holding the head hash.

```
--- PASS: TestEditingARowIsDetected
    detected: event 5 stores hash 9ff3073d9bfe… but its contents hash to
    80fc93d53d5d…: the row was edited in place

--- PASS: TestDeletingARowIsDetected
    detected: event 7 links to 0b0314c00cf4… but the previous event hashes to
    f6efcfd60e67…: an entry was altered, deleted, or reordered

--- PASS: TestRecomputingOneHashStillFails
    detected at the following link: event 6 links to d40ede44b6f6… …
```

The third case is the one that matters. An attacker who edits a row *and* correctly recomputes its own hash still fails, because the next entry commits to the old value — they would have to rewrite every subsequent row, which is exactly what makes an externally pinned head hash worth having.

The field encoding is length-prefixed rather than delimiter-joined, so an actor-controlled field cannot be used to forge a colliding digest. `Verify` walks the whole chain and is exposed to operators: a tamper-evident log whose evidence nobody can inspect is a claim, not a control.

---

## What is built and proven

| | status | evidence |
|---|---|---|
| **Binary wire protocol** | complete | exact-size assertions, fuzzed decoder |
| **Browser codec (JS)** | complete | 24 cross-language interop checks against the live server |
| **Bit ledger** | complete | analytic model + runtime counters + `bitbudget` CLI |
| **Bandwidth allocator** | complete | 9 tests; 4.2x on a 12-person gallery |
| **Congestion control** | complete | 6 tests; 32x faster recovery than a GCC reference |
| **RTP forwarder** | complete | 8 tests; 6 layer switches, zero discontinuities |
| **Keyframe detection** | complete | VP8 / VP9 / H.264 / AV1, hand-built vectors + fuzz |
| **WebRTC media forwarding** | working | two pion peers exchange real media through the router |
| **Identity: auth, RBAC, sessions** | complete | 17 tests incl. enumeration, lockout, escalation |
| **Audit log** | complete | 9 tests incl. three distinct tamper scenarios |
| **Modular kernel** | complete | dependency-ordered start, reverse shutdown |
| **Web client** | working | dual theme, admin console, live ledger |
| **Simulcast arbitration end-to-end** | partial | ladder published and forwarded; viewport-to-layer loop needs field tuning |
| **Remote control / take control** | **not yet** | see roadmap |

### Verify it yourself

```bash
cd engine
go test ./...                              # 67 tests
go run ./cmd/bitbudget -n 4 -minutes 15    # the full accounting
go run ./cmd/aether -addr :8899 -web ../web

# The server prints a bootstrap administrator and a one-time password
# on an empty database. Sign in with it at http://localhost:8899

cd ../web
npm install                                # ws, for the test harness only
AETHER_EMAIL=admin@aether.local AETHER_PASSWORD=... npm run interop
```

---

## Architecture

```
engine/
  cmd/aether/            the server
  cmd/bitbudget/         the accounting, on demand
  internal/
    wire/                binary protocol. 1-byte ping, 3-byte state delta
    ledger/              every byte attributed to a layer, RFC-sourced
    sfu/                 allocator, congestion control, RTP forwarder, router
    modular/             module lifecycle, dependency-ordered
    room/                participants, speaker ranking
    signal/              WebSocket gateway, 15 ms coalescing window
    modules/
      identity/          users, credentials, sessions, RBAC
      audit/             hash-chained, tamper-evident record
    platform/
      database/          the single handle, forward-only migrations
      httpx/             JSON, middleware, rate limiting
web/
  src/wire.js            the browser half of the codec
  src/rtc.js             publisher + subscriber peer connections
  src/console.js         administration: people, audit, sessions
  src/api.js             CSRF-aware transport
  styles.css             dual theme
```

A **modular monolith**, matching the contract already used in ITCM: vertical slices under `internal/modules`, shared plumbing under `internal/platform`, and no module importing another module's internals — `identity` holds an `audit.Recorder`, `audit` holds an `audit.Guard` that `identity` happens to satisfy, and neither knows the other's types.

The reason for a monolith is latency. The distance between "a packet arrived" and "the packet was forwarded" is the product, and every process boundary on that path is a serialisation, a scheduler hop, and a queue.

### Two peer connections per participant

The publisher connection is always offered by the client; the subscriber connection is always offered by the server. That asymmetry makes renegotiation glare *structurally impossible* rather than something to survive — each connection has exactly one possible offerer, so there is nothing to arbitrate. The cost is a second ICE and DTLS handshake at join, paid once, in parallel, before anyone is looking.

### The forwarder is where an SFU is won or lost

Each inbound simulcast rung has its own sequence and timestamp space; the receiver believes it is watching one stream. A forwarder that simply relays whichever layer is selected produces, on the first quality change, a sequence jump of thousands, a jitter buffer that concludes it lost an enormous burst, and a freeze until the next keyframe. Users experience this as "it stutters whenever the network changes."

This forwarder maintains its own outbound space and translates every packet into it. Switching is invisible:

```
--- PASS: TestManySwitchesStayContiguous
    6 switches, 117 packets, zero discontinuities
```

Upstream loss still shows through as a gap, because the receiver's concealment and NACK logic depends on seeing it.

---

## Roadmap

1. **Viewport-to-layer loop tuning against real telemetry.** The allocator, the forwarder, and the `Viewport` frame all exist and are wired; `MinUtilityPerMbps` is currently fitted to two hand-worked cases rather than to field data.
2. **Remote control and take-control** — port from ITCM's `agent/internal/rc`, which already solves screen capture, dirty-region detection with sequence-number recovery, `SendInput` injection, and the secure attention sequence on Windows and macOS. Its constraint carries over intact: self-contained frames, cheap recovery.
3. **End-to-end encryption** via insertable streams. The speaker-ranking design already anticipates it — energy is reported by the client precisely so the server never needs to listen.
4. **Recording, breakouts, reactions** — opcodes reserved, handlers not written.
5. **TURN deployment** for symmetric NAT, and SFU-to-SFU relay past 500 participants per node, where a compact trunk framing would pay for itself immediately.

**Known work items:**

- `Estimator.Observe` uses a fixed exponential filter; a trendline or Kalman estimator would classify overuse earlier.
- Layer resolutions are seeded from the conventional ladder rather than read from the publisher, so the allocator's spatial term is approximate until a rung has been measured.
- Sessions are single-factor. TOTP enrolment is the obvious next control, and the audit vocabulary already has room for it.
