// Opus configuration, applied by rewriting the SDP.
//
// Munging a session description is the standard way to reach settings the
// WebRTC API does not expose, and it is also the standard way to break every
// call at once: the text goes into setLocalDescription unvalidated, and a
// regex that matches one line too many produces a description that either
// fails to parse or silently negotiates something nobody asked for.
//
// So this tests the real exported function against real SDP, and the cases
// that matter are the destructive ones -- parameters clobbered, lines lost,
// codecs touched that should not have been -- rather than the happy path.
//
//   node --test src/opus.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';

import { tuneOpus } from './rtc.js';

// SDP is CRLF-delimited. Building fixtures with \n would test a format the
// browser never produces and would hide anything that mishandles \r.
const sdp = (...lines) => lines.join('\r\n') + '\r\n';

const AUDIO = sdp(
  'v=0',
  'o=- 4611731400430051336 2 IN IP4 127.0.0.1',
  's=-',
  't=0 0',
  'm=audio 9 UDP/TLS/RTP/SAVPF 111 63 9 0 8',
  'c=IN IP4 0.0.0.0',
  'a=rtpmap:111 opus/48000/2',
  'a=fmtp:111 minptime=10;useinbandfec=1',
  'a=rtpmap:63 red/48000/2',
  'a=fmtp:63 111/111',
  'a=rtpmap:9 G722/8000',
  'a=rtpmap:0 PCMU/8000'
);

const fmtpFor = (out, pt) => (out.match(new RegExp(`^a=fmtp:${pt} (.*)$`, 'm')) || [])[1] || '';
const params = (out, pt) => fmtpFor(out, pt).replace(/\r$/, '').split(';').filter(Boolean);

test('the bitrate and FEC actually land on the opus payload type', () => {
  const p = params(tuneOpus(AUDIO), 111);
  assert.ok(p.includes('maxaveragebitrate=64000'), `got: ${p.join(';')}`);
  assert.ok(p.includes('useinbandfec=1'));
  assert.ok(p.includes('maxplaybackrate=48000'));
});

// The case a naive replacement gets wrong: rewriting the whole fmtp line drops
// whatever the browser negotiated that we are not overriding.
test('parameters we do not manage survive', () => {
  const withExtras = AUDIO.replace(
    'a=fmtp:111 minptime=10;useinbandfec=1',
    'a=fmtp:111 minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1;cbr=0'
  );
  const p = params(tuneOpus(withExtras), 111);
  for (const kept of ['minptime=10', 'stereo=1', 'sprop-stereo=1', 'cbr=0']) {
    assert.ok(p.includes(kept), `${kept} was dropped; got: ${p.join(';')}`);
  }
});

// Negotiation happens more than once per call: every camera toggle produces a
// fresh offer, and a renegotiated description may already carry what we added
// last time. Appending blindly would grow the line without bound.
test('applying it twice changes nothing the second time', () => {
  const once = tuneOpus(AUDIO);
  assert.equal(tuneOpus(once), once);
  const p = params(once, 111);
  for (const key of ['maxaveragebitrate', 'useinbandfec', 'maxplaybackrate']) {
    assert.equal(p.filter((x) => x.startsWith(key + '=')).length, 1, `${key} duplicated`);
  }
});

test('an opus line with no fmtp of its own gets one', () => {
  const bare = AUDIO.replace('a=fmtp:111 minptime=10;useinbandfec=1\r\n', '');
  const out = tuneOpus(bare);
  assert.ok(params(out, 111).includes('maxaveragebitrate=64000'));
  // Inserted after its rtpmap, and still CRLF-delimited.
  assert.ok(
    /a=rtpmap:111 opus\/48000\/2\r\na=fmtp:111 [^\r\n]+\r\n/.test(out),
    'the inserted line is misplaced or not CRLF-terminated'
  );
});

// red/48000 and G722 share the media section. Touching them would renegotiate
// codecs nobody asked about.
test('no other codec is touched', () => {
  const out = tuneOpus(AUDIO);
  assert.equal(fmtpFor(out, 63).replace(/\r$/, ''), '111/111', 'the red fmtp was rewritten');
  assert.ok(out.includes('a=rtpmap:9 G722/8000'));
  assert.ok(out.includes('a=rtpmap:0 PCMU/8000'));
});

test('every opus payload type is configured, not just the first', () => {
  const two = AUDIO.replace(
    'a=rtpmap:63 red/48000/2',
    'a=rtpmap:112 opus/48000/2\r\na=rtpmap:63 red/48000/2'
  );
  const out = tuneOpus(two);
  assert.ok(params(out, 111).includes('maxaveragebitrate=64000'));
  assert.ok(params(out, 112).includes('maxaveragebitrate=64000'), 'the second opus pt was skipped');
});

// The structural guarantee: nothing is lost, and the only lines that differ
// are opus fmtp lines.
test('no line is dropped or reordered', () => {
  const before = AUDIO.split('\r\n');
  const after = tuneOpus(AUDIO).split('\r\n');
  assert.equal(after.length, before.length, 'the line count changed');
  for (let i = 0; i < before.length; i++) {
    if (before[i].startsWith('a=fmtp:111 ')) continue;
    assert.equal(after[i], before[i], `line ${i} changed unexpectedly`);
  }
});

test('a description with no audio at all is returned untouched', () => {
  const video = sdp('v=0', 'm=video 9 UDP/TLS/RTP/SAVPF 96', 'a=rtpmap:96 VP8/90000');
  assert.equal(tuneOpus(video), video);
});
