// The meeting's view modes, as Teams names them in its View menu, and the
// arithmetic of laying people out in a gallery.
//
// From the Teams package: Gallery, Speaker, Large gallery ("View up to 49
// videos at once", offered from `minParticipantsInLargeGrid = 10`), Focus on
// content. Gallery's own cap of 9 is Microsoft's published behaviour; the
// package carries no constant for it.

export const VIEWS = ['gallery', 'speaker', 'large', 'focus'];
export const GALLERY_MAX = 9;
export const LARGE_GALLERY_MAX = 49;
export const LARGE_GALLERY_MIN_PEOPLE = 10;

// How many tiles a view shows before the rest fold into "+N more".
export function tileCap(view) {
  return view === 'large' ? LARGE_GALLERY_MAX : GALLERY_MAX;
}

// The column count that makes n tiles of the given aspect ratio as large as
// possible inside a W x H box with `gap` between them. Returns the tile size
// too, so the caller can size tiles exactly instead of letting a grid
// stretch them out of shape.
export function bestGrid(n, W, H, gap = 8, aspect = 16 / 9) {
  if (n <= 0 || W <= 0 || H <= 0) return { cols: 1, rows: 1, w: 0, h: 0 };
  let best = { cols: 1, rows: n, w: 0, h: 0 };
  for (let cols = 1; cols <= n; cols++) {
    const rows = Math.ceil(n / cols);
    const cellW = (W - gap * (cols - 1)) / cols;
    const cellH = (H - gap * (rows - 1)) / rows;
    if (cellW <= 0 || cellH <= 0) continue;
    // The tile is the largest aspect-correct box inside the cell.
    const w = Math.min(cellW, cellH * aspect);
    const h = w / aspect;
    if (w > best.w) best = { cols, rows, w: Math.floor(w), h: Math.floor(h) };
  }
  return best;
}

// Who goes in the gallery, in Teams' order: people with video on first, the
// most recent speakers among them first; then everyone else in join order.
// list entries are {id, self, presentation, rank, hasVideo}. Your own tile is
// left out when others are present: it floats as the self view instead.
export function galleryOrder(list) {
  const others = list.filter((p) => !p.presentation && !p.self);
  const key = (p) => [p.hasVideo ? 0 : 1, p.rank >= 0 ? p.rank : 1e9];
  return others
    .map((p, i) => ({ p, i, k: key(p) }))
    .sort((a, b) => a.k[0] - b.k[0] || a.k[1] - b.k[1] || a.i - b.i)
    .map((x) => x.p);
}
