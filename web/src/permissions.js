// What the meeting's controls require this client to stop doing.
//
// Split out from the code that does the stopping because the two fail in
// different ways and deserve to be checked differently. Whether a revoked
// permission means "stop the camera now" is a rule, and a rule can be stated
// and tested on its own. Whether stopping it actually releases the device,
// repaints the stage and tells the user is plumbing, and plumbing needs a
// browser to be worth testing at all.
//
// The server has the same split for the same reason: `room.May()` is a pure
// function with its own test file, separate from the router code that acts on
// it. This is the client's half of that shape.
//
// None of this is enforcement. The server refuses the frames regardless, and
// it is the only thing that decides anything. What lives here is the client
// being honest with the person using it: not showing them a preview of a
// camera nobody is receiving, and not charging their uplink for it.

/**
 * Which capabilities in use must stop, given the controls now in force.
 *
 * @param {{mic?: boolean, camera?: boolean, share?: boolean, chat?: boolean}} perms
 *   What attendees are permitted to do.
 * @param {{mic?: boolean, camera?: boolean, share?: boolean}} doing
 *   What this client is currently doing.
 * @param {boolean} isHost Whether this client is hosting.
 * @returns {{mic: boolean, camera: boolean, share: boolean}} true means stop it.
 */
export function revocations(perms, doing, isHost) {
  // The host is exempt, exactly as they are on the server. A host caught by
  // their own switch would have no way to reach the switch again.
  if (isHost) return { mic: false, camera: false, share: false };

  const p = perms || {};
  const d = doing || {};

  // A missing field counts as "not permitted", so a malformed or truncated
  // frame stops media rather than letting it run. That direction is the
  // recoverable one: an unnecessary stop is visible and one click from being
  // undone, while carrying on sending something the server is dropping is
  // silent and looks exactly like everything working.
  return {
    mic: p.mic !== true && d.mic === true,
    camera: p.camera !== true && d.camera === true,
    share: p.share !== true && d.share === true,
  };
}

/**
 * Whether this client should be prevented from composing chat.
 *
 * Unlike media, chat has nothing running to stop -- the question is only
 * whether the composer should accept input. The server refuses the frame
 * either way; without this the message was accepted, the box cleared itself,
 * and nothing ever appeared.
 */
export function chatBlocked(perms, isHost) {
  if (isHost) return false;
  return (perms || {}).chat !== true;
}
