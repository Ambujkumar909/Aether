// The two opcode tables must agree.
//
// This exists because they silently did not. Per-meeting host controls were
// added to the Go table and not to the browser's, so `Op.Permissions` was
// undefined; `w.header(undefined)` produced a header with a NaN opcode, the
// server could not match it to any case, and the frame was dropped without an
// error on either side. The toggle simply did nothing, and there was nothing
// in either log to say why.
//
// That failure mode is the reason to test it rather than to be careful: an
// opcode mismatch is invisible. Nothing throws, nothing warns, the frame goes
// out and lands nowhere. The only way it announces itself is a feature that
// does not work.
//
//   node --test src/opcodes.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import { Op } from './wire.js';

const HERE = dirname(fileURLToPath(import.meta.url));
const GO = join(HERE, '..', '..', 'engine', 'internal', 'wire', 'opcode.go');

// Parse the Go table: lines of the form `OpName Op = 0x10d` or `= 3`.
function goOpcodes() {
  const src = readFileSync(GO, 'utf8');
  const out = new Map();
  for (const m of src.matchAll(/^\s*Op(\w+)\s+Op\s*=\s*(0x[0-9a-fA-F]+|\d+)/gm)) {
    out.set(m[1], Number(m[2]));
  }
  return out;
}

test('the Go table is readable and non-trivial', () => {
  const go = goOpcodes();
  assert.ok(go.size > 20, `parsed only ${go.size} opcodes from opcode.go`);
});

test('every opcode the browser knows has the same value in Go', () => {
  const go = goOpcodes();
  const mismatched = [];
  const absent = [];

  for (const [name, value] of Object.entries(Op)) {
    if (!go.has(name)) {
      absent.push(name);
      continue;
    }
    if (go.get(name) !== value) {
      mismatched.push(`${name}: browser 0x${value.toString(16)}, Go 0x${go.get(name).toString(16)}`);
    }
  }

  assert.deepEqual(mismatched, [], 'opcode values disagree between the two codecs');
  assert.deepEqual(absent, [], 'the browser defines opcodes Go has never heard of');
});

// The direction that actually bit: an opcode added to Go and forgotten in the
// browser. Not every Go opcode needs a browser counterpart -- some are
// server-internal -- so this reports rather than fails, except for the ones
// the browser demonstrably uses.
test('no opcode is used by the client without being defined', () => {
  const app = readFileSync(join(HERE, 'app.js'), 'utf8');
  const used = new Set([...app.matchAll(/\bOp\.([A-Z]\w*)/g)].map((m) => m[1]));

  const undefinedNames = [...used].filter((n) => Op[n] === undefined).sort();
  assert.deepEqual(
    undefinedNames,
    [],
    'app.js uses opcodes wire.js does not define; these encode as NaN and are ' +
      'silently dropped by the server'
  );
});

test('opcode values are unique', () => {
  const seen = new Map();
  const dupes = [];
  for (const [name, value] of Object.entries(Op)) {
    if (seen.has(value)) dupes.push(`${seen.get(value)} and ${name} share 0x${value.toString(16)}`);
    seen.set(value, name);
  }
  assert.deepEqual(dupes, [], 'two opcodes share a value');
});
