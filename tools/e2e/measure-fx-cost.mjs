// What the video-effects pipeline costs per frame, measured alone (§6bk/6bn).
//
// One page, in front, running the real EffectsPipeline with Standard blur on a
// drawn head-and-shoulders camera for 12 s. Reported over the last 60 frames:
// the gap between frames (30 fps is 33 ms), the pipeline's time per frame,
// and the mean segmentation. Not measured inside the two-browser e2e: there
// one page is a background tab and Chrome throttles its timers, which made
// the numbers there several times worse than real (§6bn).
//
//   GPU=1 node tools/e2e/measure-fx-cost.mjs https://localhost:8443   # real GPU
//   node tools/e2e/measure-fx-cost.mjs https://localhost:8443         # without
import puppeteer from 'puppeteer-core';
const BASE = process.argv.find((a) => a.startsWith('http'));
const b = await puppeteer.launch({ executablePath: 'C:/Program Files/Google/Chrome/Application/chrome.exe', headless: true, protocolTimeout: 300000, args: ['--ignore-certificate-errors', '--autoplay-policy=no-user-gesture-required', ...(process.env.GPU ? ['--enable-gpu', '--use-angle=d3d11', '--ignore-gpu-blocklist'] : ['--disable-gpu'])] });
const pg = await b.newPage();
pg.on('console', (m) => { if (m.text().startsWith('FX')) console.log(m.text()); });
await pg.goto(BASE + '/');
await pg.evaluate(async (delegate) => {
  const fx = await import('/src/effects.js');
  const cv = document.createElement('canvas'); cv.width = 640; cv.height = 360;
  const g = cv.getContext('2d');
  const draw = () => { g.fillStyle = '#c9d6df'; g.fillRect(0, 0, 640, 360); g.fillStyle = '#e0ac8a'; g.beginPath(); g.ellipse(160 + (Date.now() / 50) % 40, 80, 45, 58, 0, 0, 7); g.fill(); g.fillStyle = '#2a2f3a'; g.fillRect(40, 175, 240, 185); };
  setInterval(draw, 33); draw();
  const track = cv.captureStream(30).getVideoTracks()[0];
  const pipe = new fx.EffectsPipeline(track, { background: 'blur' }, { onStatus: (s) => console.log('FX status ' + s) });
  // Instrument: per-frame gaps and draw times.
  const orig = pipe.draw.bind(pipe);
  let last = performance.now(); const gaps = [], draws = [];
  pipe.draw = () => { const t0 = performance.now(); gaps.push(t0 - last); last = t0; orig(); draws.push(performance.now() - t0); };
  await new Promise((r) => setTimeout(r, 12000));
  const med = (a) => { const s = [...a].sort((x, y) => x - y); return s.length ? s[Math.floor(s.length / 2)].toFixed(1) : '-'; };
  const tail = (a) => a.slice(-60);
  console.log(`FX path ${pipe.gl ? 'gpu' : '2d'} status ${pipe.status} frames ${draws.length}; last 60: gap median ${med(tail(gaps))} ms, draw median ${med(tail(draws))} ms, max ${Math.max(...tail(draws)).toFixed(1)}; seg avg ${(pipe.stats.segMs / Math.max(1, pipe.stats.segs)).toFixed(1)} ms over ${pipe.stats.segs}, every ${pipe.segEvery}`);
  pipe.stop();
});
await b.close();
