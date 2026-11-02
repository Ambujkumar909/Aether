// Early remote candidates: the server trickles its candidate before its
// answer (or offer) reaches the browser, so the candidate ALWAYS arrives
// with no remote description set. The real MediaPlane must hold it and apply
// it once the description lands; dropping it means a browser that cannot
// receive the server's checks (firewall, NAT) never learns where the server
// is. The fake refuses a candidate with no remote description, exactly as
// Chrome does ("The remote description was null") -- a fake that accepted it
// would pass against the code that dropped it.
//
//   node --test src/icequeue.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { MediaPlane, EARLY_ICE_MAX } from './rtc.js';
import { Dir } from './wire.js';

class Pc {
  constructor(state = 'stable') {
    this.signalingState = state;
    this.remoteDescription = null;
    this.added = [];
    this.refused = 0;
  }
  async setRemoteDescription(d) {
    this.remoteDescription = d;
    this.signalingState = d.type === 'offer' ? 'have-remote-offer' : 'stable';
  }
  async createAnswer() {
    return { type: 'answer', sdp: 'v=0\r\n' };
  }
  async setLocalDescription() {
    this.signalingState = 'stable';
  }
  async addIceCandidate(c) {
    if (!this.remoteDescription) {
      this.refused++;
      throw Object.assign(new Error('The remote description was null'), { name: 'InvalidStateError' });
    }
    this.added.push(c.candidate);
  }
}

const cand = (n) => JSON.stringify({ candidate: `candidate:${n} 1 udp 2130706431 10.0.0.1 8801 typ host`, sdpMid: '0', sdpMLineIndex: 0 });

function plane() {
  const sent = [];
  const p = new MediaPlane({ sendSDP: (...a) => sent.push(a), sendICE() {} }, {});
  p.pub = new Pc('have-local-offer');
  p.sub = new Pc();
  return { p, sent };
}

test('a publisher candidate that arrives before the answer is applied after it', async () => {
  const { p } = plane();
  await p.handleICE({ dir: Dir.Publisher, candidate: cand(1) });
  assert.equal(p.pub.refused, 0, 'not offered to the connection while it would refuse');
  assert.deepEqual(p.pub.added, []);
  await p.handleSDP({ dir: Dir.Publisher, isOffer: false, body: 'v=0\r\n' });
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(p.pub.added.length, 1, 'the held candidate was applied once the answer landed');
  assert.match(p.pub.added[0], /^candidate:1 /);
});

test('a subscriber candidate that arrives before the offer is applied after it, before the answer goes out', async () => {
  const { p, sent } = plane();
  await p.handleICE({ dir: Dir.Subscriber, candidate: cand(7) });
  await p.handleICE({ dir: Dir.Subscriber, candidate: cand(8) });
  await p.handleSDP({ dir: Dir.Subscriber, isOffer: true, body: 'v=0\r\n' });
  await new Promise((r) => setTimeout(r, 0));
  assert.deepEqual(p.sub.added.map((c) => c.split(' ')[0]), ['candidate:7', 'candidate:8'], 'both, in arrival order');
  assert.equal(sent.length, 1, 'and the answer was sent');
  assert.equal(p.pub.added.length, 0, 'nothing leaked to the other connection');
});

test('once the description is set, candidates go straight in; each is applied exactly once', async () => {
  const { p } = plane();
  await p.handleICE({ dir: Dir.Publisher, candidate: cand(1) });
  await p.handleSDP({ dir: Dir.Publisher, isOffer: false, body: 'v=0\r\n' });
  await p.handleICE({ dir: Dir.Publisher, candidate: cand(2) });
  await new Promise((r) => setTimeout(r, 0));
  assert.deepEqual(p.pub.added.map((c) => c.split(' ')[0]), ['candidate:1', 'candidate:2']);
  // A later renegotiation's answer does not re-apply the held ones.
  p.pub.signalingState = 'have-local-offer';
  await p.handleSDP({ dir: Dir.Publisher, isOffer: false, body: 'v=0\r\n' });
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(p.pub.added.length, 2, 'no candidate applied twice');
});

test('the hold is bounded, and junk is ignored', async () => {
  const { p } = plane();
  for (let i = 0; i < EARLY_ICE_MAX + 10; i++) await p.handleICE({ dir: Dir.Publisher, candidate: cand(i) });
  await p.handleICE({ dir: Dir.Publisher, candidate: 'not json' });
  await p.handleSDP({ dir: Dir.Publisher, isOffer: false, body: 'v=0\r\n' });
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(p.pub.added.length, EARLY_ICE_MAX, `held at most ${EARLY_ICE_MAX}`);
});
