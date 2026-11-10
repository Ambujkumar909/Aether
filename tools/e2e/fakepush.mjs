// A fake Web Push service for the harnesses: plain HTTP on the loopback
// (the only place the server accepts an http endpoint), accepting what a
// real push service would (RFC 8030) and keeping every message so a test
// can read it back and decrypt it with the subscriber's own keys.
//
//   node tools/e2e/fakepush.mjs 9470
//
//   POST /push/<name>        a message for subscriber <name> (201); stored
//   GET  /inbox/<name>       JSON: [{headers, body (base64url), at}]
//   DELETE /inbox/<name>     forget them
//   GET  /gone/<name>        make /push/<name> answer 410 from now on
import http from 'node:http';

const port = Number(process.argv[2] || 9470);
const inbox = new Map();
const gone = new Set();

http
  .createServer((req, res) => {
    const u = new URL(req.url, 'http://x');
    const [, kind, name] = u.pathname.split('/');
    if (kind === 'push' && req.method === 'POST') {
      const chunks = [];
      req.on('data', (c) => chunks.push(c));
      req.on('end', () => {
        if (gone.has(name)) {
          res.writeHead(410).end();
          return;
        }
        const body = Buffer.concat(chunks);
        const h = req.headers;
        // What a real push service insists on.
        const okHeaders = h['content-encoding'] === 'aes128gcm' && /^vapid t=.+, k=.+$/.test(h.authorization || '') && /^\d+$/.test(h.ttl || '');
        if (!okHeaders || body.length > 4096) {
          res.writeHead(400).end();
          return;
        }
        const list = inbox.get(name) || [];
        list.push({ headers: { encoding: h['content-encoding'], ttl: h.ttl, urgency: h.urgency || '', authorization: h.authorization }, body: body.toString('base64url'), bytes: body.length, at: Date.now() });
        inbox.set(name, list);
        res.writeHead(201).end();
      });
      return;
    }
    if (kind === 'inbox' && req.method === 'GET') {
      res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify(inbox.get(name) || []));
      return;
    }
    if (kind === 'inbox' && req.method === 'DELETE') {
      inbox.delete(name);
      res.writeHead(204).end();
      return;
    }
    if (kind === 'gone') {
      gone.add(name);
      res.writeHead(204).end();
      return;
    }
    if (u.pathname === '/health') {
      res.writeHead(200).end('ok');
      return;
    }
    res.writeHead(404).end();
  })
  .listen(port, '127.0.0.1', () => console.log(`fake push service on http://127.0.0.1:${port}`));
