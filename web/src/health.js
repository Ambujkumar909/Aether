// Call health (Teams: More > "Call health"): Network, Audio, Video, Screen
// Sharing, with Teams' row names -- "Round trip time", "Sent bitrate",
// "Received bitrate", "Received packet loss", "Sent resolution", "Sent frame
// rate", "Received resolution", "Received frame rate".
//
// A sample is cumulative counters at one instant (what getStats reports);
// rates and loss are over the interval between two samples, never since the
// call began -- a call that lost packets ten minutes ago is not lossy now.

const kinds = ['audio', 'video', 'screen'];

function kbps(bytesNow, bytesThen, ms) {
  if (!(ms > 0) || bytesNow == null || bytesThen == null || bytesNow < bytesThen) return null;
  return Math.round(((bytesNow - bytesThen) * 8) / ms); // bits per ms = kbit/s
}

function lossPct(cur, prev) {
  if (!cur || !prev) return null;
  const lost = (cur.lost ?? 0) - (prev.lost ?? 0);
  const got = (cur.recv ?? 0) - (prev.recv ?? 0);
  if (lost < 0 || got < 0 || lost + got === 0) return null;
  return Math.round((lost / (lost + got)) * 1000) / 10;
}

const res = (s) => (s && s.w && s.h ? `${s.w}x${s.h}` : null);
const fps = (s) => (s && s.fps ? Math.round(s.fps) : null);

// sample: { at, rtt, out: {audio, video, screen}, in: {audio, video, screen} }
// where each side is { bytes, w, h, fps, lost, recv } (fields optional).
export function health(prev, cur) {
  const ms = prev && cur ? cur.at - prev.at : 0;
  const side = (dir, k) => ({ cur: cur?.[dir]?.[k], prev: prev?.[dir]?.[k] });
  const out = {};
  let sentAll = 0, recvAll = 0, any = false;
  for (const k of kinds) {
    const o = side('out', k), i = side('in', k);
    const sent = kbps(o.cur?.bytes, o.prev?.bytes, ms);
    const recv = kbps(i.cur?.bytes, i.prev?.bytes, ms);
    if (sent != null) { sentAll += sent; any = true; }
    if (recv != null) { recvAll += recv; any = true; }
    out[k] = {
      sentBitrate: sent,
      receivedBitrate: recv,
      receivedPacketLoss: lossPct(i.cur, i.prev),
      sentResolution: k === 'audio' ? null : res(o.cur),
      sentFrameRate: k === 'audio' ? null : fps(o.cur),
      receivedResolution: k === 'audio' ? null : res(i.cur),
      receivedFrameRate: k === 'audio' ? null : fps(i.cur),
    };
  }
  // Network loss: all received media together.
  const tot = (s) => kinds.reduce((a, k) => ({ lost: a.lost + (s?.in?.[k]?.lost ?? 0), recv: a.recv + (s?.in?.[k]?.recv ?? 0) }), { lost: 0, recv: 0 });
  out.network = {
    roundTripTime: cur?.rtt != null ? Math.round(cur.rtt) : null,
    sentBitrate: any ? sentAll : null,
    receivedBitrate: any ? recvAll : null,
    receivedPacketLoss: prev && cur ? lossPct(tot(cur), tot(prev)) : null,
  };
  return out;
}

// How a value is shown: Teams' units, and an em dash for "not measured".
export function show(field, v) {
  if (v == null) return '—';
  if (/Bitrate$/.test(field)) return v >= 1000 ? `${(v / 1000).toFixed(1)} Mbps` : `${v} Kbps`;
  if (/PacketLoss$/.test(field)) return `${v}%`;
  if (/FrameRate$/.test(field)) return `${v} fps`;
  if (field === 'roundTripTime') return `${v} ms`;
  return String(v);
}
