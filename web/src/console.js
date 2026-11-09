// The administration console: user management, the audit log, and sessions.
//
// It is a full-screen surface rather than a panel, because these are not
// things you do while half-attending a meeting, and giving them room is what
// makes a fifty-row audit table readable instead of a scrolling slit.

import { session, users, audit, settings, twoStep, api, ROLES, ApiError } from './api.js';

const esc = (s) =>
  String(s ?? '').replace(
    /[&<>"']/g,
    (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]
  );

const initials = (s) =>
  String(s || '?')
    .trim()
    .split(/[\s@.]+/)
    .slice(0, 2)
    .map((w) => w[0])
    .join('')
    .toUpperCase();

function relative(iso) {
  if (!iso) return 'never';
  const then = new Date(iso).getTime();
  const secs = Math.round((Date.now() - then) / 1000);
  if (secs < 45) return 'just now';
  if (secs < 3600) return `${Math.round(secs / 60)} min ago`;
  if (secs < 86400) return `${Math.round(secs / 3600)} h ago`;
  if (secs < 604800) return `${Math.round(secs / 86400)} d ago`;
  return new Date(iso).toLocaleDateString();
}

function toast(msg, kind = 'ok') {
  const el = document.getElementById('toast');
  if (!el) return;
  el.textContent = msg;
  el.className = 'toast show ' + kind;
  clearTimeout(toast._t);
  toast._t = setTimeout(() => (el.className = 'toast'), 4000);
}

function fail(err) {
  toast(err instanceof ApiError ? err.message : 'Something went wrong', 'bad');
}

// ---------------------------------------------------------------------------
// Shell
// ---------------------------------------------------------------------------

const state = {
  tab: 'users',
  userQuery: { q: '', role: '', status: '', limit: 50, offset: 0 },
  auditQuery: { q: '', action: '', outcome: '', limit: 100 },
};

export function openConsole(tab = 'users') {
  document.getElementById('console').classList.remove('hidden');
  document.body.classList.add('console-open');
  selectTab(tab);
}

export function closeConsole() {
  document.getElementById('console').classList.add('hidden');
  document.body.classList.remove('console-open');
}

function selectTab(tab) {
  state.tab = tab;
  document.querySelectorAll('.console-nav button').forEach((b) =>
    b.classList.toggle('active', b.dataset.tab === tab)
  );
  const body = document.getElementById('console-body');
  body.scrollTop = 0;
  if (tab === 'users') renderUsers();
  else if (tab === 'policy') renderPolicy();
  else if (tab === 'audit') renderAudit();
  else if (tab === 'sessions') renderSessions();
  else if (tab === 'account') renderAccount();
}

export function initConsole() {
  document.getElementById('console-close').onclick = closeConsole;
  document.querySelectorAll('.console-nav button').forEach((b) => {
    b.onclick = () => selectTab(b.dataset.tab);
  });
  addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && !document.getElementById('console').classList.contains('hidden')) {
      closeConsole();
    }
  });
  // Hide navigation the current role cannot use. The server refuses these
  // regardless; this is so the interface does not offer a dead end.
  document.querySelector('[data-tab="users"]').hidden = !session.can('user.read');
  document.querySelector('[data-tab="audit"]').hidden = !session.can('audit.read');
}

function section(title, subtitle, controls, content) {
  return `
    <div class="section-head">
      <div>
        <h2>${esc(title)}</h2>
        <p>${esc(subtitle)}</p>
      </div>
      <div class="section-actions">${controls}</div>
    </div>
    ${content}`;
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

async function renderUsers() {
  const body = document.getElementById('console-body');
  const canWrite = session.can('user.write');
  const canDelete = session.can('user.delete');

  body.innerHTML = section(
    'People',
    'Accounts, roles, and access.',
    canWrite ? `<button class="btn btn-primary" id="u-new">Add person</button>` : '',
    `<div class="filters">
       <input id="u-q" class="input" placeholder="Search name or email" value="${esc(state.userQuery.q)}" />
       <select id="u-role" class="input">
         <option value="">All roles</option>
         ${ROLES.map(
           (r) =>
             `<option value="${r.id}" ${state.userQuery.role === r.id ? 'selected' : ''}>${r.label}</option>`
         ).join('')}
       </select>
       <select id="u-status" class="input">
         <option value="">Any status</option>
         <option value="active" ${state.userQuery.status === 'active' ? 'selected' : ''}>Active</option>
         <option value="suspended" ${state.userQuery.status === 'suspended' ? 'selected' : ''}>Suspended</option>
       </select>
     </div>
     <div id="u-table" class="table-wrap"><div class="loading">Loading…</div></div>`
  );

  const reload = debounce(() => {
    state.userQuery.q = document.getElementById('u-q').value.trim();
    state.userQuery.role = document.getElementById('u-role').value;
    state.userQuery.status = document.getElementById('u-status').value;
    loadUsers();
  }, 220);

  document.getElementById('u-q').oninput = reload;
  document.getElementById('u-role').onchange = reload;
  document.getElementById('u-status').onchange = reload;
  if (canWrite) document.getElementById('u-new').onclick = newUserDialog;

  loadUsers();

  async function loadUsers() {
    const box = document.getElementById('u-table');
    try {
      const res = await users.list(state.userQuery);
      const list = res.users ?? [];
      if (!list.length) {
        box.innerHTML = `<div class="empty">No accounts match that filter.</div>`;
        return;
      }
      box.innerHTML = `
        <table class="table">
          <thead><tr>
            <th>Person</th><th>Role</th><th>Status</th><th>Last seen</th><th></th>
          </tr></thead>
          <tbody>${list.map((u) => userRow(u, canWrite, canDelete)).join('')}</tbody>
        </table>
        <div class="table-foot">${list.length} of ${res.total} accounts</div>`;
      wireUserRows();
    } catch (e) {
      fail(e);
      box.innerHTML = `<div class="empty">Could not load accounts.</div>`;
    }
  }

  function wireUserRows() {
    body.querySelectorAll('[data-role-for]').forEach((sel) => {
      sel.onchange = async () => {
        const id = sel.dataset.roleFor;
        try {
          await users.setRole(id, sel.value);
          toast('Role updated. Their sessions were ended.');
          loadUsers();
        } catch (e) {
          fail(e);
          loadUsers();
        }
      };
    });
    body.querySelectorAll('[data-suspend]').forEach((btn) => {
      btn.onclick = async () => {
        const { suspend, status } = btn.dataset;
        const next = status === 'active' ? 'suspended' : 'active';
        try {
          await users.setStatus(suspend, next);
          toast(next === 'suspended' ? 'Account suspended and signed out.' : 'Account reactivated.');
          loadUsers();
        } catch (e) {
          fail(e);
        }
      };
    });
    body.querySelectorAll('[data-twostep-reset]').forEach((btn) => {
      btn.onclick = async () => {
        const ok = await confirmDialog(
          'Remove two-step sign-in?',
          `${btn.dataset.email} will sign in with their password alone until they set it up again, and every session they have ends. For a lost phone with no recovery codes.`,
          'Remove'
        );
        if (!ok) return;
        try {
          await twoStep.reset(btn.dataset.twostepReset);
          toast('Two-step sign-in removed; their sessions were ended.');
        } catch (e) {
          if (e instanceof ApiError && e.code === 'two_step_not_set_up') toast('That account has no two-step sign-in.');
          else fail(e);
        }
      };
    });
    body.querySelectorAll('[data-delete]').forEach((btn) => {
      btn.onclick = async () => {
        const id = btn.dataset.delete;
        const email = btn.dataset.email;
        const ok = await confirmDialog(
          'Delete this account?',
          `${email} will be removed permanently. Their audit history is kept.`,
          'Delete'
        );
        if (!ok) return;
        try {
          await users.remove(id);
          toast('Account deleted.');
          loadUsers();
        } catch (e) {
          fail(e);
        }
      };
    });
  }
}

function userRow(u, canWrite, canDelete) {
  const self = session.user && u.id === session.user.id;
  const roleCell =
    canWrite && !self
      ? `<select class="input compact" data-role-for="${esc(u.id)}">
           ${ROLES.map(
             (r) => `<option value="${r.id}" ${u.role === r.id ? 'selected' : ''}>${r.label}</option>`
           ).join('')}
         </select>`
      : `<span class="tag tag-${esc(u.role)}">${esc(u.role)}</span>${
          self ? '<span class="you">you</span>' : ''
        }`;

  const statusCell = u.locked
    ? `<span class="tag tag-warn">locked</span>`
    : u.status === 'suspended'
      ? `<span class="tag tag-bad">suspended</span>`
      : `<span class="tag tag-ok">active</span>`;

  const actions =
    self || !canWrite
      ? ''
      : `<button class="btn btn-quiet" data-suspend="${esc(u.id)}" data-status="${esc(u.status)}">
           ${u.status === 'active' ? 'Suspend' : 'Reactivate'}
         </button>` +
        `<button class="btn btn-quiet" data-twostep-reset="${esc(u.id)}" data-email="${esc(u.email)}" title="For a lost phone: removes their two-step sign-in and ends their sessions">Remove two-step</button>` +
        (canDelete
          ? `<button class="btn btn-danger-quiet" data-delete="${esc(u.id)}" data-email="${esc(u.email)}">Delete</button>`
          : '');

  return `<tr>
    <td>
      <div class="person-cell">
        <span class="avatar sm">${esc(initials(u.displayName || u.email))}</span>
        <div>
          <div class="p-name">${esc(u.displayName || u.email)}</div>
          <div class="p-sub mono">${esc(u.email)}</div>
        </div>
      </div>
    </td>
    <td>${roleCell}</td>
    <td>${statusCell}</td>
    <td class="mono dim">${esc(relative(u.lastLoginAt))}</td>
    <td class="row-actions">${actions}</td>
  </tr>`;
}

function newUserDialog() {
  openDialog({
    title: 'Add a person',
    body: `
      <label class="field"><span>Email</span>
        <input id="nu-email" class="input" type="email" autocomplete="off" /></label>
      <label class="field"><span>Display name</span>
        <input id="nu-name" class="input" autocomplete="off" /></label>
      <label class="field"><span>Temporary password</span>
        <input id="nu-pass" class="input" type="text" autocomplete="new-password" /></label>
      <label class="field"><span>Role</span>
        <select id="nu-role" class="input">
          ${ROLES.map((r) => `<option value="${r.id}" ${r.id === 'member' ? 'selected' : ''}>${r.label} — ${esc(r.hint)}</option>`).join('')}
        </select></label>
      <p class="hint">At least 12 characters. They should change it after signing in.</p>`,
    confirm: 'Create account',
    onOpen: () => {
      document.getElementById('nu-pass').value = suggestPassword();
      document.getElementById('nu-email').focus();
    },
    onConfirm: async () => {
      try {
        await users.create({
          email: document.getElementById('nu-email').value.trim(),
          displayName: document.getElementById('nu-name').value.trim(),
          password: document.getElementById('nu-pass').value,
          role: document.getElementById('nu-role').value,
        });
        toast('Account created.');
        renderUsers();
        return true;
      } catch (e) {
        fail(e);
        return false;
      }
    },
  });
}

// A suggested passphrase, generated in the browser from the platform CSPRNG.
// Offering one is the difference between a temporary password that is random
// and one that is the administrator's usual choice reused again.
function suggestPassword() {
  const words = [
    'harbour', 'lantern', 'meadow', 'quartz', 'timber', 'violet', 'cobalt',
    'ember', 'thistle', 'marble', 'saffron', 'willow', 'orchid', 'canyon',
  ];
  const n = new Uint32Array(4);
  crypto.getRandomValues(n);
  return (
    [...n].slice(0, 3).map((v) => words[v % words.length]).join('-') +
    '-' +
    (n[3] % 900 + 100)
  );
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

const ACTIONS = [
  ['', 'All activity'],
  ['auth.login.succeeded', 'Sign in'],
  ['auth.login.failed', 'Failed sign in'],
  ['auth.account.locked', 'Account locked'],
  ['auth.logout', 'Sign out'],
  ['auth.password.changed', 'Password changed'],
  ['user.created', 'User created'],
  ['user.role.changed', 'Role changed'],
  ['user.suspended', 'User suspended'],
  ['user.deleted', 'User deleted'],
  ['audit.exported', 'Audit exported'],
];

async function renderAudit() {
  const body = document.getElementById('console-body');
  const canVerify = session.can('audit.verify');
  const canExport = session.can('audit.export');

  body.innerHTML = section(
    'Audit log',
    'Every security-relevant action, chained so that tampering is detectable.',
    `${canVerify ? '<button class="btn" id="a-verify">Verify chain</button>' : ''}
     ${canExport ? '<button class="btn" id="a-export">Export CSV</button>' : ''}`,
    `<div id="a-chain" class="chain-banner"></div>
     <div class="filters">
       <input id="a-q" class="input" placeholder="Search actor, subject, IP" value="${esc(state.auditQuery.q)}" />
       <select id="a-action" class="input">
         ${ACTIONS.map(
           ([v, l]) =>
             `<option value="${v}" ${state.auditQuery.action === v ? 'selected' : ''}>${l}</option>`
         ).join('')}
       </select>
       <select id="a-outcome" class="input">
         <option value="">Any outcome</option>
         <option value="success" ${state.auditQuery.outcome === 'success' ? 'selected' : ''}>Success</option>
         <option value="denied" ${state.auditQuery.outcome === 'denied' ? 'selected' : ''}>Denied</option>
         <option value="failure" ${state.auditQuery.outcome === 'failure' ? 'selected' : ''}>Failure</option>
       </select>
     </div>
     <div id="a-table" class="table-wrap"><div class="loading">Loading…</div></div>`
  );

  const reload = debounce(() => {
    state.auditQuery.q = document.getElementById('a-q').value.trim();
    state.auditQuery.action = document.getElementById('a-action').value;
    state.auditQuery.outcome = document.getElementById('a-outcome').value;
    loadAudit();
  }, 220);

  document.getElementById('a-q').oninput = reload;
  document.getElementById('a-action').onchange = reload;
  document.getElementById('a-outcome').onchange = reload;

  if (canVerify) {
    document.getElementById('a-verify').onclick = async () => {
      const banner = document.getElementById('a-chain');
      banner.className = 'chain-banner checking';
      banner.innerHTML = 'Recomputing every link…';
      try {
        const res = await audit.verify();
        banner.className = 'chain-banner ' + (res.ok ? 'ok' : 'bad');
        banner.innerHTML = res.ok
          ? `<strong>Chain intact.</strong> ${res.checked} entries verified.
             Head <code class="mono">${esc((res.headHash || '').slice(0, 24))}</code>`
          : `<strong>Chain broken at entry ${res.brokenAt}.</strong> ${esc(res.detail)}`;
      } catch (e) {
        fail(e);
        banner.className = 'chain-banner';
        banner.textContent = '';
      }
    };
  }
  if (canExport) {
    document.getElementById('a-export').onclick = () => {
      // The export is itself an audited action, so this navigation appears
      // in the log a moment later.
      window.location.href = audit.exportUrl(state.auditQuery);
      setTimeout(loadAudit, 800);
    };
  }

  loadAudit();

  async function loadAudit() {
    const box = document.getElementById('a-table');
    try {
      const res = await audit.list(state.auditQuery);
      const list = res.events ?? [];
      if (!list.length) {
        box.innerHTML = `<div class="empty">Nothing recorded for that filter.</div>`;
        return;
      }
      box.innerHTML = `
        <table class="table audit">
          <thead><tr>
            <th>#</th><th>When</th><th>What happened</th><th>Outcome</th><th>Source</th><th>Link</th>
          </tr></thead>
          <tbody>${list.map(auditRow).join('')}</tbody>
        </table>
        <div class="table-foot">
          Showing ${list.length} of ${res.total} entries ·
          head <code class="mono">${esc(String(res.headHash || '').slice(0, 16))}</code>
        </div>`;
    } catch (e) {
      fail(e);
      box.innerHTML = `<div class="empty">Could not load the log.</div>`;
    }
  }
}

function auditRow(e) {
  const cls =
    e.outcome === 'success' ? 'tag-ok' : e.outcome === 'denied' ? 'tag-warn' : 'tag-bad';
  return `<tr>
    <td class="mono dim">${e.seq}</td>
    <td class="mono dim" title="${esc(e.at)}">${esc(relative(e.at))}</td>
    <td>
      <div class="a-summary">${esc(e.summary || e.action)}</div>
      ${e.detail ? `<div class="a-detail">${esc(e.detail)}</div>` : ''}
    </td>
    <td><span class="tag ${cls}">${esc(e.outcome)}</span></td>
    <td class="mono dim">${esc(e.ip || '—')}</td>
    <td class="mono dim hash" title="${esc(e.hash)}">${esc(String(e.hash).slice(0, 10))}</td>
  </tr>`;
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

async function renderSessions() {
  const body = document.getElementById('console-body');
  body.innerHTML = section(
    'Your sessions',
    'Everywhere this account is currently signed in.',
    '',
    `<div id="s-table" class="table-wrap"><div class="loading">Loading…</div></div>`
  );

  try {
    const res = await users.sessions();
    const list = (res.sessions ?? []).filter((s) => !s.revokedAt);
    if (!list.length) {
      body.querySelector('#s-table').innerHTML = `<div class="empty">No active sessions.</div>`;
      return;
    }
    body.querySelector('#s-table').innerHTML = `
      <table class="table">
        <thead><tr><th>Started</th><th>Last seen</th><th>Address</th><th>Device</th><th></th></tr></thead>
        <tbody>${list
          .map(
            (s) => `<tr>
              <td class="mono dim">${esc(relative(s.createdAt))}</td>
              <td class="mono dim">${esc(relative(s.lastSeen))}</td>
              <td class="mono">${esc(s.ip || '—')}</td>
              <td class="dim ua">${esc(s.userAgent || 'unknown')}</td>
              <td class="row-actions">
                <button class="btn btn-quiet" data-revoke="${esc(s.id)}">Sign out</button>
              </td>
            </tr>`
          )
          .join('')}</tbody>
      </table>`;

    body.querySelectorAll('[data-revoke]').forEach((b) => {
      b.onclick = async () => {
        try {
          await users.revokeSession(b.dataset.revoke);
          toast('Session ended.');
          renderSessions();
        } catch (e) {
          fail(e);
        }
      };
    });
  } catch (e) {
    fail(e);
  }
}

// ---------------------------------------------------------------------------
// Account
// ---------------------------------------------------------------------------

async function renderAccount() {
  const u = session.user;
  const body = document.getElementById('console-body');
  // Two-step sign-in state comes from the server, not from memory: what is
  // shown is what is stored.
  let ts = { available: false, enabled: false, recoveryCodesLeft: 0 };
  try {
    ts = (await api.get('/api/identity/me')).twoStep ?? ts;
  } catch {}
  body.innerHTML = section(
    'Your account',
    'Change your password, protect sign-in with a second step, and review what your role permits.',
    '',
    `<div class="cards">
       ${ts.available ? `<div class="card" id="ts-card">
         <h3>Two-step sign-in</h3>
         ${ts.enabled
           ? `<p class="hint">On. Signing in asks for a code from your authenticator app. ${ts.recoveryCodesLeft} recovery code${ts.recoveryCodesLeft === 1 ? '' : 's'} left.</p>
              <label class="field"><span>Your password, to turn it off</span>
                <input id="ts-off-pw" class="input" type="password" autocomplete="current-password" /></label>
              <button class="btn" id="ts-off">Turn off two-step sign-in</button>`
           : `<p class="hint">Off. With it on, signing in needs your password and a code from an authenticator app (any TOTP app), so a stolen password alone is not enough.</p>
              <label class="field"><span>Your password, to begin</span>
                <input id="ts-pw" class="input" type="password" autocomplete="current-password" /></label>
              <button class="btn btn-primary" id="ts-start">Set up two-step sign-in</button>
              <div id="ts-setup" class="hidden">
                <p class="hint">Scan this with your authenticator app, or type the key into it. Then enter the code the app shows.</p>
                <div class="ts-qr" id="ts-qr"></div>
                <p class="hint">Key: <code class="mono" id="ts-secret"></code></p>
                <label class="field"><span>Code from the app</span>
                  <input id="ts-code" class="input" inputmode="numeric" autocomplete="one-time-code" maxlength="8" /></label>
                <button class="btn btn-primary" id="ts-enable">Turn on</button>
              </div>
              <div id="ts-codes" class="hidden">
                <p class="hint">Two-step sign-in is on. These recovery codes each work once if you lose the app. Keep them somewhere safe; they are shown only now.</p>
                <pre class="ts-codes mono" id="ts-codes-list"></pre>
              </div>`}
       </div>` : ''}
       <div class="card">
         <h3>Password</h3>
         <label class="field"><span>Current password</span>
           <input id="pw-cur" class="input" type="password" autocomplete="current-password" /></label>
         <label class="field"><span>New password</span>
           <input id="pw-new" class="input" type="password" autocomplete="new-password" /></label>
         <label class="field"><span>Confirm new password</span>
           <input id="pw-cf" class="input" type="password" autocomplete="new-password" /></label>
         <p class="hint">At least 12 characters. Changing it ends every session, including this one.</p>
         <button class="btn btn-primary" id="pw-save">Change password</button>
       </div>
       <div class="card">
         <h3>Signed in as</h3>
         <div class="kv"><span class="kv-k">Name</span><span class="kv-v">${esc(u.displayName)}</span></div>
         <div class="kv"><span class="kv-k">Email</span><span class="kv-v mono">${esc(u.email)}</span></div>
         <div class="kv"><span class="kv-k">Role</span><span class="kv-v"><span class="tag tag-${esc(u.role)}">${esc(u.role)}</span></span></div>
         <h3 style="margin-top:18px">Your permissions</h3>
         <div class="perms">${[...session.permissions]
           .sort()
           .map((p) => `<code class="perm">${esc(p)}</code>`)
           .join('')}</div>
       </div>
     </div>`
  );

  document.getElementById('ts-start')?.addEventListener('click', async () => {
    try {
      const r = await twoStep.setup(document.getElementById('ts-pw').value);
      document.getElementById('ts-qr').innerHTML = r.qr; // server-drawn SVG, no script in it
      document.getElementById('ts-secret').textContent = r.secret;
      document.getElementById('ts-setup').classList.remove('hidden');
      document.getElementById('ts-code').focus();
    } catch (e) {
      fail(e);
    }
  });
  document.getElementById('ts-enable')?.addEventListener('click', async () => {
    try {
      const r = await twoStep.enable(document.getElementById('ts-code').value);
      document.getElementById('ts-setup').classList.add('hidden');
      document.getElementById('ts-codes-list').textContent = r.recoveryCodes.join('\n');
      document.getElementById('ts-codes').classList.remove('hidden');
      toast('Two-step sign-in is on.');
    } catch (e) {
      fail(e);
    }
  });
  document.getElementById('ts-off')?.addEventListener('click', async () => {
    try {
      await twoStep.disable(document.getElementById('ts-off-pw').value);
      toast('Two-step sign-in is off.');
      renderAccount();
    } catch (e) {
      fail(e);
    }
  });

  document.getElementById('pw-save').onclick = async () => {
    const cur = document.getElementById('pw-cur').value;
    const next = document.getElementById('pw-new').value;
    const confirm = document.getElementById('pw-cf').value;
    if (next !== confirm) {
      toast('The new passwords do not match.', 'bad');
      return;
    }
    try {
      await users.changePassword(null, cur, next);
      toast('Password changed. Signing you out…');
      setTimeout(() => location.reload(), 1200);
    } catch (e) {
      fail(e);
    }
  };
}

// ---------------------------------------------------------------------------
// Dialogs
// ---------------------------------------------------------------------------

function openDialog({ title, body, confirm, danger, onConfirm, onOpen }) {
  const host = document.getElementById('dialog');
  host.innerHTML = `
    <div class="dialog-card" role="dialog" aria-modal="true">
      <h2>${esc(title)}</h2>
      <div class="dialog-body">${body}</div>
      <div class="dialog-actions">
        <button class="btn" data-cancel>Cancel</button>
        <button class="btn ${danger ? 'btn-danger' : 'btn-primary'}" data-confirm>${esc(confirm)}</button>
      </div>
    </div>`;
  host.classList.remove('hidden');

  const close = () => {
    host.classList.add('hidden');
    host.innerHTML = '';
  };
  host.querySelector('[data-cancel]').onclick = close;
  host.onclick = (e) => {
    if (e.target === host) close();
  };
  host.querySelector('[data-confirm]').onclick = async () => {
    const ok = await onConfirm?.();
    if (ok !== false) close();
  };
  onOpen?.();
  return close;
}

function confirmDialog(title, body, label) {
  return new Promise((resolve) => {
    openDialog({
      title,
      body: `<p class="hint">${esc(body)}</p>`,
      confirm: label,
      danger: true,
      onConfirm: () => {
        resolve(true);
        return true;
      },
    });
    // Cancelling resolves false via the close path.
    const host = document.getElementById('dialog');
    host.querySelector('[data-cancel]').addEventListener('click', () => resolve(false));
    host.addEventListener('click', (e) => {
      if (e.target === host) resolve(false);
    });
  });
}

function debounce(fn, ms) {
  let t;
  return (...a) => {
    clearTimeout(t);
    t = setTimeout(() => fn(...a), ms);
  };
}

export { toast };

// ---------------------------------------------------------------------------
// Deployment policy
// ---------------------------------------------------------------------------

// Settings are rendered from the server's own catalogue rather than from a
// copy kept here. A second list in the client is a list that drifts: a setting
// added on the server would be invisible, and one removed would linger as a
// control that silently does nothing.
async function renderPolicy() {
  const body = document.getElementById('console-body');
  body.innerHTML = section(
    'Deployment policy',
    'What every meeting starts from, and what hosts may not override.',
    '',
    '<div id="policy-list" class="policy-list"></div>'
  );

  let data;
  try {
    data = await settings.list();
  } catch (err) {
    document.getElementById('policy-list').innerHTML =
      `<p class="empty">${esc(err.message || 'Could not load policy.')}</p>`;
    return;
  }
  paintPolicy(data);
}

function paintPolicy(data) {
  const host = document.getElementById('policy-list');
  if (!host) return;
  const value = Object.fromEntries((data.settings ?? []).map((s) => [s.key, s.value]));

  host.innerHTML = (data.catalogue ?? [])
    .map((spec) => {
      const v = value[spec.key] ?? spec.default;
      const control =
        spec.type === 'bool'
          ? `<button class="toggle ${v === 'true' ? 'on' : ''}"
                data-key="${esc(spec.key)}" data-next="${v === 'true' ? 'false' : 'true'}"
                role="switch" aria-checked="${v === 'true'}"><span></span></button>`
          : spec.type === 'text'
          ? `<input class="input compact" type="text" data-key="${esc(spec.key)}"
                value="${esc(v ?? '')}" placeholder="${esc(spec.placeholder ?? '')}" />`
          : `<select class="input compact" data-key="${esc(spec.key)}">
              ${(spec.choices ?? [])
                .map(
                  (c) =>
                    `<option value="${esc(c)}"${c === v ? ' selected' : ''}>${esc(c)}</option>`
                )
                .join('')}
            </select>`;

      // A ceiling and a default look different, because confusing them is how
      // an administrator comes to believe something is enforced when a host
      // can simply turn it back on.
      const badge =
        spec.kind === 'ceiling'
          ? `<span class="tag tag-bad" title="Hosts cannot exceed this">Enforced</span>`
          : `<span class="tag tag-member" title="Hosts may change this per meeting">Default</span>`;

      return `<div class="policy-row">
        <div class="policy-main">
          <div class="policy-title"><b>${esc(spec.label)}</b> ${badge}</div>
          <p>${esc(spec.help)}</p>
        </div>
        <div class="policy-control">${control}</div>
      </div>`;
    })
    .join('');

  const apply = async (key, val) => {
    try {
      const fresh = await settings.set(key, val);
      paintPolicy(fresh);
      toast('Policy updated.');
    } catch (err) {
      toast(err.message || 'Could not change that setting.', 'bad');
      paintPolicy(data);
    }
  };

  host.querySelectorAll('button[data-key]').forEach((b) => {
    b.onclick = () => apply(b.dataset.key, b.dataset.next);
  });
  host.querySelectorAll('select[data-key]').forEach((sel) => {
    sel.onchange = () => apply(sel.dataset.key, sel.value);
  });
  // A text setting saves when the field is left or Enter is pressed, not on
  // every keystroke: each save is an audited write, and one per character
  // would fill the log with a domain being typed.
  host.querySelectorAll('input[data-key]').forEach((el) => {
    el.onchange = () => apply(el.dataset.key, el.value.trim());
    el.onkeydown = (e) => {
      if (e.key === 'Enter') el.blur();
    };
  });
}
