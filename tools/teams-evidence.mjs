// Read the shipped Microsoft Teams desktop package and pull out the three
// layers of evidence docs/TEAMS_PARITY.md cites. Nothing is executed; the
// package is only read, decompressed in memory and pattern-matched.
//
//   node tools/teams-evidence.mjs extract <package dir> <out dir>
//     -> modules.txt   feature chunk names, locale copies folded together
//        strings.json  {namespace: [en-us strings]}
//        settings.txt  "name = default" for every compiled config setting
//   node tools/teams-evidence.mjs strings <out dir> <namespace|*> <regex> [limit]
//   node tools/teams-evidence.mjs settings <out dir> <regex>
//   node tools/teams-evidence.mjs grep <package dir> <regex> [context chars]  (every code chunk)
import fs from 'node:fs';
import path from 'node:path';
import zlib from 'node:zlib';

const [cmd, a, b, c, d] = process.argv.slice(2);
const assets = (pkg) => path.join(pkg, 'desktop-assets', 'hashed-assets');
const gunzip = (f) => zlib.gunzipSync(fs.readFileSync(f)).toString('utf8');

// A webpack locale module: namespace:"x",locale:"en-us",translations:{k:"..",...}
// The object is walked by hand because values contain braces.
function translations(src) {
  const out = {};
  const re = /namespace:"([^"]+)",locale:"en-us",translations:\{/g;
  for (let m; (m = re.exec(src)); ) {
    let i = re.lastIndex, depth = 1;
    while (depth && i < src.length) {
      const ch = src[i++];
      if (ch === '"') { while (i < src.length && src[i] !== '"') i += src[i] === '\\' ? 2 : 1; i++; }
      else if (ch === '{') depth++;
      else if (ch === '}') depth--;
    }
    const body = src.slice(re.lastIndex, i - 1);
    (out[m[1]] ??= []).push(...[...body.matchAll(/[\w$]+:"((?:[^"\\]|\\.)*)"/g)].map((x) => x[1].replace(/\\(.)/g, '$1')));
  }
  return out;
}

if (cmd === 'extract') {
  const dir = assets(a);
  fs.mkdirSync(b, { recursive: true });
  const manifest = JSON.parse(fs.readFileSync(path.join(a, 'desktop-assets', 'asset-manifest.json'), 'utf8'));
  const modules = new Set();
  for (const p of manifest.assets.flatMap((x) => [x.networkPaths].flat())) {
    const n = p.split('/').pop()
      .replace(/-[0-9a-f]{16}(\.worker)?\.(js|css|json|wasm|onnx)$/, '')
      .replace(/[-_.](([a-z]{2,3}(-[a-z0-9]{2,8}){1,3})|[a-z]{2})$/i, '')
      .replace(/[-_~](strings|localizedstrings|locale)$/, '');
    if (!/^\d+$/.test(n)) modules.add(n);
  }
  fs.writeFileSync(path.join(b, 'modules.txt'), [...modules].sort().join('\n'));
  const strings = {};
  for (const f of fs.readdirSync(dir).filter((f) => f.includes('en-us') && f.endsWith('.js.gz')))
    for (const [ns, v] of Object.entries(translations(gunzip(path.join(dir, f))))) (strings[ns] ??= []).push(...v);
  fs.writeFileSync(path.join(b, 'strings.json'), JSON.stringify(strings));
  const prod = fs.readdirSync(dir).find((f) => f.startsWith('config-prod-'));
  const settings = new Set();
  for (const m of gunzip(path.join(dir, prod)).matchAll(/\b([a-zA-Z]\w{3,}):\[\{value:(.{1,90}?)\}(,\{|\])/g)) settings.add(`${m[1]} = ${m[2]}`);
  fs.writeFileSync(path.join(b, 'settings.txt'), [...settings].sort().join('\n'));
  const n = Object.values(strings).reduce((s, v) => s + v.length, 0);
  console.log(`chunks ${manifest.assets.length}, feature modules ${modules.size}, strings ${n} in ${Object.keys(strings).length} namespaces, settings ${settings.size}`);
} else if (cmd === 'strings') {
  const all = JSON.parse(fs.readFileSync(path.join(a, 'strings.json'), 'utf8'));
  const re = new RegExp(c, 'i'), seen = new Set();
  for (const [ns, v] of Object.entries(all))
    if (b === '*' || b === ns) for (const s of v) if (re.test(s) && !seen.has(s) && seen.size < +(d ?? 40)) { seen.add(s); console.log(`[${ns}] ${s}`); }
} else if (cmd === 'settings') {
  const re = new RegExp(b);
  for (const l of fs.readFileSync(path.join(a, 'settings.txt'), 'utf8').split('\n')) if (re.test(l)) console.log(l);
} else if (cmd === 'grep') {
  // Context is cut around each match here rather than written into the
  // pattern: a leading .{0,N} backtracks at every offset of a minified line
  // and turns a 4 s scan into minutes.
  const dir = assets(a), re = new RegExp(b, 'g'), seen = new Set(), ctx = +(c ?? 0);
  for (const f of fs.readdirSync(dir).filter((f) => f.endsWith('.js.gz') && !f.includes('locale'))) {
    const src = gunzip(path.join(dir, f));
    for (const m of src.matchAll(re)) {
      const s = src.slice(Math.max(0, m.index - ctx), m.index + m[0].length + ctx);
      if (!seen.has(s)) { seen.add(s); console.log(`${f}: ${s.slice(0, 260 + 2 * ctx)}`); }
    }
  }
} else {
  console.error('usage: extract <pkg> <out> | strings <out> <ns|*> <regex> [n] | settings <out> <regex> | grep <pkg> <regex>');
  process.exit(2);
}
