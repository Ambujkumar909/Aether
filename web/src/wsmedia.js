// Media over the signalling WebSocket.
//
// The guaranteed path. WebRTC carries the call when it can -- UDP direct on the
// fixed port, or through the TURN relay -- and on most networks it can. On the
// one that mattered it could not: a campus network where both browsers offered
// only TCP or relay candidates and ICE sat in "checking" until the participant
// gave up, three tests running, while chat over the very same WebSocket never
// failed once. A WebSocket rides the HTTPS connection to the site. It cannot
// be blocked without blocking the site. Zoom's web client shipped exactly this.
//
// How it works, and why each part is the way it is:
//
//   Encode with WebCodecs, not MediaRecorder. MediaRecorder produces a
//   container stream meant for files, with seconds of latency. WebCodecs hands
//   back individual encoded chunks the moment the hardware encoder finishes
//   them, which is the only thing that is real-time.
//
//   One WebSocket frame per encoded chunk. The app's coalescer batches them
//   with everything else in a 15 ms window, so this rides the existing
//   "no wasted packets" machinery rather than reinventing it.
//
//   Every video keyframe is self-contained and one is forced every 2 s. That
//   is the standing rule against cross-frame state: a late joiner or a dropped
//   frame costs at most two seconds of waiting, never a multi-megabyte repaint.
//
//   Bounded. If the encoder's queue backs up, frames are dropped -- whole
//   frames, never degraded ones -- rather than queued without limit. An
//   unbounded buffer is a memory leak with extra steps.
//
//   Render into a MediaStream, not a bespoke element. Decoded video goes to a
//   canvas and out through canvas.captureStream(); decoded audio goes through
//   Web Audio into a MediaStreamDestination. The result is an ordinary
//   MediaStream, so every existing tile, stage, mirror and audio-sink path
//   renders it without knowing it did not come from WebRTC.
//
// Chrome-only APIs (WebCodecs, MediaStreamTrackProcessor). Feature-detected;
// a browser without them keeps WebRTC and is told why the fallback is absent.

import { createJitterClock } from './jitter.js';

// Periodic keyframes are a safety net, not the recovery path. A receiver
// that needs one -- it just joined, its decoder erred, a delta arrived
// while it was waiting -- asks the publisher for one through the server
// (OpKeyframeReq, deduplicated there), and gets it in one round trip. So the
// cadence can be long: measured at every 2 s, keyframes were 3-9% of all
// video bytes on a camera stream and 10-18 KB each; at every 10 s they are a
// fifth of that, and nothing waits for them.
const KEYFRAME_EVERY = 300; // frames; ~10 s at 30 fps
const VIDEO_BITRATE = 600_000; // bits/s. TCP path: lean, still legible.
// A shared screen is the subject of the meeting and is mostly text. It gets
// several times the camera's budget: on the TCP path bandwidth is not the
// constraint, legibility is, and a blurry document is a useless one. The
// encoder is still bounded -- the queue cap and keyframe cadence apply -- so
// this raises quality, not risk.
const SCREEN_BITRATE = 2_500_000;
const AUDIO_BITRATE = 48_000;
const MAX_ENCODE_QUEUE = 4; // chunks pending in the encoder before we drop

// Socket backpressure. This is the bound on the ONE queue that was unbounded:
// the browser's WebSocket send buffer. Video at 2.5 Mbps on a link that
// carries 1 Mbps grows that buffer without limit; every frame -- and every
// audio chunk and control frame behind it -- then waits seconds to leave the
// machine. That was the 6-7 s screen lag, and it was "my voice is not going":
// the voice was going, seven seconds late, behind a megabyte of pixels.
//
// So: before queuing a video chunk, look at how much is already waiting. Past
// the first line, screen frames are dropped (static content: a dropped frame
// is invisible). Past the second, camera frames too. Audio is never dropped;
// at ~8 KB/s it is not what filled the buffer, and it is the thing a call
// cannot do without. Whole frames, never degraded ones -- and after any drop
// the next frame is forced to a keyframe, so the decoder recovers at once
// rather than showing smears until the next scheduled key.
const PRESSURE_DROP_SCREEN = 96 * 1024; // bytes pending before screen frames drop
const PRESSURE_DROP_CAMERA = 192 * 1024; // bytes pending before camera frames drop too

export function wsMediaSupported() {
  return (
    typeof VideoEncoder === 'function' &&
    typeof VideoDecoder === 'function' &&
    typeof AudioEncoder === 'function' &&
    typeof AudioDecoder === 'function' &&
    typeof MediaStreamTrackProcessor === 'function'
  );
}

// ---------------------------------------------------------------------------
// Sender
// ---------------------------------------------------------------------------

export class WSMediaSender {
  /** @param {(fn: (w: any) => void) => void} send  the app's coalescing send */
  /** @param {(w: any, f: object) => void} encodeFrame  wire encoder */
  /** @param {() => number} pressure  bytes queued on the socket, not yet sent */
  constructor(send, encodeFrame, pressure = () => 0) {
    this.send = send;
    this.encodeFrame = encodeFrame;
    this.pressure = pressure;
    this.droppedForPressure = 0;
    this.video = null;
    this.audio = null;
    this.screen = null;
    this.frameNo = 0;
    this.screenFrameNo = 0;
    this.forceKey = true; // first frame is always a key
    this.forceScreenKey = true;
    this.stopped = false;
    this.dropped = 0;
    this.readers = [];
    this.screenReader = null;
  }

  async start(stream) {
    if (!wsMediaSupported()) throw new Error('WebCodecs unavailable');
    const v = stream.getVideoTracks()[0];
    const a = stream.getAudioTracks()[0];
    if (v) await this._startVideo(v);
    if (a) await this._startAudio(a);
  }

  /** Force the next video frame to be a keyframe (a late joiner arrived). */
  keyframe() {
    this.forceKey = true;
    this.forceScreenKey = true;
  }

  /**
   * The camera or microphone track changed -- turned on after joining, or
   * re-acquired after being off. start() only saw the tracks that existed
   * then; a track added later was never encoded, which is how a microphone
   * turned on mid-call sent nothing over the socket. Restart just that
   * encoder on the new track; the other is untouched.
   */
  async setVideoTrack(track) {
    this._stopReader(this.videoReader);
    this.videoReader = null;
    try {
      this.video?.close();
    } catch {
      /* not configured */
    }
    this.video = null;
    if (track) await this._startVideo(track);
  }

  async setAudioTrack(track) {
    this._stopReader(this.audioReader);
    this.audioReader = null;
    try {
      this.audio?.close();
    } catch {
      /* not configured */
    }
    this.audio = null;
    this.audioTrack = null;
    if (track) await this._startAudio(track);
  }

  _stopReader(reader) {
    if (!reader) return;
    try {
      reader.cancel();
    } catch {
      /* already done */
    }
    this.readers = this.readers.filter((r) => r !== reader);
  }

  /**
   * Start carrying a shared screen as a second video stream, flagged so the
   * receiver draws it to its own surface and the server applies the "share"
   * control. Independent of the camera: starting or stopping one never
   * touches the other, which is what lets a presenter keep their face on
   * screen while they present.
   */
  async startScreen(track) {
    if (this.screen || this.stopped) return;
    const settings = track.getSettings();
    const enc = new VideoEncoder({
      output: (chunk) => this._emit(1, chunk, true),
      error: (e) => console.warn('wsmedia screen encoder:', e.message),
    });
    enc.configure({
      codec: 'vp8',
      width: settings.width || 1280,
      height: settings.height || 720,
      bitrate: SCREEN_BITRATE,
      framerate: settings.frameRate || 30,
      latencyMode: 'realtime',
    });
    this.screen = enc;
    this.forceScreenKey = true;
    this.screenFrameNo = 0;

    const reader = new MediaStreamTrackProcessor({ track }).readable.getReader();
    this.screenReader = reader;
    (async () => {
      for (;;) {
        const { value: frame, done } = await reader.read();
        if (done || this.stopped || this.screen !== enc) {
          frame?.close();
          return;
        }
        if (enc.encodeQueueSize > MAX_ENCODE_QUEUE || this.pressure() > PRESSURE_DROP_SCREEN) {
          this.dropped++;
          this.droppedForPressure++;
          this.forceScreenKey = true;
          frame.close();
          continue;
        }
        const key = this.forceScreenKey || this.screenFrameNo % KEYFRAME_EVERY === 0;
        this.forceScreenKey = false;
        this.screenFrameNo++;
        try {
          enc.encode(frame, { keyFrame: key });
        } catch (e) {
          console.warn('wsmedia screen encode:', e.message);
        }
        frame.close();
      }
    })();
  }

  stopScreen() {
    const enc = this.screen;
    this.screen = null;
    try {
      this.screenReader?.cancel();
    } catch {
      /* already done */
    }
    this.screenReader = null;
    try {
      enc?.close();
    } catch {
      /* not configured */
    }
  }

  async _startVideo(track) {
    const settings = track.getSettings();
    const enc = new VideoEncoder({
      output: (chunk) => this._emit(1, chunk),
      error: (e) => console.warn('wsmedia video encoder:', e.message),
    });
    enc.configure({
      codec: 'vp8',
      width: settings.width || 640,
      height: settings.height || 360,
      bitrate: VIDEO_BITRATE,
      framerate: settings.frameRate || 30,
      latencyMode: 'realtime',
    });
    this.video = enc;

    const reader = new MediaStreamTrackProcessor({ track }).readable.getReader();
    this.readers.push(reader);
    this.videoReader = reader;
    (async () => {
      for (;;) {
        const { value: frame, done } = await reader.read();
        if (done || this.stopped || this.video !== enc) {
          frame?.close();
          return;
        }
        // Backpressure: if the encoder is behind, or the SOCKET is, drop this
        // frame whole and make the next one a keyframe.
        if (enc.encodeQueueSize > MAX_ENCODE_QUEUE || this.pressure() > PRESSURE_DROP_CAMERA) {
          this.dropped++;
          this.droppedForPressure++;
          this.forceKey = true;
          frame.close();
          continue;
        }
        const key = this.forceKey || this.frameNo % KEYFRAME_EVERY === 0;
        this.forceKey = false;
        this.frameNo++;
        try {
          enc.encode(frame, { keyFrame: key });
        } catch (e) {
          console.warn('wsmedia encode:', e.message);
        }
        frame.close();
      }
    })();
  }

  async _startAudio(track) {
    this.audioTrack = track;
    const enc = new AudioEncoder({
      output: (chunk) => this._emit(0, chunk),
      error: (e) => console.warn('wsmedia audio encoder:', e.message),
    });
    this.audio = enc;

    const reader = new MediaStreamTrackProcessor({ track }).readable.getReader();
    this.readers.push(reader);
    this.audioReader = reader;
    (async () => {
      let configured = null;
      for (;;) {
        const { value: data, done } = await reader.read();
        if (done || this.stopped || this.audio !== enc) {
          data?.close();
          return;
        }
        // Configured from the audio that actually arrives, not from what
        // the track's settings claim: a track whose settings said mono but
        // delivered stereo (a Web Audio destination, some virtual devices)
        // made the encoder refuse the first buffer and close for good --
        // silence for the rest of the call, with one warning in the console.
        const shape = `${data.sampleRate}/${data.numberOfChannels}`;
        if (configured !== shape) {
          try {
            enc.configure({ codec: 'opus', sampleRate: data.sampleRate, numberOfChannels: data.numberOfChannels, bitrate: AUDIO_BITRATE });
            configured = shape;
          } catch (e) {
            console.warn('wsmedia audio encoder configure:', e.message);
            data.close();
            continue;
          }
        }
        // Muted is silence, and silence is not sent. A disabled track still
        // delivers frames of zeros; encoding and relaying them cost every
        // other participant a frame per 20 ms per muted person -- in a room
        // of a hundred, of whom ninety are muted, most of the audio traffic
        // was nobody saying nothing. The receiver's clock treats the gap as
        // a pause, not an underrun (jitter.js GAP).
        if (!track.enabled) {
          data.close();
          continue;
        }
        if (enc.encodeQueueSize > MAX_ENCODE_QUEUE) {
          this.dropped++;
          data.close();
          continue;
        }
        try {
          enc.encode(data);
        } catch (e) {
          console.warn('wsmedia audio encode:', e.message);
        }
        data.close();
      }
    })();
  }

  _emit(kind, chunk, screen = false) {
    if (this.stopped) return;
    const payload = new Uint8Array(chunk.byteLength);
    chunk.copyTo(payload);
    const key = kind === 0 || chunk.type === 'key';
    this.send((w) =>
      this.encodeFrame(w, { kind, key, screen, ts: Math.max(0, Math.round(chunk.timestamp)), payload })
    );
  }

  stop() {
    this.stopped = true;
    this.stopScreen();
    for (const r of this.readers) {
      try {
        r.cancel();
      } catch {
        /* already done */
      }
    }
    this.readers = [];
    try {
      this.video?.close();
    } catch {
      /* not configured */
    }
    try {
      this.audio?.close();
    } catch {
      /* not configured */
    }
    this.video = this.audio = null;
  }
}

// ---------------------------------------------------------------------------
// Receiver: one per remote publisher
// ---------------------------------------------------------------------------

export class WSMediaReceiver {
  constructor() {
    this.canvas = document.createElement('canvas');
    this.canvas.width = 640;
    this.canvas.height = 360;
    this.ctx2d = this.canvas.getContext('2d');
    this.videoStream = this.canvas.captureStream(30);

    // One audio context for every receiver on the page. A context per
    // publisher meant a hundred contexts in a hundred-person room, each
    // with its own render thread; browsers cap the count and throttle the
    // rest. Every receiver gets its own destination on the shared context.
    this.actx = sharedAudioContext();
    this.dest = this.actx.createMediaStreamDestination();
    this.dest.channelCount = 1;
    this.dest.channelCountMode = 'explicit';
    // The adaptive receive clock; see jitter.js for the policy and the
    // measurement that replaced the fixed lead.
    this.clock = createJitterClock();
    this._onState = () => {
      // While suspended the audio clock stands still and anything scheduled
      // would stack up ahead of it; start afresh when it runs again.
      if (this.actx.state !== 'running') this.clock.reset();
    };
    this.actx.addEventListener('statechange', this._onState);

    // The stream the rest of the app renders: one video track, one audio.
    this.stream = new MediaStream([
      ...this.videoStream.getVideoTracks(),
      ...this.dest.stream.getAudioTracks(),
    ]);

    this.vdec = null;
    this.adec = null;
    this.awaitingKey = true;
    this.closed = false;
    this.framesIn = 0;

    // The shared screen, when there is one: its own canvas, decoder and
    // stream, created on the first screen frame and torn down on stopScreen.
    this.screenCanvas = null;
    this.screenCtx = null;
    this.screenStream = null;
    this.sdec = null;
    this.awaitingScreenKey = true;
  }

  push({ kind, key, ts, payload, screen }) {
    if (this.closed) return;
    this.framesIn++;
    if (kind === 1 && screen) this._screen(key, ts, payload);
    else if (kind === 1) this._video(key, ts, payload);
    else {
      // Audio frames carry no timestamp (wire.js). The decoder wants a
      // monotonic one; count in 20 ms steps. The jitter clock schedules by
      // arrival, not by this value.
      // Read-then-advance, initialised: the first frame of every stream
      // used to be passed `undefined`, which EncodedAudioChunk refuses, so
      // the first 20 ms of every publisher was thrown away with a console
      // warning (seen on every join in mute.e2e.mjs).
      const ts = this.audioTs ?? 0;
      this.audioTs = ts + 20_000;
      this._audio(ts, payload);
    }
  }

  // Ask the publisher for a keyframe, at most once per 500 ms from this
  // receiver; the server deduplicates across receivers as well. onNeedKey
  // is set by the owner (app.js) with the publisher's id in hand.
  _needKey() {
    const now = performance.now();
    if (now - (this.lastNeedKey ?? -Infinity) < 500) return;
    this.lastNeedKey = now;
    this.onNeedKey?.();
  }

  /** The screen stream, or null if this publisher is not presenting. */
  get screenTrack() {
    return this.screenStream?.getVideoTracks()[0] ?? null;
  }

  _screen(key, ts, payload) {
    if (!this.screenCanvas) {
      this.screenCanvas = document.createElement('canvas');
      this.screenCanvas.width = 1280;
      this.screenCanvas.height = 720;
      this.screenCtx = this.screenCanvas.getContext('2d');
      this.screenStream = this.screenCanvas.captureStream(30);
      this.onScreenStarted?.(this.screenStream);
    }
    if (!this.sdec) {
      this.sdec = new VideoDecoder({
        output: (frame) => {
          if (this.closed || !this.screenCanvas) {
            frame.close();
            return;
          }
          if (this.screenCanvas.width !== frame.displayWidth || this.screenCanvas.height !== frame.displayHeight) {
            this.screenCanvas.width = frame.displayWidth;
            this.screenCanvas.height = frame.displayHeight;
          }
          this.screenCtx.drawImage(frame, 0, 0);
          frame.close();
        },
        error: (e) => {
          console.warn('wsmedia screen decoder:', e.message);
          this.awaitingScreenKey = true;
        },
      });
      this.sdec.configure({ codec: 'vp8' });
    }
    if (this.awaitingScreenKey && !key) return;
    this.awaitingScreenKey = false;
    try {
      this.sdec.decode(new EncodedVideoChunk({ type: key ? 'key' : 'delta', timestamp: ts, data: payload }));
    } catch (e) {
      console.warn('wsmedia screen decode:', e.message);
      this.awaitingScreenKey = true;
    }
  }

  /** The publisher stopped presenting: tear the screen surface down. */
  stopScreen() {
    try {
      this.sdec?.close();
    } catch {
      /* not configured */
    }
    this.sdec = null;
    this.awaitingScreenKey = true;
    for (const t of this.screenStream?.getTracks() ?? []) t.stop();
    this.screenStream = null;
    this.screenCanvas = null;
    this.screenCtx = null;
  }

  _video(key, ts, payload) {
    if (!this.vdec) {
      this.vdec = new VideoDecoder({
        output: (frame) => {
          if (this.closed) {
            frame.close();
            return;
          }
          // Size the canvas to the first real frame, once; captureStream
          // follows canvas size, so the tile gets the true aspect.
          if (this.canvas.width !== frame.displayWidth || this.canvas.height !== frame.displayHeight) {
            this.canvas.width = frame.displayWidth;
            this.canvas.height = frame.displayHeight;
          }
          this.ctx2d.drawImage(frame, 0, 0);
          frame.close();
        },
        error: (e) => {
          console.warn('wsmedia video decoder:', e.message);
          // A decoder error is recoverable with the next key: ask for it.
          this.awaitingKey = true;
          this._needKey();
        },
      });
      this.vdec.configure({ codec: 'vp8' });
    }
    // A decoder cannot start on a delta frame. Waiting, ask the publisher
    // for a key (rate-limited); the periodic one is only the fallback.
    if (this.awaitingKey && !key) {
      this._needKey();
      return;
    }
    this.awaitingKey = false;
    try {
      this.vdec.decode(new EncodedVideoChunk({ type: key ? 'key' : 'delta', timestamp: ts, data: payload }));
    } catch (e) {
      console.warn('wsmedia decode:', e.message);
      this.awaitingKey = true;
      this._needKey();
    }
  }

  _audio(ts, payload) {
    if (!this.adec) {
      this.adec = new AudioDecoder({
        output: (data) => this._play(data),
        error: (e) => console.warn('wsmedia audio decoder:', e.message),
      });
      this.adec.configure({ codec: 'opus', sampleRate: 48000, numberOfChannels: 1 });
    }
    try {
      this.adec.decode(new EncodedAudioChunk({ type: 'key', timestamp: ts, data: payload }));
    } catch (e) {
      console.warn('wsmedia audio decode:', e.message);
    }
  }

  // Schedule decoded PCM into Web Audio behind the adaptive jitter clock
  // (jitter.js): a 60 ms lead that grows on underruns and trims a burst one
  // chunk at a time. The fixed 150 ms lead it replaces measured 495 ms of
  // receive-side delay on loopback; see ARCHITECTURE.md.
  _play(data) {
    if (this.closed) {
      data.close();
      return;
    }
    const ch = data.numberOfChannels;
    const buf = this.actx.createBuffer(ch, data.numberOfFrames, data.sampleRate);
    for (let c = 0; c < ch; c++) {
      const plane = new Float32Array(data.numberOfFrames);
      data.copyTo(plane, { planeIndex: c, format: 'f32-planar' });
      buf.copyToChannel(plane, c);
    }
    data.close();

    if (this.actx.state !== 'running') {
      // Nothing can be heard until the context runs; scheduling into a
      // stopped clock only builds the backlog that made the old lead climb.
      this.actx.resume().catch(() => {});
      return;
    }
    const d = this.clock.schedule(this.actx.currentTime, buf.duration);
    if (d.drop) return;
    const src = this.actx.createBufferSource();
    src.buffer = buf;
    src.connect(this.dest);
    src.start(d.at);
  }

  /** The receive clock's state, for the rate readout and the checks. */
  get jitter() {
    return { targetMs: this.clock.target * 1000, underruns: this.clock.underruns, trimmed: this.clock.trimmed };
  }

  close() {
    this.closed = true;
    this.stopScreen();
    try {
      this.vdec?.close();
    } catch {
      /* not configured */
    }
    try {
      this.adec?.close();
    } catch {
      /* not configured */
    }
    for (const t of this.stream.getTracks()) t.stop();
    this.actx.removeEventListener('statechange', this._onState);
    try {
      this.dest.disconnect();
    } catch {
      /* already gone */
    }
  }
}

let sharedCtx = null;
function sharedAudioContext() {
  if (!sharedCtx || sharedCtx.state === 'closed') sharedCtx = new AudioContext({ sampleRate: 48000 });
  return sharedCtx;
}
