// An OpenID Connect identity provider for the harnesses: discovery, a key
// set, an authorize page a browser visits and clicks through, a token
// endpoint that signs RS256 identity tokens, and a control endpoint the
// harness uses to say who signs in next. Plain HTTP on the loopback, which
// the server's OIDC client allows for localhost only.
//
//   node tools/e2e/fakeidp.mjs [port]      # default 9460
//
// Client id "e2e-idp", secret "e2e-idp-secret". Control:
//   POST /_control/person  {"email","name","verified"}   who the next sign-in is
//   GET  /_control/stats                                  token-endpoint calls
import { createServer } from 'node:http';
import { createSign, generateKeyPairSync, randomBytes } from 'node:crypto';

const PORT = Number(process.argv[2] || 9460);
const ISSUER = `http://127.0.0.1:${PORT}`;
const CLIENT_ID = 'e2e-idp';
const SECRET = 'e2e-idp-secret';
const { privateKey, publicKey } = generateKeyPairSync('rsa', { modulusLength: 2048 });
const jwk = publicKey.export({ format: 'jwk' });
const KID = 'e2e-k1';

let person = { email: 'sso.person@example.test', name: 'SSO Person', verified: true };
const codes = new Map(); // code -> { nonce, person }
const stats = { tokenCalls: 0, authorizeCalls: 0 };

const b64 = (b) => Buffer.from(b).toString('base64url');
const sign = (claims) => {
  const h = b64(JSON.stringify({ alg: 'RS256', typ: 'JWT', kid: KID }));
  const p = b64(JSON.stringify(claims));
  const s = createSign('RSA-SHA256').update(`${h}.${p}`).sign(privateKey);
  return `${h}.${p}.${b64(s)}`;
};
const json = (res, status, body) => { res.writeHead(status, { 'content-type': 'application/json' }); res.end(JSON.stringify(body)); };
const readBody = (req) => new Promise((r) => { let d = ''; req.on('data', (c) => (d += c)); req.on('end', () => r(d)); });

createServer(async (req, res) => {
  const u = new URL(req.url, ISSUER);
  if (u.pathname === '/.well-known/openid-configuration') {
    return json(res, 200, { issuer: ISSUER, authorization_endpoint: ISSUER + '/authorize', token_endpoint: ISSUER + '/token', jwks_uri: ISSUER + '/keys' });
  }
  if (u.pathname === '/keys') return json(res, 200, { keys: [{ kty: 'RSA', use: 'sig', alg: 'RS256', kid: KID, n: jwk.n, e: jwk.e }] });
  if (u.pathname === '/authorize') {
    stats.authorizeCalls++;
    const q = u.searchParams;
    if (q.get('client_id') !== CLIENT_ID || q.get('response_type') !== 'code' || q.get('code_challenge_method') !== 'S256') {
      return json(res, 400, { error: 'invalid_request' });
    }
    // The page a person sees at their organisation; the harness clicks it.
    const code = randomBytes(16).toString('base64url');
    codes.set(code, { nonce: q.get('nonce'), person: { ...person }, redirect: q.get('redirect_uri'), state: q.get('state') });
    const back = new URL(q.get('redirect_uri'));
    back.searchParams.set('code', code);
    back.searchParams.set('state', q.get('state'));
    const deny = new URL(q.get('redirect_uri'));
    deny.searchParams.set('error', 'access_denied');
    deny.searchParams.set('state', q.get('state'));
    res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' });
    return res.end(`<!doctype html><title>Example Org sign-in</title><body style="font:16px system-ui;padding:40px">
      <h1>Example Org</h1><p>Continue to Aether as <b id="who">${person.email}</b>?</p>
      <p><a id="continue" href="${back.href}" style="padding:12px 18px;background:#0b7a62;color:#fff;border-radius:8px;text-decoration:none">Continue</a>
      <a id="deny" href="${deny.href}" style="margin-left:16px">Cancel</a></p></body>`);
  }
  if (u.pathname === '/token' && req.method === 'POST') {
    stats.tokenCalls++;
    const body = new URLSearchParams(await readBody(req));
    const auth = Buffer.from((req.headers.authorization || '').replace(/^Basic /, ''), 'base64').toString();
    const entry = codes.get(body.get('code'));
    codes.delete(body.get('code'));
    if (auth !== `${CLIENT_ID}:${SECRET}` || !entry || body.get('grant_type') !== 'authorization_code' || !body.get('code_verifier')) {
      return json(res, 400, { error: 'invalid_grant' });
    }
    const now = Math.floor(Date.now() / 1000);
    const idToken = sign({ iss: ISSUER, sub: 'sub-' + entry.person.email, aud: CLIENT_ID, exp: now + 300, iat: now, nonce: entry.nonce, email: entry.person.email, email_verified: entry.person.verified, name: entry.person.name });
    return json(res, 200, { id_token: idToken, token_type: 'Bearer', access_token: 'unused' });
  }
  if (u.pathname === '/_control/person' && req.method === 'POST') {
    person = { ...person, ...JSON.parse(await readBody(req)) };
    return json(res, 200, person);
  }
  if (u.pathname === '/_control/stats') return json(res, 200, stats);
  json(res, 404, { error: 'not_found' });
}).listen(PORT, '127.0.0.1', () => console.log(`fake idp ${ISSUER}`));
