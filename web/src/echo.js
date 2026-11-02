// The microphone's processing: echo cancellation, noise suppression, gain.
//
// Hearing yourself back in a call means your voice left this machine and
// came back through somebody's loudspeaker and microphone -- theirs, or a
// second client on the same desk. The server never sends a person their
// own audio (the SFU skips the publisher; the socket relay skips the
// sender), so what stops the loop is the microphone processing on every
// device: echo cancellation subtracts what this device is playing from what
// its microphone hears. A browser asked for it with a plain `true` may
// quietly leave it off when the device cannot; this module asks for it as
// a requirement first, falls back to using the device anyway, and reports
// what it got, so the interface can say "use headphones" instead of the
// other side hearing themselves and nobody knowing why.
//
// Pure apart from the injected getUserMedia, so the fallback and the report
// are tested without a device.
export const PROCESSING = { echoCancellation: true, noiseSuppression: true, autoGainControl: true };

const NAMES = { echoCancellation: 'echo cancellation', noiseSuppression: 'noise suppression', autoGainControl: 'automatic gain' };

// The constraints for one microphone. strict asks for each processing
// stage as a requirement (exact), which a device without it refuses.
export function micConstraints(deviceId = '', { strict = false } = {}) {
  const audio = {};
  for (const k of Object.keys(PROCESSING)) audio[k] = strict ? { exact: true } : true;
  if (deviceId) audio.deviceId = { exact: deviceId };
  return { audio };
}

// What a track's settings say about its processing, in words for a person.
export function processingReport(settings) {
  const off = Object.keys(PROCESSING).filter((k) => settings?.[k] === false);
  const echoOff = off.includes('echoCancellation');
  let message = '';
  if (off.length) {
    message = `This microphone has ${off.map((k) => NAMES[k]).join(', ')} off.`;
    if (echoOff) message += ' Others may hear themselves. Use headphones, or pick another microphone.';
  }
  return { ok: !echoOff, off, message };
}

// acquireMic opens the microphone with processing required; a device that
// cannot process is used anyway (a voice with some echo beats no voice) and
// the report says so. Any other failure -- no permission, no device, a
// device id that no longer exists -- is the caller's to handle as before.
export async function acquireMic(getUserMedia, deviceId = '') {
  let stream;
  try {
    stream = await getUserMedia(micConstraints(deviceId, { strict: true }));
  } catch (e) {
    if (e?.name !== 'OverconstrainedError' && e?.name !== 'NotSupportedError' && e?.name !== 'ConstraintNotSatisfiedError') throw e;
    stream = await getUserMedia(micConstraints(deviceId));
  }
  const track = stream.getAudioTracks()[0] ?? null;
  const report = processingReport(track?.getSettings?.() ?? {});
  return { stream, track, report };
}
