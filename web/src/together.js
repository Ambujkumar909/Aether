// Together mode's cut-outs (ARCHITECTURE §6bo).
//
// Teams puts everyone in one scene, cut out of their own rooms. Here each
// sender, while the meeting is in Together mode, sends the outline of the
// person in its own picture alongside the video it already sends: a 64 x 36
// mask, run-length coded -- about a hundred bytes. Receivers cut each video
// out with it and seat it in the scene. Nobody segments anybody else's video,
// and the video itself is unchanged for whoever is not looking at the scene.
//
// A mask is self-contained: losing one costs the next tenth of a second of
// outline, nothing more (rule 5).

export const MASK_W = 64;
export const MASK_H = 36;
export const MAX_MASK_BYTES = 1024; // a worst case checkerboard is ~2.3 KB; a person is ~100 B

// The segmenter's confidence (any size) to a 64 x 36 grid of 0/1: a cell is
// the person when more than half its area is.
export function quantize(conf, w, h) {
  const out = new Uint8Array(MASK_W * MASK_H);
  for (let y = 0; y < MASK_H; y++) {
    const y0 = Math.floor((y * h) / MASK_H), y1 = Math.max(y0 + 1, Math.floor(((y + 1) * h) / MASK_H));
    for (let x = 0; x < MASK_W; x++) {
      const x0 = Math.floor((x * w) / MASK_W), x1 = Math.max(x0 + 1, Math.floor(((x + 1) * w) / MASK_W));
      let sum = 0, n = 0;
      for (let yy = y0; yy < y1; yy++) for (let xx = x0; xx < x1; xx++, n++) sum += conf[yy * w + xx];
      out[y * MASK_W + x] = sum / n > 0.5 ? 1 : 0;
    }
  }
  return out;
}

// Runs of alternating value, starting with background (0), as unsigned
// varints. An empty or full mask is one run.
export function encodeMask(bits) {
  const runs = [];
  let cur = 0, n = 0;
  for (const b of bits) {
    if (b === cur) n++;
    else {
      runs.push(n);
      cur = b;
      n = 1;
    }
  }
  runs.push(n);
  const out = [];
  for (let r of runs) {
    while (r >= 0x80) {
      out.push((r & 0x7f) | 0x80);
      r >>>= 7;
    }
    out.push(r);
  }
  return Uint8Array.from(out);
}

// Null for anything that is not exactly one 64 x 36 mask.
export function decodeMask(bytes) {
  if (!bytes || bytes.length > MAX_MASK_BYTES) return null;
  const bits = new Uint8Array(MASK_W * MASK_H);
  let i = 0, pos = 0, cur = 0;
  while (i < bytes.length) {
    let r = 0, shift = 0, b;
    do {
      if (i >= bytes.length || shift > 21) return null;
      b = bytes[i++];
      r |= (b & 0x7f) << shift;
      shift += 7;
    } while (b & 0x80);
    if (pos + r > bits.length) return null;
    if (cur) bits.fill(1, pos, pos + r);
    pos += r;
    cur ^= 1;
  }
  return pos === bits.length ? bits : null;
}
