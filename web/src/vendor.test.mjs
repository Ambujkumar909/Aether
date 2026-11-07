// Third-party code served from /vendor never phones home.
//
// MediaPipe's bundle carries a usage logger that POSTs to Google every 60 s
// (vendor/mediapipe/PATCHED.md). The CSP blocks it, but the promise is that
// nothing is even attempted: these checks fail if an upgrade restores it.
// VENDOR_ROOT points the test at another tree (used to prove it can fail).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = process.env.VENDOR_ROOT || join(dirname(fileURLToPath(import.meta.url)), '..', 'vendor');
const files = (dir) => readdirSync(dir).flatMap((n) => {
  const p = join(dir, n);
  return statSync(p).isDirectory() ? files(p) : [p];
});
const code = files(root).filter((p) => /\.(m?js)$/.test(p));

test('no vendored script names a Google telemetry or analytics host', () => {
  const hosts = /(?:odml\.pa|firebaselogging|play\.googleapis|google-analytics|googletagmanager)/;
  for (const p of code) assert.equal(hosts.test(readFileSync(p, 'utf8')), false, p);
});

test("MediaPipe's usage logger schedules no send and sends nothing on flush", () => {
  const src = readFileSync(join(root, 'mediapipe', 'vision_bundle.mjs'), 'utf8');
  assert.equal(src.includes('setInterval(()=>{this.flush()},6e4)'), false, 'the 60 s flush timer is back');
  assert.ok(src.includes('flush(t,e){this.i=[];t?.();return;'), 'flush is no longer inert');
});

test('vendored scripts reach no absolute https endpoint by fetch or XHR', () => {
  // A literal https URL beside a send is how a library reports home.
  const send = /(?:fetch\(|\.send\(\{url:|open\("(?:GET|POST)",)\s*"https:\/\//;
  for (const p of code) assert.equal(send.test(readFileSync(p, 'utf8')), false, p);
});
