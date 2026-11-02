// Signalling robustness under rapid toggling.
//
// The reported bug: turn the camera on, off, on quickly a few times and the
// client stops responding to the button entirely, for the rest of the
// meeting. Same for screen sharing.
//
// The cause is that a peer connection has exactly ONE slot for a pending
// local description. Calling createOffer/setLocalDescription while an earlier
// offer is still waiting for its answer throws InvalidStateError and leaves
// the connection stuck in "have-local-offer". Nothing retries, so it never
// recovers.
//
// FakePeerConnection below models that state machine faithfully, including the
// throw. That fidelity is the entire value of this file: a fake that politely
// accepted out-of-order descriptions would pass against the broken code and
// prove nothing.
//
//   node --test src/rtc.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';

// ---------------------------------------------------------------------------
// A peer connection that enforces the real rules
// ---------------------------------------------------------------------------

class InvalidStateError extends Error {
  constructor(msg) {
    super(msg);
    this.name = 'InvalidStateError';
  }
}

class FakePeerConnection {
  constructor() {
    this.signalingState = 'stable';
    this.senders = [];
    this.transceivers = [];
    this.offersCreated = 0;
    this.localDescriptionsSet = 0;
    this.history = [];
  }

  async createOffer() {
    this.offersCreated++;
    return { type: 'offer', sdp: `offer-${this.offersCreated}` };
  }

  async setLocalDescription(desc) {
    // The real rule: an offer may only be set from stable.
    if (desc.type === 'offer' && this.signalingState !== 'stable') {
      throw new InvalidStateError(
        `Failed to set local offer sdp: Called in wrong state: ${this.signalingState}`
      );
    }
    this.localDescriptionsSet++;
    this.signalingState = desc.type === 'offer' ? 'have-local-offer' : 'stable';
    this.history.push(`setLocal:${desc.type}`);
  }

  async setRemoteDescription(desc) {
    if (desc.type === 'answer' && this.signalingState !== 'have-local-offer') {
      throw new InvalidStateError(
        `Failed to set remote answer sdp: Called in wrong state: ${this.signalingState}`
      );
    }
    this.signalingState = 'stable';
    this.history.push(`setRemote:${desc.type}`);
  }

  addTransceiver(track, init) {
    const sender = {
      track,
      replaceTrack: async (t) => {
        sender.track = t;
        this.history.push(`replaceTrack:${t ? t.kind : 'null'}`);
      },
    };
    this.senders.push(sender);
    this.transceivers.push({ sender, init });
    return { sender };
  }

  addIceCandidate() {}
  close() {
    this.signalingState = 'closed';
  }
}

// A minimal stand-in for MediaPlane carrying the same serialisation logic.
// The real class is not imported because it constructs browser objects at
// construction time; what is under test is the ordering discipline, and this
// mirrors it exactly.
class Signaller {
  constructor(pc, { autoAnswer = true, answerDelay = 0 } = {}) {
    this.pub = pc;
    this._chain = Promise.resolve();
    this._renegotiateWhenStable = false;
    this.senders = new Map();
    this.sent = [];
    this.autoAnswer = autoAnswer;
    this.answerDelay = answerDelay;
    this.errors = [];
  }

  _enqueue(fn) {
    const run = this._chain.then(fn, fn).catch((err) => {
      this.errors.push(err);
    });
    this._chain = run;
    return run;
  }

  async _negotiateNow() {
    if (!this.pub || this.pub.signalingState === 'closed') return;
    if (this.pub.signalingState !== 'stable') {
      this._renegotiateWhenStable = true;
      return;
    }
    const offer = await this.pub.createOffer();
    if (this.pub.signalingState !== 'stable') {
      this._renegotiateWhenStable = true;
      return;
    }
    await this.pub.setLocalDescription(offer);
    this.sent.push(offer.sdp);
    if (this.autoAnswer) this._scheduleAnswer(offer.sdp);
  }

  _scheduleAnswer(sdp) {
    setTimeout(() => {
      this.handleAnswer(sdp.replace('offer', 'answer'));
    }, this.answerDelay);
  }

  async handleAnswer(sdp) {
    return this._enqueue(async () => {
      if (!this.pub || this.pub.signalingState === 'closed') return;
      if (this.pub.signalingState !== 'have-local-offer') return;
      await this.pub.setRemoteDescription({ type: 'answer', sdp });
      if (this._renegotiateWhenStable) {
        this._renegotiateWhenStable = false;
        await this._negotiateNow();
      }
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
      const tx = this.pub.addTransceiver(track, { direction: 'sendonly' });
      this.senders.set('video', tx.sender);
      await this._negotiateNow();
    });
  }

  async setSending(kind, on, track = null) {
    return this._enqueue(async () => {
      const s = this.senders.get(kind);
      if (!s) return;
      await s.replaceTrack(on ? track : null);
    });
  }

  async settle() {
    // Drain the chain and any scheduled answers.
    for (let i = 0; i < 50; i++) {
      await this._chain;
      await new Promise((r) => setTimeout(r, this.answerDelay + 1));
      if (!this._renegotiateWhenStable && this.pub.signalingState === 'stable') break;
    }
    await this._chain;
  }
}

const track = (kind) => ({ kind, readyState: 'live' });

// ---------------------------------------------------------------------------
// The reported bug
// ---------------------------------------------------------------------------

test('the fake models the real failure: unserialised offers wedge the connection', async () => {
  // Proof that the fake can fail. Without serialisation, a second offer while
  // the first is outstanding throws and leaves the connection stuck. If this
  // ever stops throwing, every other test in this file is worthless.
  const pc = new FakePeerConnection();
  await pc.setLocalDescription(await pc.createOffer());
  assert.equal(pc.signalingState, 'have-local-offer');

  await assert.rejects(
    async () => pc.setLocalDescription(await pc.createOffer()),
    (err) => err.name === 'InvalidStateError'
  );
  assert.equal(pc.signalingState, 'have-local-offer', 'still wedged, as in the browser');
});

test('rapid camera toggling never wedges signalling', async () => {
  const pc = new FakePeerConnection();
  const sig = new Signaller(pc, { answerDelay: 2 });

  // The exact reported interaction: on, off, on, off... as fast as possible.
  const ops = [];
  for (let i = 0; i < 20; i++) {
    ops.push(i % 2 === 0 ? sig.ensureVideo(track('video')) : sig.setSending('video', false));
  }
  await Promise.all(ops);
  await sig.settle();

  assert.deepEqual(sig.errors, [], 'no signalling operation may fail');
  assert.equal(pc.signalingState, 'stable', 'connection must settle back to stable');
});

test('a toggle during an in-flight offer is applied, not lost', async () => {
  // Answers are slow, so every toggle after the first lands mid-negotiation.
  const pc = new FakePeerConnection();
  const sig = new Signaller(pc, { answerDelay: 20 });

  await sig.ensureVideo(track('video'));   // first offer goes out
  const second = sig.ensureVideo(track('video2')); // arrives while unstable
  await second;
  await sig.settle();

  assert.deepEqual(sig.errors, []);
  assert.equal(pc.signalingState, 'stable');
  assert.equal(sig.senders.get('video').track.kind, 'video2',
    'the last requested track must be the one being sent');
});

test('interleaved screen share and camera toggles converge', async () => {
  const pc = new FakePeerConnection();
  const sig = new Signaller(pc, { answerDelay: 3 });

  const ops = [
    sig.ensureVideo(track('camera')),
    sig.ensureVideo(track('screen')),
    sig.setSending('video', false),
    sig.ensureVideo(track('camera')),
    sig.ensureVideo(track('screen')),
    sig.setSending('video', false),
    sig.ensureVideo(track('camera')),
  ];
  await Promise.all(ops);
  await sig.settle();

  assert.deepEqual(sig.errors, []);
  assert.equal(pc.signalingState, 'stable');
  assert.equal(sig.senders.get('video').track.kind, 'camera',
    'the final state must match the final intent');
});

test('exactly one transceiver is created however many times video is enabled', async () => {
  const pc = new FakePeerConnection();
  const sig = new Signaller(pc, { answerDelay: 1 });

  for (let i = 0; i < 10; i++) {
    await sig.ensureVideo(track('video'));
    await sig.setSending('video', false);
  }
  await sig.settle();

  assert.equal(pc.transceivers.length, 1,
    'adding a transceiver per toggle would renegotiate forever and grow the SDP without bound');
  assert.deepEqual(sig.errors, []);
});

test('a late answer for a superseded offer is ignored, not applied', async () => {
  const pc = new FakePeerConnection();
  const sig = new Signaller(pc, { autoAnswer: false });

  await sig.ensureVideo(track('video'));
  assert.equal(pc.signalingState, 'have-local-offer');

  await sig.handleAnswer('answer-1');
  assert.equal(pc.signalingState, 'stable');

  // A duplicate or late answer must not throw or corrupt state.
  await sig.handleAnswer('answer-1');
  await sig.handleAnswer('answer-stale');

  assert.deepEqual(sig.errors, []);
  assert.equal(pc.signalingState, 'stable');
});

test('operations after close are inert', async () => {
  const pc = new FakePeerConnection();
  const sig = new Signaller(pc, { answerDelay: 1 });
  await sig.ensureVideo(track('video'));
  await sig.settle();

  pc.close();
  await sig.ensureVideo(track('video'));
  await sig.setSending('video', false);
  await sig.handleAnswer('answer-late');

  assert.deepEqual(sig.errors, [], 'a closed connection must be ignored, not throw');
});

test('one failing operation does not break the queue for the next', async () => {
  const pc = new FakePeerConnection();
  const sig = new Signaller(pc, { answerDelay: 1 });

  // Force a failure inside the chain.
  await sig._enqueue(async () => {
    throw new Error('deliberate');
  });
  assert.equal(sig.errors.length, 1);

  // The queue must still work afterwards. A chain that breaks on first error
  // turns a transient fault into the permanent wedge it exists to prevent.
  await sig.ensureVideo(track('video'));
  await sig.settle();

  assert.equal(pc.signalingState, 'stable');
  assert.equal(sig.senders.get('video').track.kind, 'video');
});
