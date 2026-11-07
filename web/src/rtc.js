// The browser half of the media plane.
//
// Two peer connections, matching the server. The publisher connection is
// always offered by us; the subscriber connection is always offered by the
// server and we only ever answer on it. Neither side ever has to arbitrate a
// glare, because on each connection there is only one possible offerer.

import { Dir } from './wire.js';

// The simulcast ladder we publish.
//
// Chrome requires encodings ordered lowest first. The RIDs match what the
// server maps in layerFromRID, and the bitrates match the conventional ladder
// the allocator assumes before its first measurement window closes.
//
// maxBitrate on the top rung is deliberately generous rather than a target.
// It is a clamp on how far a motion burst may spike, and clamping it tightly
// is precisely the near-CBR behaviour that smears a hand wave. The average is
// governed by the congestion controller, not by this number.
// Caps sized for VP9, which is now preferred (preferVideoCodecs). Measured
// on the same camera-like footage with the browser's own real-time encoders
// (ARCHITECTURE.md): VP9 at 250 kbit/s scored 42.2 dB against VP8's 40.0,
// and VP9 levels off near 42.6 dB by 500 kbit/s where VP8 needs ~1.2 Mbit/s
// for 42.5. An encoder spends the cap it is given, so without lower caps the
// better codec buys only quality, not bandwidth. The full rung keeps 2 Mbit/s
// of headroom for motion -- above where VP9 levels off, so no quality is
// traded -- against 4 Mbit/s before. L1T1: one spatial and one temporal
// layer per simulcast stream, which is what Chrome needs to simulcast VP9.
const LADDER = [
  { rid: 'q', scaleResolutionDownBy: 4, maxBitrate: 150_000, maxFramerate: 15, scalabilityMode: 'L1T1' },
  { rid: 'h', scaleResolutionDownBy: 2, maxBitrate: 500_000, maxFramerate: 30, scalabilityMode: 'L1T1' },
  { rid: 'f', scaleResolutionDownBy: 1, maxBitrate: 2_000_000, maxFramerate: 30, scalabilityMode: 'L1T1' },
];

// The video codecs, best first: VP9, then VP8 as the fallback every WebRTC
// endpoint has. H.264 and AV1 are kept but last: H.264 for compatibility,
// AV1 because its encoder is the heaviest on a low-end phone and older Safari
// cannot decode it. Retransmission and FEC entries keep their places after
// the codecs they protect. Pure: a capability list in, a reordered one out.
export const VIDEO_PREFERENCE = ['video/VP9', 'video/VP8'];
export function preferVideoCodecs(codecs, order = VIDEO_PREFERENCE) {
  const rank = (c) => {
    const i = order.findIndex((m) => m.toLowerCase() === (c.mimeType || '').toLowerCase());
    if (i < 0) return order.length;
    // VP9 profile 0 (8-bit 4:2:0) before profile 2 (10-bit): what every
    // decoder supports.
    const prof = /profile-id=(\d+)/.exec(c.sdpFmtpLine || '');
    return i + (prof && prof[1] !== '0' ? 0.5 : 0);
  };
  return codecs
    .map((c, i) => ({ c, i, r: rank(c) }))
    .sort((a, b) => a.r - b.r || a.i - b.i)
    .map((x) => x.c);
}

// Apply the preference to a video transceiver; a browser without
// setCodecPreferences keeps its own order.
function preferOn(tx) {
  try {
    const caps = globalThis.RTCRtpSender?.getCapabilities?.('video')?.codecs;
    if (caps?.length && tx.setCodecPreferences) tx.setCodecPreferences(preferVideoCodecs(caps));
  } catch (e) {
    console.warn('codec preference not applied:', e?.message);
  }
}

// A screen share needs a far higher ceiling than a camera tile.
//
// Browsers default a screen encoding conservatively, around 1 Mbps, which is
// not enough for legible text at 1440p or above -- the encoder meets the
// ceiling by blurring, and the result is the soft, smeary share everybody
// recognises. Two and a half megabits is comfortably inside any connection
// that can carry a meeting at all, and because a static screen sends almost
// nothing, the ceiling only costs anything during the moments it is needed.
const SCREEN_MAX_BITRATE = 2_500_000;

// Opus, configured rather than defaulted.
//
// Left alone, a browser negotiates Opus at around 32 kbps mono. That is
// enough to be understood and audibly thin for a voice somebody is meant to
// listen to for an hour, and it is where "the audio sounds poor" came from.
// This project has no bitrate ceiling and audio is the one stream a meeting
// genuinely cannot do without -- degrading it to protect video is always the
// wrong trade -- so it gets a real allocation. 64 kbps is transparent for
// speech; the difference from 32 is not subtle.
const AUDIO_MAX_BITRATE = 64_000;

/**
 * Ask for in-band FEC and a real bitrate on every Opus payload type.
 *
 * FEC is the half that matters under loss: Opus carries a coarse copy of the
 * previous frame inside the next one, so an isolated lost packet is
 * reconstructed instead of becoming a gap. That is a quality property bought
 * with a few percent of bitrate, which is the right side of the trade.
 *
 * DTX is deliberately NOT enabled, even though "idle must cost zero" argues
 * for it. Opus DTX clips the onset of speech after silence, and the thing
 * being fixed here is exactly that the audio sounds bad. Silence suppression
 * is worth revisiting with a measurement of what it actually saves against
 * what it costs at the start of every sentence; asserting the saving without
 * that measurement is what this codebase does not do.
 */
// How many early candidates are held per connection before the remote
// description arrives (see handleICE).
export const EARLY_ICE_MAX = 32;

export function tuneOpus(sdp) {
  const want = `useinbandfec=1;maxaveragebitrate=${AUDIO_MAX_BITRATE};maxplaybackrate=48000`;
  const managed = /^(useinbandfec|maxaveragebitrate|maxplaybackrate)=/i;
  let out = sdp;
  for (const m of sdp.matchAll(/^a=rtpmap:(\d+)\s+opus\/48000/gim)) {
    const pt = m[1];
    const fmtp = new RegExp(`^a=fmtp:${pt} (.*)$`, 'im');
    if (fmtp.test(out)) {
      out = out.replace(fmtp, (_line, params) => {
        // Keep whatever the browser negotiated that we are not overriding;
        // replacing the whole line drops things like stereo and cbr.
        const kept = params
          .split(';')
          .map((x) => x.trim())
          .filter((x) => x && !managed.test(x));
        return `a=fmtp:${pt} ${[...kept, ...want.split(';')].join(';')}`;
      });
    } else {
      out = out.replace(
        new RegExp(`^(a=rtpmap:${pt} opus/48000.*)$`, 'im'),
        `$1\r\na=fmtp:${pt} ${want}`
      );
    }
  }
  return out;
}

export class MediaPlane {
  /**
   * @param {object} io  { sendSDP(dir,isOffer,body), sendICE(dir,candidate) }
   * @param {object} hooks { onRemoteTrack(track, stream, publisherHint) }
   */
  constructor(io, hooks) {
    // Every operation that mutates a peer connection is chained onto this, so
    // no two ever overlap. See _enqueue.
    this._chain = Promise.resolve();
    this._renegotiateWhenStable = false;
    this.io = io;
    this.hooks = hooks;
    this.pub = null;
    this.sub = null;
    this.senders = new Map(); // kind -> RTPSender
    this.publishing = false;
    this.remoteStreams = new Map(); // streamId -> MediaStream
  }

  async start(iceServers) {
    const cfg = {
      iceServers,
      // Pooling pre-gathers candidates so the first offer already carries
      // them, which removes a round trip from join.
      iceCandidatePoolSize: 4,
      bundlePolicy: 'max-bundle',
      // 'all' keeps the fast direct path and lets the browser fall back to a
      // relay only when no direct pair forms. 'relay' would force every call
      // through the TURN server even where direct works, slower for no reason.
      iceTransportPolicy: 'all',
    };
    this._iceCfg = cfg;

    this.pub = new RTCPeerConnection(cfg);
    this.sub = new RTCPeerConnection(cfg);

    this.pub.onicecandidate = (e) => {
      if (e.candidate) this.io.sendICE(Dir.Publisher, JSON.stringify(e.candidate.toJSON()));
    };
    this.sub.onicecandidate = (e) => {
      if (e.candidate) this.io.sendICE(Dir.Subscriber, JSON.stringify(e.candidate.toJSON()));
    };

    this.sub.ontrack = (e) => {
      const stream = e.streams[0];
      if (stream) this.remoteStreams.set(stream.id, stream);
      // The server sets the outbound stream id to the publishing peer's id,
      // so the receiver knows whose feed this is without a side channel.
      this.hooks.onRemoteTrack?.(e.track, stream, stream?.id ?? '');
    };

    this.pub.onconnectionstatechange = () => {
      this.hooks.onState?.('publisher', this.pub.connectionState);
      this._maybeRestart(this.pub, Dir.Publisher);
    };
    this.sub.onconnectionstatechange = () => {
      this.hooks.onState?.('subscriber', this.sub.connectionState);
      this._maybeRestart(this.sub, Dir.Subscriber);
    };
    // iceconnectionstate reaches 'failed' sooner than connectionstate on some
    // browsers, so watch both -- whichever fires first triggers recovery.
    this.pub.oniceconnectionstatechange = () => this._maybeRestart(this.pub, Dir.Publisher);
    this.sub.oniceconnectionstatechange = () => this._maybeRestart(this.sub, Dir.Subscriber);

    // The subscriber connection must be able to receive before the server
    // offers anything, and a connection with no transceivers cannot be
    // negotiated at all.
    this.sub.addTransceiver('audio', { direction: 'recvonly' });
    this.sub.addTransceiver('video', { direction: 'recvonly' });

    // An offer the server made before these connections existed. The server
    // subscribes a newcomer to everyone already publishing the moment they
    // join; that offer can land while start() still awaits the relay
    // credentials, and was then dropped -- the newcomer never received
    // anybody's camera (a slow link, joinrace.e2e.mjs). Applied now, in order.
    if (this._earlySubOffer) {
      const body = this._earlySubOffer;
      this._earlySubOffer = null;
      this.handleSDP({ dir: Dir.Subscriber, isOffer: true, body }).catch(() => {});
    }
  }

  /**
   * Publish a local stream. Video goes up as a three-rung simulcast ladder so
   * the server has something to choose between; audio goes up once.
   */
  async publish(stream) {
    if (!this.pub || !stream) return;

    for (const track of stream.getTracks()) {
      if (this.senders.has(track.kind)) {
        await this.senders.get(track.kind).replaceTrack(track);
        continue;
      }
      const init = { direction: 'sendonly', streams: [stream] };
      if (track.kind === 'video') {
        init.sendEncodings = LADDER;
      }
      const tx = this.pub.addTransceiver(track, init);
      if (track.kind === 'video') preferOn(tx);
      this.senders.set(track.kind, tx.sender);
    }

    // Prefer sharpness over smoothness when the encoder has to choose. A
    // support session or a shared document is unreadable at low resolution
    // and merely slightly jerky at low frame rate.
    const vs = this.senders.get('video');
    if (vs) {
      const params = vs.getParameters();
      params.degradationPreference = 'balanced';
      try {
        await vs.setParameters(params);
      } catch {
        // Not every browser accepts every parameter; the ladder still applies.
      }
    }

    // What the fmtp line asks for is a hint to the far end; this is the lever
    // that actually governs what this browser sends. Without it the encoder
    // sits at its default no matter what the SDP says.
    const as = this.senders.get('audio');
    if (as) {
      const params = as.getParameters();
      if (!params.encodings || !params.encodings.length) params.encodings = [{}];
      params.encodings[0].maxBitrate = AUDIO_MAX_BITRATE;
      // Audio wins the contest for a constrained uplink. Video degrades
      // gracefully and speech does not.
      params.encodings[0].priority = 'high';
      params.encodings[0].networkPriority = 'high';
      try {
        await as.setParameters(params);
      } catch {
        // An older browser may refuse; the fmtp hint still stands.
      }
    }

    this.publishing = true;
    await this.negotiate();
  }

  /** Replace the outbound video track, for screen share swaps. */
  async replaceVideo(track) {
    return this._enqueue(async () => {
      const s = this.senders.get('video');
      if (s) await s.replaceTrack(track);
    });
  }

  /**
   * Send this video track, creating a sender if there is not one yet.
   *
   * replaceVideo cannot do this. replaceTrack needs an existing sender, and
   * when the camera failed at join there is none -- so sharing a screen did
   * nothing at all, with no error, which reads as a dead button. The first
   * screen share of a session is exactly when this happens, because somebody
   * whose camera is busy is precisely the person who shares their screen
   * instead.
   *
   * Adding a transceiver requires renegotiation, which replaceTrack does not.
   * That is the cost of going from nothing to something, and it is paid once.
   */
  /**
   * Send a screen share as a SECOND track, alongside the camera.
   *
   * The earlier implementation replaced the camera track on the existing
   * video sender, which is why presenting made the presenter disappear. Every
   * product people expect this to behave like -- Meet, Zoom, Teams -- keeps
   * both: the presenter stays a participant tile and the presentation becomes
   * its own tile that can be pinned, unpinned, and viewed alongside faces.
   *
   * Deliberately no simulcast ladder. A shared screen is mostly static and
   * needs resolution far more than it needs frame rate or layer choice, and
   * three encodings of the same slide is bandwidth spent to give the
   * allocator a choice it should not make. The allocator already treats
   * screen shares differently for this reason.
   *
   * The transceiver is created once and kept. Stopping a share replaces its
   * track with null rather than removing the transceiver, so starting and
   * stopping repeatedly costs no renegotiation after the first time -- which
   * is what made rapid toggling unreliable before.
   */
  async startScreen(track) {
    // contentHint is the single most effective lever a browser exposes for
    // screen share quality, and leaving it unset is why sharing looked soft
    // and stuttered.
    //
    // Without a hint the encoder guesses from resolution and motion, and it
    // guesses badly for the commonest case: a mostly static document with
    // occasional scrolling. It sees low motion, decides frame rate is what
    // matters, and spends its budget holding 30 fps of a slide that has not
    // changed while dropping the spatial detail that makes the text legible.
    // 'detail' tells it the opposite, which is also exactly how the server's
    // allocator weights a screen: 0.9 spatial against 0.1 temporal.
    try {
      track.contentHint = 'detail';
    } catch {
      // Not every browser implements it; the bitrate ceiling below still does.
    }

    return this._enqueue(async () => {
      const existing = this.senders.get('screen');
      if (existing) {
        await existing.replaceTrack(track);
        return;
      }
      if (!this.pub || this.pub.signalingState === 'closed') return;

      const tx = this.pub.addTransceiver(track, {
        direction: 'sendonly',
        sendEncodings: [{ maxBitrate: SCREEN_MAX_BITRATE, maxFramerate: 30 }],
      });
      preferOn(tx);
      this.senders.set('screen', tx.sender);
      await this._negotiateNow();
    });
  }

  /** Stop sending the screen, leaving the camera untouched. */
  async stopScreen() {
    return this._enqueue(async () => {
      const s = this.senders.get('screen');
      if (s) await s.replaceTrack(null);
    });
  }

  async ensureVideo(track) {
    return this._enqueue(async () => {
      const existing = this.senders.get('video');
      if (existing) {
        await existing.replaceTrack(track);
        return;
      }
      if (!this.pub || this.pub.signalingState === 'closed') return;
      const tx = this.pub.addTransceiver(track, {
        direction: 'sendonly',
        sendEncodings: LADDER,
      });
      preferOn(tx);
      this.senders.set('video', tx.sender);
      // Inline rather than calling negotiate(), which would enqueue a second
      // operation behind this one and deadlock the chain on itself.
      await this._negotiateNow();
    });
  }

  /**
   * Send a microphone that was not there at publish: the person joined
   * muted with no track, or lost the device. Same shape as ensureVideo --
   * an existing sender takes the track without renegotiation; a missing one
   * is added and negotiated.
   */
  async ensureAudio(track) {
    return this._enqueue(async () => {
      const existing = this.senders.get('audio');
      if (existing) {
        await existing.replaceTrack(track);
        return;
      }
      if (!this.pub || this.pub.signalingState === 'closed') return;
      const tx = this.pub.addTransceiver(track, { direction: 'sendonly' });
      this.senders.set('audio', tx.sender);
      await this._negotiateNow();
    });
  }

  /**
   * Stop or resume sending one kind of media, for real.
   *
   * The obvious implementation is `track.enabled = false`, and it is wrong.
   * A disabled MediaStreamTrack does not stop the encoder: per spec it
   * produces black frames for video and digital silence for audio, and RTP
   * keeps flowing the entire time. The SFU then dutifully forwards those
   * black frames to every subscriber, so one participant with their camera
   * "off" in a forty person meeting still costs the server thirty-nine
   * outbound streams of nothing. Idle has to cost zero, not a little.
   *
   * `replaceTrack(null)` genuinely stops it, and it does so without
   * renegotiation: the transceiver and its m-line stay exactly where they
   * are, so there is no offer/answer round trip and no chance of glare. The
   * far end sees the track go silent, which is what it should see.
   */
  /** Whether a sender of this kind exists and carries a track right now. */
  sending(kind) {
    return !!this.senders.get(kind)?.track;
  }

  async setSending(kind, on, track = null) {
    return this._enqueue(async () => {
      const s = this.senders.get(kind);
      if (!s) return;
      // replaceTrack needs no renegotiation, but it must still not interleave
      // with one: swapping the track underneath an offer that is describing it
      // produces a description that does not match what is being sent.
      await s.replaceTrack(on ? track : null);
    });
  }

  /** Offer on the publisher connection. */
  /**
   * Run one signalling operation, never overlapping another.
   *
   * Everything that touches a peer connection's description goes through
   * here, and it is the difference between a robust client and one that
   * wedges the first time somebody clicks quickly.
   *
   * A peer connection is a state machine with exactly one slot for a pending
   * local description. Calling createOffer/setLocalDescription while an
   * earlier offer is still waiting for its answer throws InvalidStateError
   * and leaves the connection stuck in "have-local-offer" -- permanently,
   * because nothing retries. The visible symptom is precise and was reported
   * exactly: toggle the camera on and off quickly a few times and it stops
   * responding altogether, for the rest of the meeting.
   *
   * The chain deliberately survives a failed operation. If one toggle throws,
   * the next must still run; a queue that breaks on first error would turn a
   * transient fault into the same permanent wedge it exists to prevent.
   */
  _enqueue(fn) {
    const run = this._chain.then(fn, fn).catch((err) => {
      console.error('signalling operation failed:', err?.name, err?.message);
    });
    this._chain = run;
    return run;
  }

  /** Offer on the publisher connection, serialised and state-checked. */
  async negotiate() {
    return this._enqueue(() => this._negotiateNow());
  }

  async _negotiateNow(iceRestart = false) {
    if (!this.pub || this.pub.signalingState === 'closed') return;

    // Not stable means an offer/answer is already in flight. Remember that
    // another round is needed and run it when this one settles, rather than
    // forcing a second offer into a connection that cannot accept one.
    if (this.pub.signalingState !== 'stable') {
      this._renegotiateWhenStable = true;
      return;
    }

    const offer = await this.pub.createOffer(iceRestart ? { iceRestart: true } : undefined);
    // createOffer is asynchronous, so the state can have moved underneath it.
    if (this.pub.signalingState !== 'stable') {
      this._renegotiateWhenStable = true;
      return;
    }
    // Munged before setLocalDescription, and the SAME text is what goes to
    // the server: describing one thing locally and sending another is how
    // descriptions and reality drift apart.
    offer.sdp = tuneOpus(offer.sdp);
    await this.pub.setLocalDescription(offer);
    this.io.sendSDP(Dir.Publisher, true, offer.sdp);
  }

  /**
   * Re-run ICE when a connection has failed, so a call survives a path going
   * away -- a tab backgrounded long enough for the network to drop the
   * binding, a Wi-Fi handover, or a first attempt that never formed a direct
   * pair and can be rescued by relaying.
   *
   * Guarded so it fires once per failure, not once per event: browsers emit
   * several state changes around a failure, and restarting on each would spam
   * offers. Only the publisher restarts itself; a restart re-gathers
   * candidates, and with the relay configured that now includes relay
   * candidates that were not tried the first time.
   */
  _maybeRestart(pc, dir) {
    // The publisher has been handed to the WebSocket. Do not restart it: a
    // restart re-offers, re-checks, and fails again on a network that already
    // refused it, and every cycle disturbs the media that is now flowing over
    // the socket instead.
    if (this._publisherAbandoned && dir === Dir.Publisher) return;
    const st = pc.iceConnectionState;
    if (st !== 'failed' && st !== 'disconnected') {
      if (st === 'connected' || st === 'completed') this._restarting?.delete(dir);
      return;
    }
    this._restarting ??= new Set();
    if (this._restarting.has(dir)) return;
    this._restarting.add(dir);

    // 'disconnected' often recovers on its own within a few seconds; give it
    // that chance before forcing a restart, which is disruptive. 'failed' is
    // terminal and restarted at once.
    const delay = st === 'failed' ? 0 : 3000;
    setTimeout(() => {
      const now = pc.iceConnectionState;
      if (now === 'connected' || now === 'completed' || now === 'closed') {
        this._restarting.delete(dir);
        return;
      }
      if (dir === Dir.Publisher) {
        this._enqueue(() => this._negotiateNow(true)).catch(() => {});
      } else {
        // The subscriber is server-offered; a full subscriber ICE restart
        // needs the server to offer again, which it does on the next roster
        // change. Nudge whoever is listening to prompt that.
        this.hooks.onSubscriberStalled?.();
      }
    }, delay);
  }

  /**
   * Give up on the WebRTC publisher: stop restarting it and empty its senders
   * so nothing more goes out over it. Called when media moves to the
   * WebSocket, so the far side hears one continuous stream from one transport
   * rather than a torn one from two. The subscriber connection is untouched:
   * incoming WebRTC media from peers who CAN reach us directly still arrives.
   */
  abandonPublisher() {
    this._publisherAbandoned = true;
    for (const [kind, sender] of this.senders) {
      if (kind === 'audio' || kind === 'video' || kind === 'screen') {
        sender.replaceTrack(null).catch(() => {});
      }
    }
  }

  /** Handle a signalling frame from the server. */
  async handleSDP(msg) {
    return this._enqueue(() => this._handleSDPNow(msg));
  }

  async _handleSDPNow({ dir, isOffer, body }) {
    if (dir === Dir.Publisher) {
      if (isOffer) return; // the server never offers on this connection
      if (!this.pub || this.pub.signalingState === 'closed') return;
      // An answer is only meaningful against our own outstanding offer.
      // Applying one in any other state is an error that wedges the
      // connection, and it happens whenever an answer arrives late.
      if (this.pub.signalingState !== 'have-local-offer') return;

      await this.pub.setRemoteDescription({ type: 'answer', sdp: body });
      this._flushIce(Dir.Publisher);

      // Back to stable. If a toggle asked for another round while this one
      // was in flight, run it now -- otherwise that change is silently lost
      // and the camera stays in the wrong state.
      if (this._renegotiateWhenStable) {
        this._renegotiateWhenStable = false;
        await this._negotiateNow();
      }
      return;
    }

    // Subscriber connection: the server offers, we answer. Always.
    if (!isOffer) return;
    if (!this.sub) {
      // Before start(): kept, and applied when the connection exists. The
      // server has one offer outstanding at a time, so the latest is the one.
      this._earlySubOffer = body;
      return;
    }
    if (this.sub.signalingState === 'closed') return;
    await this.sub.setRemoteDescription({ type: 'offer', sdp: body });
    this._flushIce(Dir.Subscriber);
    const answer = await this.sub.createAnswer();
    answer.sdp = tuneOpus(answer.sdp);
    await this.sub.setLocalDescription(answer);
    this.io.sendSDP(Dir.Subscriber, false, answer.sdp);
  }

  // A candidate that arrives before the remote description is held, not
  // dropped. The server trickles its candidate the moment it has one, which
  // is before its answer reaches us -- so it ALWAYS arrived early, and
  // addIceCandidate refused it ("The remote description was null"). On an
  // open LAN nobody noticed: the browser learned the server's address anyway,
  // as peer-reflexive, from the server's own connectivity checks. Behind a
  // firewall or NAT that drops those inbound checks it never learned it, and
  // WebRTC could not connect at all -- measured through the impairment proxy
  // (tools/impair), where the only path is the one the candidate names.
  // Bounded: a server does not send more than a handful.
  async handleICE({ dir, candidate }) {
    const pc = dir === Dir.Publisher ? this.pub : this.sub;
    let c;
    try {
      c = JSON.parse(candidate);
    } catch {
      return;
    }
    // No connection yet (start() still running) or no remote description:
    // held, bounded, and added once the description is set (_flushIce).
    if (!pc || !pc.remoteDescription) {
      const q = (this._earlyIce ??= new Map());
      const list = q.get(dir) ?? [];
      if (list.length < EARLY_ICE_MAX) list.push(c);
      q.set(dir, list);
      return;
    }
    try {
      await pc.addIceCandidate(c);
    } catch {
      // A candidate for a superseded ICE generation; nothing to do.
    }
  }

  _flushIce(dir) {
    const list = this._earlyIce?.get(dir);
    if (!list?.length) return;
    this._earlyIce.set(dir, []);
    const pc = dir === Dir.Publisher ? this.pub : this.sub;
    for (const c of list) pc?.addIceCandidate(c).catch(() => {});
  }

  /**
   * Receiver-side bandwidth estimate, read from the real transport rather
   * than guessed. Returned in the shape the BweReport frame expects.
   */
  async readStats() {
    if (!this.sub) return null;
    let bytes = 0;
    let jitter = 0;
    let lost = 0;
    let received = 0;
    let rtt = 0;
    let available = 0;

    const stats = await this.sub.getStats();
    stats.forEach((r) => {
      if (r.type === 'inbound-rtp') {
        bytes += r.bytesReceived ?? 0;
        jitter = Math.max(jitter, (r.jitter ?? 0) * 1000);
        lost += r.packetsLost ?? 0;
        received += r.packetsReceived ?? 0;
      }
      if (r.type === 'candidate-pair' && r.state === 'succeeded') {
        rtt = (r.currentRoundTripTime ?? 0) * 1000;
        available = r.availableIncomingBitrate ?? 0;
      }
    });

    const total = lost + received;
    return {
      bytes,
      estimateKbps: Math.round(available / 1000),
      lossFraction: total > 0 ? lost / total : 0,
      jitterMs: Math.round(jitter),
      rttMs: Math.round(rtt),
    };
  }

  // Cumulative counters for Call health (health.js), sent and received, by
  // audio / camera video / screen. A screen is told from a camera by its
  // track id: ours in screenIds as a media-source, theirs as an inbound
  // track. Rates are the caller's to compute between two samples.
  async healthSample(screenIds = new Set()) {
    const out = {};
    const inn = {};
    let rtt = null;
    const into = (side, k, r, dir) => {
      const s = (side[k] ??= { bytes: 0, lost: 0, recv: 0, w: 0, h: 0, fps: 0 });
      s.bytes += (dir === 'out' ? r.bytesSent : r.bytesReceived) ?? 0;
      s.lost += r.packetsLost ?? 0;
      s.recv += r.packetsReceived ?? 0;
      // The largest stream of a kind stands for it: the one on the stage.
      if ((r.frameWidth ?? 0) * (r.frameHeight ?? 0) > s.w * s.h) {
        s.w = r.frameWidth;
        s.h = r.frameHeight;
        s.fps = r.framesPerSecond ?? 0;
      }
    };
    const pair = (r) => {
      if (r.type === 'candidate-pair' && r.state === 'succeeded' && r.currentRoundTripTime != null) rtt = r.currentRoundTripTime * 1000;
    };
    if (this.pub) {
      const st = await this.pub.getStats();
      const sources = new Map();
      st.forEach((r) => r.type === 'media-source' && sources.set(r.id, r));
      st.forEach((r) => {
        pair(r);
        if (r.type !== 'outbound-rtp') return;
        const screen = screenIds.has(sources.get(r.mediaSourceId)?.trackIdentifier);
        into(out, r.kind === 'audio' ? 'audio' : screen ? 'screen' : 'video', r, 'out');
      });
    }
    if (this.sub) {
      const st = await this.sub.getStats();
      st.forEach((r) => {
        pair(r);
        if (r.type !== 'inbound-rtp') return;
        const screen = screenIds.has(r.trackIdentifier);
        into(inn, r.kind === 'audio' ? 'audio' : screen ? 'screen' : 'video', r, 'in');
      });
    }
    return { at: performance.now(), rtt, out, in: inn };
  }

  close() {
    this.pub?.close();
    this.sub?.close();
    this.pub = null;
    this.sub = null;
    this.senders.clear();
    this.remoteStreams.clear();
  }
}
