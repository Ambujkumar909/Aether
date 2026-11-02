// The client's half of the meeting controls.
//
// The bug this exists to prevent was reported like this: "if admin closed
// attendees can turn on video then how my video is on". The server was doing
// its job -- it had stopped forwarding the track the moment the host flipped
// the switch, and nobody in the meeting could see the camera. The client had
// only greyed the button out. The camera stayed on, the local preview kept
// playing, and the uplink kept carrying a stream with no audience. Every
// signal the person had said it was working.
//
// So the property under test is not "the button is disabled". It is "what was
// already running stops", which is a different question and the one that was
// answered wrongly.
//
//   node --test src/permissions.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';

import { revocations, chatBlocked } from './permissions.js';

const ALL = { mic: true, camera: true, share: true, chat: true };
const NONE = { mic: false, camera: false, share: false, chat: false };
const NOTHING = { mic: false, camera: false, share: false };

test('a permissive meeting stops nothing', () => {
  const busy = { mic: true, camera: true, share: true };
  assert.deepEqual(revocations(ALL, busy, false), NOTHING);
});

// The regression. Each of these fails against the old behaviour, where the
// answer was always "stop nothing" and only the button changed.
test('a capability revoked while it is in use must stop', () => {
  assert.deepEqual(revocations({ ...ALL, camera: false }, { camera: true }, false), {
    mic: false,
    camera: true,
    share: false,
  });
  assert.deepEqual(revocations({ ...ALL, mic: false }, { mic: true }, false), {
    mic: true,
    camera: false,
    share: false,
  });
  assert.deepEqual(revocations({ ...ALL, share: false }, { share: true }, false), {
    mic: false,
    camera: false,
    share: true,
  });
});

// Turning sharing off must not silence anybody, which is the client-side twin
// of TestControlsAreIndependent on the server.
test('the controls are independent of each other', () => {
  const busy = { mic: true, camera: true, share: true };
  assert.deepEqual(revocations({ ...ALL, share: false }, busy, false), {
    mic: false,
    camera: false,
    share: true,
  });
  assert.deepEqual(revocations(NONE, busy, false), {
    mic: true,
    camera: true,
    share: true,
  });
});

// A control turned off for something the person was not doing must produce
// nothing to stop. Otherwise every permission change fires a round of toasts
// about capabilities nobody was using.
test('revoking an idle capability is not an event', () => {
  assert.deepEqual(revocations(NONE, { mic: false, camera: false, share: false }, false), NOTHING);
  assert.deepEqual(revocations(NONE, {}, false), NOTHING);
  assert.deepEqual(revocations(NONE, undefined, false), NOTHING);
});

test('the host is exempt from every control, including their own', () => {
  const busy = { mic: true, camera: true, share: true };
  assert.deepEqual(revocations(NONE, busy, true), NOTHING);
});

// A truncated or malformed frame must not be read as permission. Stopping
// media unnecessarily is visible and one click from undone; carrying on
// sending into a server that is dropping it is silent and looks like success.
test('an absent permission counts as refused, not as granted', () => {
  const busy = { mic: true, camera: true, share: true };
  assert.deepEqual(revocations({}, busy, false), { mic: true, camera: true, share: true });
  assert.deepEqual(revocations(undefined, busy, false), {
    mic: true,
    camera: true,
    share: true,
  });
});

// Only exactly true grants, and only exactly true counts as in use. Anything
// truthy-but-not-true has come from somewhere that was not the decoder.
test('only real booleans are honoured', () => {
  assert.deepEqual(revocations({ camera: 'yes' }, { camera: true }, false).camera, true);
  assert.deepEqual(revocations(ALL, { camera: 'yes' }, false).camera, false);
});

test('chat follows the same exemption and the same failure direction', () => {
  assert.equal(chatBlocked(ALL, false), false);
  assert.equal(chatBlocked({ ...ALL, chat: false }, false), true);
  assert.equal(chatBlocked({ ...ALL, chat: false }, true), false, 'the host was blocked from chat');
  assert.equal(chatBlocked({}, false), true);
  assert.equal(chatBlocked(undefined, false), true);
});

// Permission frames arrive repeatedly -- on every change anybody makes, and
// again on rejoin. Asking twice must give the same answer, or the second frame
// re-stops something the person has since been allowed to restart.
test('the rule is a function of its inputs alone', () => {
  const perms = { ...ALL, camera: false };
  const busy = { mic: true, camera: true, share: false };
  const first = revocations(perms, busy, false);
  assert.deepEqual(revocations(perms, busy, false), first);
  // Once the camera has actually stopped, there is nothing left to stop.
  assert.deepEqual(revocations(perms, { ...busy, camera: false }, false), NOTHING);
});
