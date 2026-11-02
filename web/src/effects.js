// Video effects: Teams' "Background effects" (None, Blur, Portrait blur,
// images, "Add new") and "Filters" ("Soft focus", "Adjust brightness"), plus
// "Mirror my video", which only ever changes your own view.
//
// Everything happens on this device. The camera's frames are segmented by
// MediaPipe's selfie segmenter (vendored under /vendor/mediapipe, Apache-2.0)
// and composited on a canvas whose stream is what gets published -- the room
// never receives the unprocessed picture. An image you add is kept in this
// browser and never uploaded: Teams uploads custom backgrounds to its cloud.
//
// Two rules shape the pipeline:
//   - A background you asked to hide is never shown. Until the segmenter has
//     loaded, or if it cannot load, the whole frame is blurred rather than
//     sent as it is.
//   - No effect costs nothing: with "None" and no filters there is no
//     pipeline at all, and the camera's own track is published.

export const BACKGROUNDS = ['none', 'blur', 'portrait', 'image'];

// Built-in backgrounds: plain fields in the app's own neutrals, drawn, not
// shipped as photos (no clipart; docs/ARCHITECTURE visual system).
export const PRESETS = [
  { id: 'slate', label: 'Slate', color: '#2b3440' },
  { id: 'teal', label: 'Deep teal', color: '#0f3d3a' },
  { id: 'stone', label: 'Stone', color: '#8a857c' },
  { id: 'paper', label: 'Paper', color: '#e9e6df' },
];

export const MAX_IMAGES = 12; // your own, kept in this browser
export const MAX_IMAGE_BYTES = 8 << 20; // per image, as picked

export function defaultSettings() {
  return { background: 'none', image: '', softFocus: false, brightness: false };
}

// Settings from storage, never trusted: anything unknown falls back.
export function normalize(s) {
  const d = defaultSettings();
  if (!s || typeof s !== 'object') return d;
  return {
    background: BACKGROUNDS.includes(s.background) ? s.background : d.background,
    image: typeof s.image === 'string' ? s.image.slice(0, 64) : '',
    softFocus: s.softFocus === true,
    brightness: s.brightness === true,
  };
}

export const needsPipeline = (s) => s.background !== 'none' || s.softFocus || s.brightness;
export const needsSegmenter = (s) => s.background !== 'none';

// Blur radius in pixels for a frame of this height: Teams' "Standard blur"
// and the heavier "Portrait blur", the same look at any resolution.
export function blurRadius(mode, height) {
  const at720 = mode === 'portrait' ? 26 : 13;
  return Math.max(2, Math.round((at720 * height) / 720));
}

// The segmenter's confidence (0..1, person) to an alpha byte, with a soft
// edge: below LO is background, above HI is person, smooth between. A hard
// threshold makes hair shimmer frame to frame.
export const EDGE_LO = 0.35;
export const EDGE_HI = 0.75;
export function edgeAlpha(c) {
  if (c <= EDGE_LO) return 0;
  if (c >= EDGE_HI) return 255;
  const t = (c - EDGE_LO) / (EDGE_HI - EDGE_LO);
  return Math.round(255 * t * t * (3 - 2 * t));
}

// Temporal smoothing of the mask, in place: the new frame weighs `k`. This is
// state across frames on this device only -- nothing on the wire depends on
// it, so losing it costs one frame of a slightly harder edge.
export function smoothMask(prev, cur, k = 0.65) {
  if (!prev || prev.length !== cur.length) return Float32Array.from(cur);
  for (let i = 0; i < cur.length; i++) prev[i] = prev[i] + k * (cur[i] - prev[i]);
  return prev;
}

// "Adjust brightness": a gain for a dim picture, from its mean luma (0..1).
// Nothing for a picture that is already bright enough; at most 1.8x.
export function brightnessGain(meanLuma) {
  if (!(meanLuma > 0)) return 1.8;
  return Math.min(1.8, Math.max(1, 0.46 / meanLuma));
}

// Mean luma of RGBA bytes, sampling every `step`th pixel.
export function meanLuma(rgba, step = 4) {
  let sum = 0, n = 0;
  for (let i = 0; i < rgba.length; i += 4 * step) {
    sum += 0.2126 * rgba[i] + 0.7152 * rgba[i + 1] + 0.0722 * rgba[i + 2];
    n++;
  }
  return n ? sum / n / 255 : 0;
}

// The canvas filter for the person (or the whole frame without a segmenter).
export function personFilter(s, gain) {
  const f = [];
  if (s.brightness && gain > 1.001) f.push(`brightness(${gain.toFixed(2)})`, 'contrast(1.04)');
  return f.length ? f.join(' ') : 'none';
}

// ---------------------------------------------------------------------------
// Settings and your own images, kept in this browser.
// ---------------------------------------------------------------------------

const KEY = 'aether.effects';
export function loadSettings() {
  try {
    return normalize(JSON.parse(localStorage.getItem(KEY) || 'null'));
  } catch {
    return defaultSettings();
  }
}
export function saveSettings(s) {
  try {
    localStorage.setItem(KEY, JSON.stringify(normalize(s)));
  } catch {}
}

// Images are scaled down to 1920 wide before they are kept, so a 40 MB photo
// does not become 40 MB of storage, and stored as JPEG in IndexedDB.
const DB = 'aether-effects', STORE = 'images';
function idb() {
  return new Promise((resolve, reject) => {
    const r = indexedDB.open(DB, 1);
    r.onupgradeneeded = () => r.result.createObjectStore(STORE, { keyPath: 'id' });
    r.onsuccess = () => resolve(r.result);
    r.onerror = () => reject(r.error);
  });
}
async function tx(mode, fn) {
  const db = await idb();
  return new Promise((resolve, reject) => {
    const t = db.transaction(STORE, mode);
    const out = fn(t.objectStore(STORE));
    t.oncomplete = () => resolve(out?.result ?? out);
    t.onerror = () => reject(t.error);
  });
}
export async function listImages() {
  try {
    const all = await tx('readonly', (s) => s.getAll());
    return (all ?? []).sort((a, b) => a.at - b.at);
  } catch {
    return [];
  }
}
export async function addImage(file) {
  if (!file || !/^image\/(png|jpeg|webp|gif|bmp)$/.test(file.type)) throw new Error('Choose a PNG, JPEG or WebP image.');
  if (file.size > MAX_IMAGE_BYTES) throw new Error('That image is over 8 MB.');
  const have = await listImages();
  if (have.length >= MAX_IMAGES) throw new Error(`You can keep up to ${MAX_IMAGES} images. Remove one first.`);
  const bmp = await createImageBitmap(file);
  const scale = Math.min(1, 1920 / bmp.width);
  const c = document.createElement('canvas');
  c.width = Math.round(bmp.width * scale);
  c.height = Math.round(bmp.height * scale);
  c.getContext('2d').drawImage(bmp, 0, 0, c.width, c.height);
  bmp.close?.();
  const blob = await new Promise((r) => c.toBlob(r, 'image/jpeg', 0.9));
  const rec = { id: 'img-' + Date.now().toString(36) + Math.random().toString(36).slice(2, 6), blob, at: Date.now() };
  await tx('readwrite', (s) => s.put(rec));
  return rec;
}
export async function removeImage(id) {
  try {
    await tx('readwrite', (s) => s.delete(id));
  } catch {}
}

// ---------------------------------------------------------------------------
// The pipeline.
// ---------------------------------------------------------------------------

const VENDOR = '/vendor/mediapipe';
// Past this many milliseconds a segmentation (with its read-back), the mask is
// made every other frame. ponytail: main-thread pipeline, about 9 ms a frame
// with a GPU (§6bn); segmentation is the cost, so a worker running it is the
// upgrade if this binds. (A WebGL compositor was measured: no gain.)
export const SEG_BUDGET_MS = 12;
let segmenterPromise = null;

// One segmenter for the page, loaded on first use: 4 MB of WebAssembly
// (gzipped) that someone who never blurs never downloads.
export function loadSegmenter() {
  if (!segmenterPromise) {
    segmenterPromise = (async () => {
      const { FilesetResolver, ImageSegmenter } = await import(`${VENDOR}/vision_bundle.mjs`);
      const files = await FilesetResolver.forVisionTasks(VENDOR);
      // Files resolved by name, so the SIMD build is used (every current
      // browser has WebAssembly SIMD; the others are not shipped).
      files.wasmLoaderPath = `${VENDOR}/vision_wasm_internal.js`;
      files.wasmBinaryPath = `${VENDOR}/vision_wasm_internal.wasm`;
      const opts = (delegate) => ({
        baseOptions: { modelAssetPath: `${VENDOR}/selfie_segmenter.tflite`, delegate },
        runningMode: 'VIDEO',
        outputConfidenceMasks: true,
        outputCategoryMask: false,
      });
      try {
        return await ImageSegmenter.createFromOptions(files, opts('GPU'));
      } catch {
        return await ImageSegmenter.createFromOptions(files, opts('CPU'));
      }
    })();
    segmenterPromise.catch(() => {
      segmenterPromise = null; // a later attempt may succeed
    });
  }
  return segmenterPromise;
}

export class EffectsPipeline {
  // raw: the camera's track, which this does not own (the caller stops it).
  // onStatus('loading' | 'ready' | 'failed') reports the segmenter.
  constructor(raw, settings, { onStatus = () => {}, images = new Map() } = {}) {
    this.raw = raw;
    this.s = normalize(settings);
    this.onStatus = onStatus;
    this.images = images; // id -> ImageBitmap
    this.video = document.createElement('video');
    this.video.muted = true;
    this.video.playsInline = true;
    this.video.srcObject = new MediaStream([raw]);
    // Kept in the page (off screen): a detached video may not decode frames.
    Object.assign(this.video.style, { position: 'fixed', left: '-10000px', top: '0', width: '2px', height: '2px', opacity: '0', pointerEvents: 'none' });
    document.body.appendChild(this.video);
    this.out = document.createElement('canvas');
    this.ctx = this.out.getContext('2d');
    this.person = document.createElement('canvas');
    this.pctx = this.person.getContext('2d');
    this.maskCanvas = document.createElement('canvas');
    this.mctx = this.maskCanvas.getContext('2d');
    this.mask = null;
    this.gain = 1;
    this.frames = 0;
    this.segmenter = null;
    this.status = 'none';
    this.stopped = false;
    this.stats = { frames: 0, ms: 0, segMs: 0, segs: 0 }; // time spent per frame, for the record
    this.segTick = 0;
    this.segEvery = 1;
    this.segAvg = 0;
    const stream = this.out.captureStream(0);
    this.track = stream.getVideoTracks()[0];
    // The browser's own stop, kept: the caller may wrap track.stop to stop
    // this pipeline too, and calling the wrapped one from here would recurse.
    this.stopTrack = this.track.stop.bind(this.track);
    this.bg = document.createElement('canvas');
    this.bgctx = this.bg.getContext('2d');
    try {
      this.track.contentHint = 'motion';
    } catch {}
    this.video.play().catch(() => {});
    this.ensureSegmenter();
    this.loop();
  }

  set(settings) {
    this.s = normalize(settings);
    this.ensureSegmenter();
  }

  ensureSegmenter() {
    if (!needsSegmenter(this.s) || this.segmenter || this.status === 'loading' || this.status === 'failed') return;
    this.status = 'loading';
    this.onStatus('loading');
    loadSegmenter().then(
      (seg) => {
        if (this.stopped) return;
        this.segmenter = seg;
        this.status = 'ready';
        this.onStatus('ready');
      },
      (err) => {
        console.warn('background effects unavailable:', err?.message ?? err);
        this.status = 'failed';
        this.onStatus('failed');
      }
    );
  }

  loop() {
    if (this.stopped) return;
    const v = this.video;
    if (v.requestVideoFrameCallback) v.requestVideoFrameCallback(() => this.frame());
    else setTimeout(() => this.frame(), 33);
  }

  frame() {
    if (this.stopped) return;
    const t0 = performance.now();
    try {
      this.draw();
    } catch (e) {
      console.warn('effects frame:', e?.message ?? e);
    }
    this.stats.frames++;
    this.stats.ms += performance.now() - t0;
    // The cost per frame on this device, for the record (ARCHITECTURE), and
    // the rate actually achieved: once a second.
    const now = performance.now();
    this.markAt ??= now;
    this.markFrames ??= 0;
    this.markFrames++;
    if (now - this.markAt >= 1000) {
      document.documentElement.dataset.fxFps = ((this.markFrames * 1000) / (now - this.markAt)).toFixed(1);
      this.markAt = now;
      this.markFrames = 0;
      document.documentElement.dataset.fxMs = (this.stats.ms / this.stats.frames).toFixed(1);
      document.documentElement.dataset.fxSegMs = (this.stats.segMs / Math.max(1, this.stats.segs)).toFixed(1);
      document.documentElement.dataset.fxSegEvery = String(this.segEvery);
    }
    this.track.requestFrame?.();
    this.loop();
  }

  draw() {
    const v = this.video, W = v.videoWidth, H = v.videoHeight;
    if (!W || !H) return;
    if (this.out.width !== W || this.out.height !== H) {
      this.out.width = this.person.width = W;
      this.out.height = this.person.height = H;
    }
    const s = this.s, ctx = this.ctx;

    // Low-light gain, measured twice a second from a small sample.
    if (s.brightness && this.frames++ % 15 === 0) {
      const probe = this.probe ?? (this.probe = Object.assign(document.createElement('canvas'), { width: 64, height: 36 }));
      const pc = probe.getContext('2d', { willReadFrequently: true });
      pc.drawImage(v, 0, 0, 64, 36);
      this.gain = brightnessGain(meanLuma(pc.getImageData(0, 0, 64, 36).data, 1));
    }
    const fx = personFilter(s, this.gain);

    if (!needsSegmenter(s)) {
      ctx.filter = fx;
      ctx.drawImage(v, 0, 0, W, H);
      ctx.filter = 'none';
      if (s.softFocus) this.soften(ctx, v, W, H);
      return;
    }

    // Asked to hide the background and cannot yet tell it from the person:
    // hide all of it.
    if (!this.segmenter) {
      this.blurInto(ctx, v, W, H, blurRadius('portrait', H));
      return;
    }

    // Segment this frame, or, on a machine where segmenting costs more than
    // about a third of a 30 fps frame, every other frame -- the smoothed mask
    // carries the edge between, the picture itself stays at full rate.
    if (this.segTick++ % this.segEvery === 0 || !this.mask) {
      const ts = performance.now();
      const res = this.segmenter.segmentForVideo(v, ts);
      const m = res.confidenceMasks?.[0];
      if (m) {
        const mw = m.width, mh = m.height;
        this.mask = smoothMask(this.mask, m.getAsFloat32Array()); // includes the GPU read-back
        if (this.maskCanvas.width !== mw || this.maskCanvas.height !== mh) {
          this.maskCanvas.width = mw;
          this.maskCanvas.height = mh;
          this.maskData = this.mctx.createImageData(mw, mh);
        }
        const d = this.maskData.data;
        for (let i = 0, j = 3; i < this.mask.length; i++, j += 4) d[j] = edgeAlpha(this.mask[i]);
        this.mctx.putImageData(this.maskData, 0, 0);
      }
      res.close?.();
      const dt = performance.now() - ts;
      this.stats.segMs += dt;
      this.stats.segs++;
      this.segAvg = this.segAvg ? this.segAvg * 0.9 + dt * 0.1 : dt;
      this.segEvery = this.segAvg > SEG_BUDGET_MS ? 2 : 1;
    }

    // The person: the mask, scaled up smoothly (a soft edge), then the frame
    // kept only where the mask is.
    const p = this.pctx;
    p.globalCompositeOperation = 'copy';
    p.imageSmoothingEnabled = true;
    p.drawImage(this.maskCanvas, 0, 0, W, H);
    p.globalCompositeOperation = 'source-in';
    p.filter = fx;
    p.drawImage(v, 0, 0, W, H);
    p.filter = 'none';
    p.globalCompositeOperation = 'source-over';

    // The background.
    if (s.background === 'image') this.drawImageBackground(ctx, W, H);
    else this.blurInto(ctx, v, W, H, blurRadius(s.background, H));
    ctx.drawImage(this.person, 0, 0);
    if (s.softFocus) this.soften(ctx, this.person, W, H);
  }

  // A blurred copy of the frame, made at a quarter of the size and scaled up:
  // a blur has no fine detail left to lose, so it looks the same, and it is a
  // sixteenth of the pixels to blur -- the largest single cost per frame.
  blurInto(ctx, v, W, H, r) {
    const w = Math.max(16, Math.round(W / 4)), h = Math.max(9, Math.round(H / 4));
    if (this.bg.width !== w || this.bg.height !== h) {
      this.bg.width = w;
      this.bg.height = h;
    }
    const b = this.bgctx;
    b.filter = `blur(${Math.max(1, r / 4)}px)`;
    b.drawImage(v, 0, 0, w, h);
    b.filter = 'none';
    ctx.imageSmoothingEnabled = true;
    ctx.imageSmoothingQuality = 'high';
    ctx.drawImage(this.bg, 0, 0, W, H);
  }

  // "Soft focus": a light blur laid over the sharp picture, so skin smooths
  // and edges stay.
  soften(ctx, src, W, H) {
    ctx.globalAlpha = 0.35;
    ctx.filter = `blur(${Math.max(1, Math.round(H / 360))}px)`;
    ctx.drawImage(src, 0, 0, W, H);
    ctx.filter = 'none';
    ctx.globalAlpha = 1;
  }

  drawImageBackground(ctx, W, H) {
    const preset = PRESETS.find((p) => p.id === this.s.image);
    const img = this.images.get(this.s.image);
    if (img) {
      // Cover: fill the frame, crop the overflow.
      const k = Math.max(W / img.width, H / img.height);
      const w = img.width * k, h = img.height * k;
      ctx.drawImage(img, (W - w) / 2, (H - h) / 2, w, h);
    } else {
      ctx.fillStyle = preset?.color ?? PRESETS[0].color;
      ctx.fillRect(0, 0, W, H);
    }
  }

  stop() {
    if (this.stopped) return;
    this.stopped = true;
    this.stopTrack();
    this.video.srcObject = null;
    this.video.remove();
  }
}
