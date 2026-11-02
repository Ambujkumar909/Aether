// Which transport carries media. Pure: a function of the URL query alone, so
// it can be tested without a browser, and so the answer is inspectable.
//
// The default is WebRTC, with the socket path armed as the automatic
// fallback (app.js, WS_MEDIA_FALLBACK_MS). It was the socket for a while,
// after WebRTC failed to form a pair on three networks; those failures were
// then traced and fixed one by one (fixed media port, no STUN dependence,
// candidates restricted to the default-route adapter, IPv6 filtered -- see
// ARCHITECTURE.md), and the socket path was measured against it on a
// speech sample under Wi-Fi-like stalls (tools/impair, audio.e2e.mjs
// --speech): TCP turns every stall into a gap in the voice, because bytes
// behind a lost segment wait for the retransmit; RTP over UDP loses one
// packet and conceals it. The numbers are in ARCHITECTURE.md. Audio is what
// a meeting cannot do without, so the transport built for it is the default
// and the reliable one is the fallback, not the other way round.
//
//   ?media=ws  or  ?wsmedia=1   -> 'ws' (socket from the first frame; no ICE)
//   ?media=webrtc or ?webrtc=1  -> 'webrtc' (explicitly)
//   anything else               -> 'webrtc'
export function decideMediaMode(search) {
  const q = new URLSearchParams(search ?? '');
  if (q.get('media') === 'ws' || q.get('wsmedia') === '1') return 'ws';
  return 'webrtc';
}

// chooseTransport is decideMediaMode with the browser's abilities applied:
// the socket path needs WebCodecs and MediaStreamTrackProcessor, which
// Chromium has (Chrome, Edge, Android Chrome and WebView, Windows WebView2)
// and Safari, iOS and Firefox do not. A browser without them gets WebRTC,
// which every browser has, whatever the link asked for.
export function chooseTransport(search, wsSupported) {
  const asked = decideMediaMode(search);
  if (asked === 'ws' && !wsSupported) return 'webrtc';
  return asked;
}
