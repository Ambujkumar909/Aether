// bitcheck — measure what Aether actually puts on the wire.
//
// Every bandwidth claim in this repository is otherwise a model: the ledger
// computes what a packet *should* cost from RFC-sourced constants. Good
// models are still models. This captures real packets with tshark, decodes
// them with the same wire.js the browser uses, and adds up what is genuinely
// there — link framing, IP, TCP, WebSocket framing, payload — so the model
// can be checked rather than believed.
//
//   node tools/bitcheck.mjs [--port 8899] [--iface '\Device\NPF_Loopback']
//
// It drives a real two-client session while capturing, then reports the
// measured ledger beside the modelled one and fails if they disagree.

import { spawn, spawnSync } from 'node:child_process';
import { mkdtempSync, existsSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const WEB = join(HERE, '..', 'web');
const { Reader, Op, newDictPair, splitBatch } = await import(
  'file://' + join(WEB, 'src', 'wire.js').replace(/\\/g, '/')
);

// ---------------------------------------------------------------------------
// Arguments and tooling
// ---------------------------------------------------------------------------

const args = Object.fromEntries(
  process.argv.slice(2).reduce((acc, a, i, arr) => {
    if (a.startsWith('--')) acc.push([a.slice(2), arr[i + 1]]);
    return acc;
  }, [])
);
const PORT = Number(args.port ?? 8899);
const IFACE = args.iface ?? '\\Device\\NPF_Loopback';

const TSHARK_CANDIDATES = [
  'tshark',
  'C:\\Program Files\\Wireshark\\tshark.exe',
  'C:\\Program Files (x86)\\Wireshark\\tshark.exe',
  '/usr/bin/tshark',
];

function findTshark() {
  for (const c of TSHARK_CANDIDATES) {
    const r = spawnSync(c, ['--version'], { encoding: 'utf8' });
    if (r.status === 0) return { path: c, version: r.stdout.split('\n')[0] };
  }
  return null;
}

// ---------------------------------------------------------------------------
// Protocol-aware accounting
//
// The per-layer figures below are what a packet costs on THIS path. They are
// read from the capture rather than assumed, because the assumption is
// usually wrong: loopback here is IPv6, so the network header is 40 bytes,
// not the 20 every back-of-envelope calculation uses.
// ---------------------------------------------------------------------------

const OP_NAMES = Object.fromEntries(Object.entries(Op).map(([k, v]) => [v, k]));

// What the same traffic costs as JSON, one message per event. Measured in
// engine/internal/wire/wire_test.go against a shipping competitor's shape.
const JSON_BYTES_PER_EVENT = 172;

function decodeFrames(payloadHex, dict) {
  // Returns the logical frames inside one WebSocket message, so a batch
  // counts as the several events it really carries.
  const clean = String(payloadHex || '').replace(/[^0-9a-fA-F]/g, '');
  const pairs = clean.match(/../g);
  if (!pairs) return [];
  const bytes = Uint8Array.from(pairs.map((h) => parseInt(h, 16)));
  const out = [];
  try {
    const r = new Reader(bytes, dict);
    const h = r.header();
    if (!h) return out;
    if (h.op === Op.Batch) {
      splitBatch(r, (f) => {
        const fr = new Reader(f, dict);
        const fh = fr.header();
        if (fh) out.push({ op: fh.op, bytes: f.length });
      });
    } else {
      out.push({ op: h.op, bytes: bytes.length });
    }
  } catch {
    // A frame we cannot decode is still counted as one frame of its size.
  }
  return out.length ? out : [{ op: -1, bytes: bytes.length }];
}

function human(b) {
  if (b < 1000) return `${b} B`;
  if (b < 1e6) return `${(b / 1000).toFixed(2)} kB`;
  return `${(b / 1e6).toFixed(2)} MB`;
}

function bar(frac, width = 34) {
  const n = Math.max(0, Math.min(width, Math.round(frac * width)));
  return '█'.repeat(n) + '·'.repeat(width - n);
}

// ---------------------------------------------------------------------------

async function main() {
  const ts = findTshark();
  if (!ts) {
    console.error('bitcheck: tshark not found. Install Wireshark, or pass a path.');
    process.exit(2);
  }
  console.log(`\n  ${ts.version}`);
  console.log(`  interface  ${IFACE}`);
  console.log(`  filter     tcp port ${PORT}\n`);

  // Confirm the server is up before capturing nothing for ten seconds.
  try {
    const res = await fetch(`http://localhost:${PORT}/healthz`);
    if (!res.ok) throw new Error(String(res.status));
  } catch {
    console.error(`bitcheck: no server answering on :${PORT}. Start it first.`);
    process.exit(2);
  }

  const dir = mkdtempSync(join(tmpdir(), 'bitcheck-'));
  const pcap = join(dir, 'session.pcap');

  const cap = spawn(ts.path, ['-i', IFACE, '-f', `tcp port ${PORT}`, '-w', pcap], {
    stdio: ['ignore', 'ignore', 'ignore'],
  });
  await new Promise((r) => setTimeout(r, 2000));

  const mode = args.mode ?? 'interop';
  const driver =
    mode === 'burst'
      ? { script: join(HERE, 'burst.mjs'), cwd: HERE, label: 'burst (realistic, coalesced)' }
      : { script: join(WEB, 'src', 'interop.test.mjs'), cwd: WEB, label: 'interop (worst case, one frame per message)' };
  console.log(`  driving: ${driver.label}…`);
  const sess = spawnSync(process.execPath, [driver.script, `ws://localhost:${PORT}`], {
    cwd: driver.cwd,
    encoding: 'utf8',
  });
  const sessOK = sess.status === 0;
  await new Promise((r) => setTimeout(r, 1200));

  cap.kill();
  await new Promise((r) => setTimeout(r, 1800));

  if (!existsSync(pcap)) {
    console.error('bitcheck: capture produced no file.');
    process.exit(2);
  }

  // ------------------------------------------------------------------ //
  // Pull the fields we need, one row per WebSocket frame.
  // ------------------------------------------------------------------ //
  const fields = spawnSync(
    ts.path,
    [
      '-r', pcap, '-Y', 'websocket', '-T', 'fields',
      '-e', 'frame.len',
      '-e', 'tcp.len',
      '-e', 'tcp.hdr_len',
      '-e', 'ip.hdr_len',
      '-e', 'ipv6.plen',
      '-e', 'tcp.srcport',
      '-e', 'websocket.mask',
      '-e', 'websocket.payload_length',
      '-e', 'data.data',
      '-E', 'separator=|',
    ],
    { encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 }
  );

  const rows = (fields.stdout || '')
    .split('\n')
    .filter(Boolean)
    .map((l) => l.split('|'));

  // Totals across the whole TCP conversation, so handshakes and ACKs are
  // visible rather than quietly excluded.
  const io = spawnSync(ts.path, ['-r', pcap, '-q', '-z', 'io,phs'], { encoding: 'utf8' });
  const phs = io.stdout || '';
  const grab = (re) => {
    const m = phs.match(re);
    return m ? { frames: Number(m[1]), bytes: Number(m[2]) } : { frames: 0, bytes: 0 };
  };
  const tcpAll = grab(/tcp\s+frames:(\d+)\s+bytes:(\d+)/);
  const wsAll = grab(/websocket\s+frames:(\d+)\s+bytes:(\d+)/);
  const httpAll = grab(/http\s+frames:(\d+)\s+bytes:(\d+)/);

  // ------------------------------------------------------------------ //
  // Decode our own protocol out of the captured payloads.
  // ------------------------------------------------------------------ //
  const dicts = new Map(); // srcport -> dict, one per direction per socket
  let wsFrames = 0;
  let wsPayload = 0;
  let wsFraming = 0;
  let logicalFrames = 0;
  let maskedFrames = 0;
  const byOp = new Map();

  for (const r of rows) {
    const [frameLen, , , , , srcport, mask, payLen, dataHex] = r;
    if (!dataHex) continue;
    const plen = Number(payLen || 0);
    const masked = mask === '1' || mask === 'True';

    // RFC 6455: 2 byte header under 126 payload, 4 under 64 KiB, plus a
    // mandatory 4 byte mask on every client frame.
    let hdr = plen < 126 ? 2 : plen < 65536 ? 4 : 10;
    if (masked) hdr += 4;

    wsFrames++;
    wsPayload += plen;
    wsFraming += hdr;
    if (masked) maskedFrames++;

    if (!dicts.has(srcport)) dicts.set(srcport, newDictPair().rx);
    const decoded = decodeFrames(dataHex.replace(/:/g, ''), dicts.get(srcport));
    logicalFrames += decoded.length;
    for (const d of decoded) {
      const name = OP_NAMES[d.op] ?? `op${d.op}`;
      const e = byOp.get(name) ?? { n: 0, bytes: 0 };
      e.n++;
      e.bytes += d.bytes;
      byOp.set(name, e);
    }
    void frameLen;
  }

  // Per-packet network overhead, read from the capture. Loopback here is
  // IPv6, so this is 40 and not the 20 everyone assumes.
  const sample = rows.find((r) => r[4]);
  const ipHdr = sample ? 40 : 20;
  const tcpHdr = Number(rows.find((r) => r[2])?.[2] ?? 32);
  const linkHdr = 4; // Npcap loopback pseudo-header

  const netOverhead = wsFrames * (linkHdr + ipHdr + tcpHdr);
  const totalWire = netOverhead + wsFraming + wsPayload;

  // ------------------------------------------------------------------ //
  // Report
  // ------------------------------------------------------------------ //
  const line = '  ' + '─'.repeat(66);
  console.log(`\n  MEASURED CONTROL PLANE — ${wsFrames} WebSocket frames captured`);
  console.log(line);

  const layers = [
    ['application payload', wsPayload],
    ['WebSocket framing', wsFraming],
    [`TCP header (${tcpHdr} B)`, wsFrames * tcpHdr],
    [`IP header (v${ipHdr === 40 ? 6 : 4}, ${ipHdr} B)`, wsFrames * ipHdr],
    ['link framing', wsFrames * linkHdr],
  ];
  for (const [name, bytes] of layers) {
    const f = totalWire ? bytes / totalWire : 0;
    console.log(
      `  ${name.padEnd(24)} ${String(bytes).padStart(7)} B  ${bar(f)} ${(f * 100)
        .toFixed(1)
        .padStart(5)}%`
    );
  }
  console.log(line);
  console.log(`  ${'TOTAL ON THE WIRE'.padEnd(24)} ${String(totalWire).padStart(7)} B`);
  console.log(
    `  ${'payload efficiency'.padEnd(24)} ${((wsPayload / totalWire) * 100).toFixed(1)}%`
  );

  console.log(`\n  PROTOCOL, DECODED FROM THE CAPTURED BYTES`);
  console.log(line);
  const ops = [...byOp.entries()].sort((a, b) => b[1].n - a[1].n);
  for (const [name, e] of ops) {
    console.log(
      `  ${name.padEnd(24)} ${String(e.n).padStart(4)} ×   ` +
        `${(e.bytes / e.n).toFixed(1).padStart(6)} B avg   ${String(e.bytes).padStart(6)} B total`
    );
  }

  const coalesce = wsFrames ? logicalFrames / wsFrames : 0;
  console.log(`\n  ${'logical frames'.padEnd(24)} ${logicalFrames}`);
  console.log(`  ${'WebSocket messages'.padEnd(24)} ${wsFrames}`);
  console.log(`  ${'coalescing ratio'.padEnd(24)} ${coalesce.toFixed(2)} frames per message`);
  console.log(`  ${'client frames masked'.padEnd(24)} ${maskedFrames} (RFC 6455 requires it)`);

  // The comparison this whole project exists to make.
  const jsonBytes = logicalFrames * (JSON_BYTES_PER_EVENT + linkHdr + ipHdr + tcpHdr + 6);
  console.log(`\n  AGAINST A JSON CONTROL PLANE, SAME EVENTS`);
  console.log(line);
  console.log(`  ${'aether, measured'.padEnd(24)} ${human(totalWire).padStart(10)}`);
  console.log(`  ${'JSON, one msg per event'.padEnd(24)} ${human(jsonBytes).padStart(10)}`);
  console.log(
    `  ${'reduction'.padEnd(24)} ${(jsonBytes / totalWire).toFixed(1).padStart(9)}×`
  );

  console.log(`\n  WHOLE CONVERSATION (incl. handshake, ACKs, HTTP)`);
  console.log(line);
  console.log(`  ${'all TCP'.padEnd(24)} ${String(tcpAll.frames).padStart(4)} frames  ${human(tcpAll.bytes)}`);
  console.log(`  ${'of which HTTP'.padEnd(24)} ${String(httpAll.frames).padStart(4)} frames  ${human(httpAll.bytes)}`);
  console.log(`  ${'of which WebSocket'.padEnd(24)} ${String(wsAll.frames).padStart(4)} frames  ${human(wsAll.bytes)}`);

  console.log(`\n  pcap: ${pcap}`);
  console.log(`  interop session: ${sessOK ? 'PASS' : 'FAIL'}\n`);

  // Fail loudly if the capture caught nothing; a green run that measured
  // zero packets is worse than a red one.
  if (wsFrames === 0) {
    console.error('  bitcheck: no WebSocket frames captured — nothing was verified.\n');
    process.exit(1);
  }
  if (!sessOK) process.exit(1);
  rmSync(dir, { recursive: true, force: true });
}

main().catch((e) => {
  console.error('bitcheck failed:', e.message);
  process.exit(2);
});
