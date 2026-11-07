// Signalling that arrives before the media plane has its connections.
//
// The server subscribes a newcomer to everyone already publishing the moment
// they join, and that offer (and its candidates) can land while start() still
// awaits the relay credentials. They were dropped, so on a slow link the
// newcomer never received anybody's camera (tools/e2e/joinrace.e2e.mjs).
// These drive the real MediaPlane with a peer connection that enforces the
// real ordering rules.
import test from 'node:test';
import assert from 'node:assert/strict';
import { Dir } from './wire.js';

class FakePC {
  constructor() {
    this.signalingState = 'stable';
    this.remoteDescription = null;
    this.localDescription = null;
    this.added = [];
    this.history = [];
  }
  addTransceiver() {
    return { sender: { replaceTrack: async () => {}, getParameters: () => ({}), setParameters: async () => {} } };
  }
  getTransceivers() {
    return [];
  }
  async setRemoteDescription(d) {
    this.remoteDescription = d;
    this.signalingState = d.type === 'offer' ? 'have-remote-offer' : 'stable';
    this.history.push(`remote:${d.type}:${d.sdp}`);
  }
  async createAnswer() {
    return { type: 'answer', sdp: 'answer' };
  }
  async createOffer() {
    return { type: 'offer', sdp: 'offer' };
  }
  async setLocalDescription(d) {
    this.localDescription = d;
    this.signalingState = d.type === 'offer' ? 'have-local-offer' : 'stable';
    this.history.push(`local:${d.type}`);
  }
  async addIceCandidate(c) {
    // The real rule: no candidate before a remote description.
    if (!this.remoteDescription) throw new Error('InvalidStateError: remote description is not set');
    this.added.push(c.candidate);
  }
  async getStats() {
    return new Map();
  }
  close() {
    this.signalingState = 'closed';
  }
}
globalThis.RTCPeerConnection = FakePC;
const { MediaPlane, EARLY_ICE_MAX } = await import('./rtc.js');

const settle = async () => {
  for (let i = 0; i < 20; i++) await new Promise((r) => setTimeout(r, 1));
};
const plane = () => {
  const sent = [];
  const m = new MediaPlane({ sendSDP: (dir, isOffer, body) => sent.push({ dir, isOffer, body }), sendICE: () => {} }, {});
  return { m, sent };
};
const ice = (dir, c) => ({ dir, candidate: JSON.stringify({ candidate: c, sdpMid: '0', sdpMLineIndex: 0 }) });

test('an offer that arrives before start() is answered once the connection exists', async () => {
  const { m, sent } = plane();
  await m.handleSDP({ dir: Dir.Subscriber, isOffer: true, body: 'early-offer' });
  await m.handleICE(ice(Dir.Subscriber, 'cand-1'));
  await m.handleICE(ice(Dir.Subscriber, 'cand-2'));
  assert.equal(m.sub, null, 'nothing is created before start');
  await m.start([]);
  await settle();
  assert.ok(m.sub.history.includes('remote:offer:early-offer'), `the early offer was applied: ${m.sub.history}`);
  assert.ok(sent.some((s) => s.dir === Dir.Subscriber && !s.isOffer), 'and answered');
  assert.deepEqual(m.sub.added, ['cand-1', 'cand-2'], 'the early candidates were added after it, in order');
});

test('of two offers before start(), only the latest is applied', async () => {
  const { m } = plane();
  await m.handleSDP({ dir: Dir.Subscriber, isOffer: true, body: 'first' });
  await m.handleSDP({ dir: Dir.Subscriber, isOffer: true, body: 'second' });
  await m.start([]);
  await settle();
  const offers = m.sub.history.filter((h) => h.startsWith('remote:offer'));
  assert.deepEqual(offers, ['remote:offer:second']);
});

test('candidates before start() are held within the bound', async () => {
  const { m } = plane();
  for (let i = 0; i < EARLY_ICE_MAX + 10; i++) await m.handleICE(ice(Dir.Subscriber, `c${i}`));
  await m.handleSDP({ dir: Dir.Subscriber, isOffer: true, body: 'offer' });
  await m.start([]);
  await settle();
  assert.equal(m.sub.added.length, EARLY_ICE_MAX, 'held, but never more than the bound');
});

test('an offer after start() still goes straight through', async () => {
  const { m, sent } = plane();
  await m.start([]);
  await m.handleSDP({ dir: Dir.Subscriber, isOffer: true, body: 'late' });
  await settle();
  assert.ok(m.sub.history.includes('remote:offer:late'));
  assert.ok(sent.some((s) => s.dir === Dir.Subscriber && !s.isOffer));
});
