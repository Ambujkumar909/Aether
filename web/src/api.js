// Typed access to the server, with CSRF handled once rather than at every
// call site.
//
// The session itself lives in an HttpOnly cookie this file cannot read, which
// is the point: a script that gets injected into this page still cannot
// exfiltrate the session. What it can read is the CSRF cookie, and echoing
// that back in a header is what proves a request came from this origin --
// something a cross-site attacker cannot do, because it can cause the browser
// to SEND our cookies but never to READ them.

const CSRF_COOKIE = 'aether_csrf';
const CSRF_HEADER = 'X-Aether-CSRF';

function csrfToken() {
  const m = document.cookie.match(new RegExp('(?:^|; )' + CSRF_COOKIE + '=([^;]*)'));
  return m ? decodeURIComponent(m[1]) : '';
}

export class ApiError extends Error {
  constructor(status, body) {
    super(body?.error || `request failed (${status})`);
    this.status = status;
    this.code = body?.code || '';
    this.details = body?.details || '';
    // The whole answer, for errors that carry data (a notes conflict carries
    // the item as it now is).
    this.body = body ?? null;
  }
}

// This tab's identity for routing: a message sent from here is pushed to
// the person's other tabs and apps, not echoed back. Per tab (session
// storage), random, and meaningless to anyone else.
export const deviceId = (() => {
  try {
    let d = sessionStorage.getItem('aether.device');
    if (!d) {
      d = crypto.randomUUID();
      sessionStorage.setItem('aether.device', d);
    }
    return d;
  } catch {
    return crypto.randomUUID?.() ?? Math.random().toString(36).slice(2);
  }
})();

async function request(method, path, body) {
  const headers = { 'X-Aether-Device': deviceId };
  const init = { method, headers, credentials: 'same-origin' };

  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  if (method !== 'GET' && method !== 'HEAD') {
    headers[CSRF_HEADER] = csrfToken();
  }

  const res = await fetch(path, init);
  if (res.status === 204) return null;

  const text = await res.text();
  let parsed = null;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = { error: text };
    }
  }
  if (!res.ok) throw new ApiError(res.status, parsed);
  return parsed;
}

export const api = {
  get: (p) => request('GET', p),
  post: (p, b) => request('POST', p, b),
  patch: (p, b) => request('PATCH', p, b),
  put: (p, b) => request('PUT', p, b),
  del: (p) => request('DELETE', p),
};

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

export const session = {
  user: null,
  permissions: new Set(),

  can(p) {
    return this.permissions.has(p);
  },

  adopt(payload) {
    this.user = payload.user;
    this.permissions = new Set(payload.permissions ?? []);
    return this.user;
  },

  clear() {
    this.user = null;
    this.permissions = new Set();
  },
};

export async function login(email, password) {
  const res = await api.post('/api/identity/login', { email, password });
  // An account with two-step sign-in: the password was right, a code is
  // owed. No session yet; the ticket goes back with the code.
  if (res?.secondStep) return { secondStep: res.secondStep, ticket: res.ticket };
  return session.adopt(res);
}

export async function secondStep(ticket, code) {
  const res = await api.post('/api/identity/login/second', { ticket, code });
  return session.adopt(res);
}

export const twoStep = {
  setup: (password) => api.post('/api/identity/two-step/setup', { password }),
  enable: (code) => api.post('/api/identity/two-step/enable', { code }),
  disable: (password) => api.post('/api/identity/two-step/disable', { password }),
  reset: (userId) => api.del(`/api/identity/users/${encodeURIComponent(userId)}/two-step`),
};

export async function logout() {
  try {
    await api.post('/api/identity/logout', {});
  } finally {
    session.clear();
  }
}

export async function signup(email, displayName, password) {
  const res = await api.post('/api/identity/signup', { email, displayName, password });
  // A 202 means the address was already taken, which the server declines to
  // confirm. There is no session in that response.
  if (!res?.user) return { pending: true, verify: !!res?.verify, status: res?.status || '' };
  return session.adopt(res);
}

// completeSignup finishes an email-verified sign-up: the link's token and the
// password chosen at sign-up. It signs the new account in.
export async function completeSignup(token, password) {
  const res = await api.post('/api/identity/signup/verify', { token, password });
  return session.adopt(res);
}

// forgotPassword asks for a reset link. The answer is the same whether or not
// the address has an account.
export async function forgotPassword(email) {
  return api.post('/api/identity/password/forgot', { email });
}

// resetPassword sets a new password with the token from the emailed link.
export async function resetPassword(token, password) {
  return api.post('/api/identity/password/reset', { token, password });
}

// serverConfig reports which options this deployment offers, so the sign-in
// page never renders a "Create an account" link on a server that refuses.
export async function serverConfig() {
  try {
    return await api.get('/api/identity/config');
  } catch {
    return { allowSignup: false, minPasswordLength: 12 };
  }
}

// resume checks for an existing session on load, so a reload does not force a
// sign-in.
export async function resume() {
  try {
    const res = await api.get('/api/identity/me');
    return session.adopt(res);
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

export const users = {
  list: (params = {}) => {
    const q = new URLSearchParams(
      Object.entries(params).filter(([, v]) => v !== '' && v != null)
    );
    return api.get('/api/identity/users?' + q);
  },
  create: (u) => api.post('/api/identity/users', u),
  setRole: (id, role) => api.patch(`/api/identity/users/${encodeURIComponent(id)}/role`, { role }),
  setStatus: (id, status) =>
    api.patch(`/api/identity/users/${encodeURIComponent(id)}/status`, { status }),
  remove: (id) => api.del(`/api/identity/users/${encodeURIComponent(id)}`),
  changePassword: (userId, current, next) =>
    api.post('/api/identity/password', { userId, current, next }),
  sessions: (userId) =>
    api.get('/api/identity/sessions' + (userId ? `?userId=${encodeURIComponent(userId)}` : '')),
  revokeSession: (id, userId) =>
    api.del(
      `/api/identity/sessions/${encodeURIComponent(id)}` +
        (userId ? `?userId=${encodeURIComponent(userId)}` : '')
    ),
};

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

export const audit = {
  list: (params = {}) => {
    const q = new URLSearchParams(
      Object.entries(params).filter(([, v]) => v !== '' && v != null)
    );
    return api.get('/api/audit/events?' + q);
  },
  verify: () => api.get('/api/audit/verify'),
  exportUrl: (params = {}) => {
    const q = new URLSearchParams(
      Object.entries(params).filter(([, v]) => v !== '' && v != null)
    );
    return '/api/audit/export?' + q;
  },
};

export const telemetry = {
  read: () => api.get('/api/telemetry'),
};

// ---------------------------------------------------------------------------
// Scheduling
// ---------------------------------------------------------------------------

const qs = (params) =>
  new URLSearchParams(Object.entries(params).filter(([, v]) => v !== '' && v != null));

export const scheduling = {
  list: (params = {}) => api.get('/api/scheduling/meetings?' + qs(params)),
  get: (id) => api.get(`/api/scheduling/meetings/${encodeURIComponent(id)}`),
  create: (m) => api.post('/api/scheduling/meetings', m),
  update: (id, m) => api.patch(`/api/scheduling/meetings/${encodeURIComponent(id)}`, m),
  cancel: (id) => api.post(`/api/scheduling/meetings/${encodeURIComponent(id)}/cancel`, {}),
  // One occurrence of a series, by its start (Unix ms).
  cancelOccurrence: (id, occurrence) => api.post(`/api/scheduling/meetings/${encodeURIComponent(id)}/cancel`, { occurrence }),
  // The scheduling assistant: when these people are busy, as spans only.
  freeBusy: (emails, from, to) => api.get('/api/scheduling/freebusy?' + qs({ emails: emails.join(','), from, to })),
  history: () => api.get('/api/scheduling/history'),
  remove: (id) => api.del(`/api/scheduling/meetings/${encodeURIComponent(id)}`),
  respond: (id, response) =>
    api.post(`/api/scheduling/meetings/${encodeURIComponent(id)}/respond`, { response }),
  conflicts: (from, to, exclude = '') =>
    api.get('/api/scheduling/conflicts?' + qs({ from, to, exclude })),
};

// Q&A in the meeting in progress (engine scheduling/qna.go).
const qnaPath = (room, rest = '') => `/api/scheduling/rooms/${encodeURIComponent(room)}/qna${rest}`;
export const qna = {
  list: (room) => api.get(qnaPath(room)),
  ask: (room, body, anonymous) => api.post(qnaPath(room), { body, anonymous }),
  like: (room, id) => api.post(qnaPath(room, `/${encodeURIComponent(id)}/like`), {}),
  reply: (room, id, body, anonymous) => api.post(qnaPath(room, `/${encodeURIComponent(id)}/replies`), { body, anonymous }),
  moderate: (room, id, action) => api.post(qnaPath(room, `/${encodeURIComponent(id)}/moderate`), { action }),
  settings: (room, s) => api.put(qnaPath(room, '/settings'), s),
};

// Meeting notes (engine scheduling/notes.go).
const notesPath = (room, rest = '') => `/api/scheduling/rooms/${encodeURIComponent(room)}/notes${rest}`;
export const notes = {
  get: (room) => api.get(notesPath(room)),
  create: (room) => api.post(notesPath(room), {}),
  add: (room, section, body) => api.post(notesPath(room, '/items'), { section, body }),
  edit: (room, id, change) => api.patch(notesPath(room, `/items/${encodeURIComponent(id)}`), change),
  remove: (room, id, version) => api.del(notesPath(room, `/items/${encodeURIComponent(id)}?version=${version}`)),
};

// The meeting transcript (engine scheduling/transcript.go).
export const transcript = {
  get: (room, session = 0) => api.get(`/api/scheduling/rooms/${encodeURIComponent(room)}/transcript${session ? `?session=${session}` : ''}`),
};

// ---------------------------------------------------------------------------
// Messaging
// ---------------------------------------------------------------------------

// The people directory. Available to anyone signed in, because a directory
// only administrators can search is a directory nobody can use.
// Push notifications while Aether is closed (engine modules/notify).
export const push = {
  key: () => api.get('/api/notify/push/key'),
  subscribe: (sub) => api.post('/api/notify/push/subscribe', sub),
  unsubscribe: (endpoint) => api.post('/api/notify/push/unsubscribe', { endpoint }),
};

export const directory = {
  search: (q = '', limit = 20) => api.get('/api/identity/directory?' + qs({ q, limit })),
};

export const messaging = {
  list: () => api.get('/api/messaging/conversations'),
  openDirect: (email) => api.post('/api/messaging/direct', { email }),
  // Inside a meeting the roster gives account ids, never addresses; the
  // server resolves one to the other rather than broadcasting everybody's
  // email to everybody in the room.
  openDirectById: (userId) => api.post('/api/messaging/direct', { userId }),
  // The thread you keep with yourself. No body: the only account it can name
  // is your own.
  openSelf: () => api.post('/api/messaging/self', {}),
  createGroup: (title, members) => api.post('/api/messaging/groups', { title, members }),
  messages: (id, params = {}) =>
    api.get(`/api/messaging/conversations/${encodeURIComponent(id)}/messages?` + qs(params)),
  sendFull: (id, body, replyTo = '', mentions = [], opts = {}) =>
    api.post(`/api/messaging/conversations/${encodeURIComponent(id)}/messages`, {
      body, replyTo, mentions,
      priority: opts.priority || '',
      scheduleAt: opts.scheduleAt || 0,
      meta: opts.meta || '',
    }),
  search: (q, limit = 30) => api.get('/api/messaging/search?' + qs({ q, limit })),
  // The same index, narrowed to one conversation.
  searchIn: (conv, q, limit = 30) => api.get('/api/messaging/search?' + qs({ q, conv, limit })),
  // Channel threads and edit history.
  replies: (id, limit = 200) => api.get(`/api/messaging/messages/${encodeURIComponent(id)}/replies?` + qs({ limit })),
  history: (id) => api.get(`/api/messaging/messages/${encodeURIComponent(id)}/history`),
  // Availability: mine to set; my peers' to read.
  setStatus: (status, note = '', until = null) => api.put('/api/messaging/status', { status, note, until }),
  statuses: (ids) => api.get('/api/messaging/status?' + qs({ ids: ids.join(',') })),
  // Polls: my vote on one, as option indexes (empty withdraws).
  vote: (id, options) => api.post(`/api/messaging/messages/${encodeURIComponent(id)}/vote`, { options }),
  // A channel, gone. Owners only.
  deleteChannel: (id) => api.del(`/api/messaging/channels/${encodeURIComponent(id)}`),
  // Blocking: mine to see and change.
  blocks: () => api.get('/api/messaging/blocks'),
  block: (userId) => api.put(`/api/messaging/blocks/${encodeURIComponent(userId)}`, {}),
  unblock: (userId) => api.del(`/api/messaging/blocks/${encodeURIComponent(userId)}`),
  send: (id, body, replyTo = '') =>
    api.post(`/api/messaging/conversations/${encodeURIComponent(id)}/messages`, {
      body,
      replyTo,
    }),
  react: (messageId, emoji, on) =>
    api.post(`/api/messaging/messages/${encodeURIComponent(messageId)}/react`, { emoji, on }),
  saveMessage: (messageId, on) =>
    api.post(`/api/messaging/messages/${encodeURIComponent(messageId)}/save`, { on }),
  savedMessages: () => api.get('/api/messaging/saved'),
  markRead: (id) =>
    api.post(`/api/messaging/conversations/${encodeURIComponent(id)}/read`, {}),
  calls: (id) => api.get(`/api/messaging/conversations/${encodeURIComponent(id)}/calls`),
  edit: (id, body) => api.patch(`/api/messaging/messages/${encodeURIComponent(id)}`, { body }),
  // Two deletes. remove() is "for everyone": the sender takes it back and
  // every member sees a tombstone. hide() is "for me": it leaves this
  // person's thread and nobody else's, and anybody in the thread may do it.
  remove: (id) => api.del(`/api/messaging/messages/${encodeURIComponent(id)}`),
  hide: (id) => api.post(`/api/messaging/messages/${encodeURIComponent(id)}/hide`, {}),
  // Forward: new messages, from you, in each named conversation, marked as
  // forwarded. All-or-nothing on membership; at most ten at once.
  forward: (id, to) => api.post(`/api/messaging/messages/${encodeURIComponent(id)}/forward`, { to }),
  // Files. One message, any text plus up to ten files, as multipart -- the
  // one request in this file that is not JSON, because a file is not JSON.
  upload: async (convId, files, body = '', replyTo = '', mentions = [], opts = {}) => {
    const fd = new FormData();
    if (body) fd.append('body', body);
    if (replyTo) fd.append('replyTo', replyTo);
    if (mentions.length) fd.append('mentions', mentions.join(','));
    if (opts.priority) fd.append('priority', opts.priority);
    if (opts.scheduleAt) fd.append('scheduleAt', String(opts.scheduleAt));
    if (opts.meta) fd.append('meta', opts.meta);
    for (const f of files) fd.append('file', f, f.name);
    const res = await fetch(`/api/messaging/conversations/${encodeURIComponent(convId)}/files`, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { [CSRF_HEADER]: csrfToken() },
      body: fd,
    });
    const text = await res.text();
    let parsed = null;
    try {
      parsed = text ? JSON.parse(text) : null;
    } catch {
      parsed = { error: text };
    }
    if (!res.ok) throw new ApiError(res.status, parsed);
    return parsed;
  },
  fileURL: (id) => `/api/messaging/files/${encodeURIComponent(id)}`,
  // What you decide about a conversation: pinned, quiet, hidden; unread again;
  // gone for you.
  setPrefs: (id, prefs) => api.patch(`/api/messaging/conversations/${encodeURIComponent(id)}/prefs`, prefs),
  markUnread: (id) => api.post(`/api/messaging/conversations/${encodeURIComponent(id)}/unread`, {}),
  forget: (id) => api.del(`/api/messaging/conversations/${encodeURIComponent(id)}`),
  // The meeting's own conversation, once the room has three or more people.
  meetingChat: (room) => api.get(`/api/messaging/meeting/${encodeURIComponent(room)}`),
  // Channels and groups.
  createChannel: (name, topic, visibility) => api.post('/api/messaging/channels', { name, topic, visibility }),
  // Teams (engine messaging/teams.go).
  teams: () => api.get('/api/messaging/teams'),
  createTeam: (name, description, visibility) => api.post('/api/messaging/teams', { name, description, visibility }),
  browseTeams: (q = '') => api.get('/api/messaging/teams/browse?' + qs({ q })),
  joinTeam: (id) => api.post(`/api/messaging/teams/${encodeURIComponent(id)}/join`, {}),
  joinTeamByCode: (code) => api.post('/api/messaging/teams/join', { code }),
  updateTeam: (id, change) => api.patch(`/api/messaging/teams/${encodeURIComponent(id)}`, change),
  deleteTeam: (id) => api.del(`/api/messaging/teams/${encodeURIComponent(id)}`),
  teamMembers: (id) => api.get(`/api/messaging/teams/${encodeURIComponent(id)}/members`),
  addTeamMembers: (id, emails) => api.post(`/api/messaging/teams/${encodeURIComponent(id)}/members`, { emails }),
  removeTeamMember: (id, userId) => api.del(`/api/messaging/teams/${encodeURIComponent(id)}/members/${encodeURIComponent(userId)}`),
  setTeamRole: (id, userId, role) => api.put(`/api/messaging/teams/${encodeURIComponent(id)}/members/${encodeURIComponent(userId)}/role`, { role }),
  teamCode: (id) => api.post(`/api/messaging/teams/${encodeURIComponent(id)}/code`, {}),
  removeTeamCode: (id) => api.del(`/api/messaging/teams/${encodeURIComponent(id)}/code`),
  createTeamChannel: (id, name, topic, isPrivate) => api.post(`/api/messaging/teams/${encodeURIComponent(id)}/channels`, { name, topic, private: isPrivate }),
  activity: ({ before = '', kind = '', unread = false, limit = 50 } = {}) =>
    api.get('/api/messaging/activity?' + qs({ before, kind, unread: unread ? '1' : '', limit })),
  readActivity: (ids, all = false) => api.post('/api/messaging/activity/read', { ids, all }),
  browseChannels: (q = '', limit = 50) => api.get('/api/messaging/channels?' + qs({ q, limit })),
  joinChannel: (id) => api.post(`/api/messaging/channels/${encodeURIComponent(id)}/join`, {}),
  leave: (id) => api.post(`/api/messaging/conversations/${encodeURIComponent(id)}/leave`, {}),
  members: (id, offset = 0, limit = 100) =>
    api.get(`/api/messaging/conversations/${encodeURIComponent(id)}/members?` + qs({ offset, limit })),
  addMembers: (id, emails, history = { mode: 'all' }) =>
    api.post(`/api/messaging/conversations/${encodeURIComponent(id)}/members`, { emails, history }),
  pin: (id, messageId) => api.post(`/api/messaging/conversations/${encodeURIComponent(id)}/pin`, { messageId }),
  avatarURL: (id) => `/api/messaging/conversations/${encodeURIComponent(id)}/avatar`,
  setAvatar: async (id, file) => {
    const fd = new FormData();
    fd.append('file', file, file.name);
    const res = await fetch(`/api/messaging/conversations/${encodeURIComponent(id)}/avatar`, {
      method: 'PUT',
      credentials: 'same-origin',
      headers: { [CSRF_HEADER]: csrfToken() },
      body: fd,
    });
    if (!res.ok) {
      let parsed = null;
      try {
        parsed = await res.json();
      } catch {}
      throw new ApiError(res.status, parsed);
    }
  },
  removeMember: (id, userId) =>
    api.del(`/api/messaging/conversations/${encodeURIComponent(id)}/members/${encodeURIComponent(userId)}`),
  rename: (id, title, topic) => api.patch(`/api/messaging/conversations/${encodeURIComponent(id)}`, { title, topic }),
  shared: (convId) => api.get(`/api/messaging/conversations/${encodeURIComponent(convId)}/shared`),
};

// Roles, mirrored from the server so the interface can label them. The
// server re-checks every permission regardless; this exists to avoid showing
// a button that is going to fail.
export const ROLES = [
  { id: 'guest', label: 'Guest', hint: 'Can join meetings only' },
  { id: 'member', label: 'Member', hint: 'Can join and host meetings' },
  { id: 'moderator', label: 'Moderator', hint: 'Can record, moderate, and read the audit log' },
  { id: 'admin', label: 'Admin', hint: 'Full control including user management' },
];

// Deployment-wide policy. Administrator only; the server refuses the rest.
export const settings = {
  list: () => api.get('/api/settings'),
  set: (key, value) => api.put(`/api/settings/${encodeURIComponent(key)}`, { value }),
};
