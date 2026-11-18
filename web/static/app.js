// Cotta frontend. No framework and no build step: this file is served as-is.
/* global Terminal, FitAddon, WebLinksAddon */

const $app = document.getElementById('app');
const coarse = matchMedia('(pointer: coarse)').matches;

// ---------- tiny DOM helpers ----------

const ICONS = {
  grid: '<rect x="3" y="3" width="5.5" height="5.5" rx="1.5"/><rect x="11.5" y="3" width="5.5" height="5.5" rx="1.5"/><rect x="3" y="11.5" width="5.5" height="5.5" rx="1.5"/><rect x="11.5" y="11.5" width="5.5" height="5.5" rx="1.5"/>',
  lock: '<rect x="4" y="9" width="12" height="8" rx="2"/><path d="M7 9V6.5a3 3 0 016 0V9"/>',
  plus: '<path d="M10 4v12M4 10h12"/>',
  edit: '<path d="M4 16l.8-3.6L13.5 3.7a1.6 1.6 0 012.3 0l.5.5a1.6 1.6 0 010 2.3L7.6 15.2z"/>',
  close: '<path d="M5 5l10 10M15 5L5 15"/>',
  servers: '<rect x="3" y="3.5" width="14" height="5.5" rx="1.8"/><rect x="3" y="11" width="14" height="5.5" rx="1.8"/><path d="M6 6.25h.01M6 13.75h.01"/>',
  moon: '<path d="M16.5 11.8A7 7 0 018.2 3.5a7 7 0 108.3 8.3z"/>',
  sun: '<circle cx="10" cy="10" r="3.2"/><path d="M10 2.5v2M10 15.5v2M2.5 10h2M15.5 10h2M4.7 4.7l1.4 1.4M13.9 13.9l1.4 1.4M4.7 15.3l1.4-1.4M13.9 6.1l1.4-1.4"/>',
  clipboard: '<rect x="4.5" y="4" width="11" height="13" rx="2"/><path d="M7.5 4V3.2A1.2 1.2 0 018.7 2h2.6a1.2 1.2 0 011.2 1.2V4M7.5 9h5M7.5 12.5h3.5"/>',
  key: '<circle cx="7" cy="13" r="3.2"/><path d="M9.3 10.7L16.5 3.5M13.5 6.5l2.2 2.2"/>',
  left: '<path d="M12 5l-5 5 5 5"/>', right: '<path d="M8 5l5 5-5 5"/>',
  up: '<path d="M5 12l5-5 5 5"/>', down: '<path d="M5 8l5 5 5-5"/>',
};

function icon(name) {
  const s = document.createElement('span');
  s.className = 'icon';
  s.setAttribute('aria-hidden', 'true');
  s.innerHTML = `<svg viewBox="0 0 20 20">${ICONS[name]}</svg>`; // static, trusted markup
  return s;
}

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'value') el.value = v;
    else if (k === 'checked' || k === 'disabled' || k === 'required') el[k] = !!v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat(Infinity)) if (c != null && c !== false) el.append(c);
  return el;
}

function toast(msg) {
  const t = h('div', { class: 'toast', role: 'status' }, msg);
  document.getElementById('toasts').append(t);
  setTimeout(() => t.remove(), 6000);
}

function debounce(fn, ms) {
  let t;
  return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
}

function timeAgo(iso) {
  if (!iso) return 'Never used';
  const s = (Date.now() - new Date(iso)) / 1000;
  if (s < 90) return 'Used just now';
  if (s < 3600) return `Used ${Math.round(s / 60)} min ago`;
  if (s < 86400) return `Used ${Math.round(s / 3600)} h ago`;
  if (s < 172800) return 'Used yesterday';
  if (s < 30 * 86400) return `Used ${Math.round(s / 86400)} days ago`;
  return 'Used ' + new Date(iso).toLocaleDateString();
}

function runningFor(iso) {
  const m = Math.max(0, Math.round((Date.now() - new Date(iso)) / 60000));
  if (m < 1) return 'under a minute';
  if (m < 60) return `${m} min`;
  if (m < 1440) return `${Math.floor(m / 60)} h ${m % 60} min`;
  const d = Math.floor(m / 1440);
  return d === 1 ? '1 day' : `${d} days`;
}

function initials(name) {
  const parts = name.split(/[^A-Za-z0-9]+/).filter(Boolean);
  const two = parts.length > 1 ? parts[0][0] + parts[1][0] : (parts[0] || '?').slice(0, 2);
  return two.toUpperCase();
}

const addrOf = (x) => `${x.user}@${x.host.includes(':') ? `[${x.host}]` : x.host}${x.port === 22 ? '' : ':' + x.port}`;

// ---------- API ----------

class ApiError extends Error {
  constructor(status, body) {
    super(body.error || 'Something went wrong.');
    this.status = status;
    this.code = body.code;
    this.body = body;
  }
}

async function api(method, path, body) {
  let res;
  try {
    res = await fetch(path, {
      method,
      credentials: 'same-origin',
      headers: { 'X-Cotta': '1', ...(body ? { 'Content-Type': 'application/json' } : {}) },
      body: body ? JSON.stringify(body) : undefined,
    });
  } catch {
    throw new ApiError(0, { error: "Can't reach your Cotta server.", code: 'offline' });
  }
  let data = {};
  try { data = await res.json(); } catch { /* empty or non-JSON body */ }
  if (!res.ok) {
    if (res.status === 401) boot(); // vault locked or sign-in expired
    throw new ApiError(res.status, data || {});
  }
  return data;
}

// ---------- app state & routing ----------

const state = { hosts: [], sessions: [], keys: [] };
const createdHere = new Set(); // sessions started in this tab
let view = null;               // { destroy() } of the mounted view
let unlocked = false;

function mount(node, v) {
  if (view && view.destroy) view.destroy();
  view = v || null;
  $app.replaceChildren(node);
}

async function boot() {
  let st;
  try {
    st = await api('GET', '/api/state');
  } catch (e) {
    mount(h('div', { class: 'gate' }, h('form', {}, brand(), h('p', { class: 'error' }, e.message),
      h('button', { class: 'btn primary', type: 'button', onclick: boot }, 'Try again'))));
    return;
  }
  unlocked = st.unlocked;
  if (!st.initialized) return gate('setup');
  if (!st.unlocked) return gate('unlock');
  route();
}

function route() {
  if (!unlocked) return;
  const hash = location.hash || '#/';
  const m = hash.match(/^#\/s\/([0-9a-f]+)$/);
  if (m) return sessionView(m[1]);
  if (hash === '#/keys') return keysView();
  return homeView();
}
window.addEventListener('hashchange', route);

const brand = () => h('div', { class: 'brand' }, h('img', { src: 'icon.svg', alt: '' }), 'Cotta');

// ---------- setup / unlock ----------

function gate(mode) {
  const setup = mode === 'setup';
  const pass = h('input', { class: 'input', type: 'password', required: true, autocomplete: setup ? 'new-password' : 'current-password', minlength: setup ? 8 : null });
  const again = setup ? h('input', { class: 'input', type: 'password', required: true, autocomplete: 'new-password' }) : null;
  const err = h('p', { class: 'error', role: 'alert' });
  const btn = h('button', { class: 'btn primary', type: 'submit' }, setup ? 'Create vault' : 'Unlock');
  const form = h('form', {
    onsubmit: async (ev) => {
      ev.preventDefault();
      err.textContent = '';
      if (setup && pass.value !== again.value) { err.textContent = "Those two don't match."; return; }
      btn.disabled = true;
      try {
        await api('POST', setup ? '/api/setup' : '/api/unlock', { passphrase: pass.value });
        unlocked = true;
        route();
      } catch (e) {
        err.textContent = e.message;
        btn.disabled = false;
        pass.select();
      }
    },
  },
    brand(),
    h('h1', {}, setup ? 'Choose a master passphrase' : 'Unlock Cotta'),
    h('p', { class: 'muted' }, setup
      ? 'It encrypts your saved keys and passwords. There is no way to recover it if you forget it.'
      : 'Enter your master passphrase to continue.'),
    h('label', { class: 'field' }, h('span', {}, 'Master passphrase'), pass),
    setup && h('label', { class: 'field' }, h('span', {}, 'Type it again'), again),
    err, btn);
  mount(h('div', { class: 'gate' }, form));
  pass.focus();
}

async function lockVault() {
  try { await api('POST', '/api/lock'); } catch { /* boot() shows the gate either way */ }
  boot();
}

// ---------- dialogs ----------

// modal shows a dialog and resolves with the chosen action's value, or null
// when dismissed.
function modal({ title, body, actions }) {
  return new Promise((resolve) => {
    let result = null;
    const dlg = h('dialog', { onclose: () => { dlg.remove(); resolve(result); } },
      h('form', { method: 'dialog', onsubmit: (ev) => { ev.preventDefault(); const p = actions.find((a) => a.primary); result = p ? resolveValue(p) : null; dlg.close(); } },
        h('h2', {}, title), body,
        h('div', { class: 'dialog-actions' }, actions.map((a) =>
          h('button', { class: 'btn ' + (a.class || (a.primary ? 'primary' : 'quiet')), type: a.primary ? 'submit' : 'button', onclick: a.primary ? null : () => { result = resolveValue(a); dlg.close(); } }, a.label)))));
    const resolveValue = (a) => (typeof a.value === 'function' ? a.value() : a.value);
    document.body.append(dlg);
    dlg.showModal();
  });
}

function askPassword(host, message) {
  const input = h('input', { class: 'input', type: 'password', required: true, autocomplete: 'off' });
  setTimeout(() => input.focus());
  return modal({
    title: `Password for ${host.name}`,
    body: [h('p', { class: 'muted' }, message), h('label', { class: 'field' }, h('span', {}, addrOf(host)), input)],
    actions: [{ label: 'Cancel', value: null }, { label: 'Connect', primary: true, value: () => input.value }],
  });
}

function confirmHostKey(host, info) {
  if (info.code === 'hostkey_changed') {
    return modal({
      title: `${host.name} has a different key`,
      body: [
        h('div', { class: 'check warn' },
          h('strong', {}, 'This is not the server key you trusted.'),
          h('span', {}, 'Either the server was reinstalled, or something is impersonating it. If you did not expect this, do not connect.'),
          h('span', {}, 'Trusted before:'), h('span', { class: 'mono' }, info.pinnedFingerprint),
          h('span', {}, 'Offered now:'), h('span', { class: 'mono' }, `${info.keyType} ${info.fingerprint}`)),
      ],
      actions: [{ label: 'Trust the new key', value: true, class: 'danger' }, { label: "Don't connect", primary: true, value: false }],
    });
  }
  return modal({
    title: `First time connecting to ${host.name}`,
    body: [
      h('div', { class: 'check ok' },
        h('strong', {}, 'Check this fingerprint matches the server'),
        h('span', { class: 'mono' }, `${info.keyType} ${info.fingerprint}`),
        h('span', {}, 'Cotta will remember it and warn you if it ever changes.')),
    ],
    actions: [{ label: 'Cancel', value: false }, { label: 'Trust & connect', primary: true, value: true }],
  });
}

// ---------- connecting ----------

function estimateSize() {
  return { cols: Math.max(20, Math.floor((innerWidth - 60) / 8.4)), rows: Math.max(8, Math.floor((innerHeight - 190) / 18)) };
}

async function connect(host, password) {
  try {
    const s = await api('POST', '/api/sessions', { hostId: host.id, password, ...estimateSize() });
    createdHere.add(s.id);
    location.hash = '#/s/' + s.id;
  } catch (e) {
    if (e.code === 'password_required' || (e.code === 'auth_failed' && host.authType === 'ask')) {
      const pw = await askPassword(host, e.message);
      if (pw) return connect(host, pw);
    } else if (e.code === 'hostkey_unknown' || e.code === 'hostkey_changed') {
      if (await confirmHostKey(host, e.body)) {
        try {
          await api('POST', `/api/hosts/${host.id}/trust`, { hostKey: e.body.hostKey });
        } catch (e2) { toast(e2.message); return; }
        return connect(host, password);
      }
    } else if (e.status !== 401) {
      toast(`${host.name}: ${e.message}`);
    }
  }
}

async function withBusy(btn, label, fn) {
  const old = btn.textContent;
  btn.disabled = true;
  btn.textContent = label;
  try { await fn(); } finally { btn.disabled = false; btn.textContent = old; }
}

// ---------- light / dark ----------
// With no saved choice the system setting decides; the switch saves an
// explicit choice in this browser (theme.js applies it before first paint).

const systemDark = matchMedia('(prefers-color-scheme: dark)');
function isDark() {
  const chosen = document.documentElement.dataset.theme;
  return chosen ? chosen === 'dark' : systemDark.matches;
}
function syncThemeColor() {
  document.querySelector('meta[name=theme-color]')?.setAttribute('content', isDark() ? '#1D1512' : '#EDE4DA');
}
systemDark.addEventListener('change', syncThemeColor);
syncThemeColor();

// themeButton offers the theme you are not in. compact = icon only.
function themeButton(compact) {
  const btn = h('button', { class: 'btn ghost theme-switch' + (compact ? ' icon-only' : ''), type: 'button' });
  const paint = () => {
    const label = isDark() ? 'Light mode' : 'Dark mode';
    btn.replaceChildren(icon(isDark() ? 'sun' : 'moon'), compact ? '' : label);
    btn.setAttribute('aria-label', label);
  };
  btn.addEventListener('click', () => {
    const next = isDark() ? 'light' : 'dark';
    document.documentElement.dataset.theme = next;
    try { localStorage.setItem('cotta-theme', next); } catch { /* still applies for this visit */ }
    syncThemeColor();
    document.querySelectorAll('.theme-switch').forEach((b) => b.dispatchEvent(new Event('repaint')));
  });
  btn.addEventListener('repaint', paint);
  systemDark.addEventListener('change', paint);
  paint();
  return btn;
}

// ---------- shell chrome (sidebar / mobile bars) ----------

function shell(active, main) {
  const link = (href, label, key, extra) => h('a', { href, 'aria-current': active === key ? 'page' : null }, label, extra);
  return h('div', { class: 'shell' },
    h('nav', { class: 'sidebar', 'aria-label': 'Main' },
      brand(),
      h('div', { class: 'nav' },
        link('#/', 'All servers', 'home', h('span', { class: 'count' }, String(state.hosts.length || ''))),
        link('#/keys', 'Keys', 'keys')),
      h('div', { class: 'spacer' }),
      h('div', { class: 'nav' },
        themeButton(false),
        h('button', { class: 'btn ghost', type: 'button', onclick: lockVault }, icon('lock'), 'Lock vault'))),
    h('header', { class: 'topbar' },
      brand(),
      h('div', { class: 'actions' },
        themeButton(true),
        h('button', { class: 'btn ghost icon-only', type: 'button', 'aria-label': 'Lock vault', onclick: lockVault }, icon('lock')))),
    main,
    h('nav', { class: 'tabbar', 'aria-label': 'Main' },
      h('a', { href: '#/', 'aria-current': active === 'home' ? 'page' : null }, icon('servers'), 'Servers'),
      h('a', { href: '#/keys', 'aria-current': active === 'keys' ? 'page' : null }, icon('key'), 'Keys')));
}

// ---------- home ----------

async function homeView() {
  const quickInput = h('input', { id: 'qc', type: 'text', placeholder: coarse ? 'Paste user@host or an IP' : 'Paste anything — user@host, an ssh command, or just an IP', autocapitalize: 'off', autocomplete: 'off', spellcheck: 'false' });
  const quickErr = h('p', { class: 'error', role: 'alert' });
  const runningBox = h('div');
  const hostsBox = h('div', { class: 'section' });

  const quick = h('form', {
    class: 'quick',
    onsubmit: async (ev) => {
      ev.preventDefault();
      quickErr.textContent = '';
      const text = quickInput.value.trim();
      if (!text) return openHostDialog({ onSaved: load });
      try {
        const p = await api('POST', '/api/parse', { text });
        const match = state.hosts.find((x) => x.host.toLowerCase() === p.host.toLowerCase() && x.port === p.port && (!p.user || x.user === p.user));
        if (match) { quickInput.value = ''; return connect(match); }
        openHostDialog({ prefill: p, pasted: text, onSaved: load });
      } catch (e) { quickErr.textContent = e.message; }
    },
  }, h('label', { class: 'sr', for: 'qc' }, 'Quick connect'), quickInput, h('button', { class: 'btn primary', type: 'submit' }, 'Connect'));

  const main = h('main', { class: 'main' },
    h('section', { class: 'section' }, h('h1', { class: 'title' }, 'Where to?'), quick, quickErr),
    runningBox, hostsBox);

  function paint() {
    const live = new Map(state.sessions.map((s) => [s.hostId, s]));
    runningBox.replaceChildren(...(state.sessions.length ? [h('section', { class: 'section', 'aria-label': 'Still running' },
      h('div', { class: 'section-head' }, h('h2', {}, 'Still running'),
        h('span', { class: 'sub' }, `${state.sessions.length} ${state.sessions.length === 1 ? 'session' : 'sessions'} kept alive on your Cotta server`)),
      h('div', { class: 'grid running' }, state.sessions.map((s) =>
        h('article', { class: 'card live' },
          h('div', { class: 'card-head' },
            h('div', { class: 'tile' }, initials(s.name)),
            h('div', { class: 'card-text' }, h('strong', {}, s.name),
              h('span', {}, `${s.attached ? 'Open' : 'Left open'} on ${s.lastDevice} · running ${runningFor(s.startedAt)}`)),
            h('a', { class: 'btn primary', href: '#/s/' + s.id }, 'Resume'),
            h('button', { class: 'btn', type: 'button', onclick: () => endSession(s).then(load) }, 'End')),
          h('div', { class: 'lastline' }, s.lastLine || ' ')))))] : []));

    hostsBox.replaceChildren(
      h('div', { class: 'section-head' }, h('h2', {}, 'All servers'),
        h('button', { class: 'btn outline', type: 'button', onclick: () => openHostDialog({ onSaved: load }) }, icon('plus'), 'Add server')),
      state.hosts.length
        ? h('div', { class: 'grid' }, state.hosts.map((x) => {
          const s = live.get(x.id);
          const go = s
            ? h('a', { class: 'btn primary', href: '#/s/' + s.id }, 'Resume')
            : h('button', { class: 'btn primary', type: 'button', onclick: (ev) => withBusy(ev.currentTarget, 'Connecting…', () => connect(x)) }, 'Connect');
          return h('article', { class: 'card' },
            h('div', { class: 'card-head' },
              h('div', { class: 'tile' }, initials(x.name)),
              h('div', { class: 'card-text' }, h('strong', {}, x.name), h('span', { class: 'mono' }, addrOf(x)))),
            h('div', { class: 'card-meta' }, s ? 'Session running' : timeAgo(x.lastUsedAt)),
            h('div', { class: 'card-actions' }, go,
              h('button', { class: 'btn icon-only', type: 'button', 'aria-label': `Edit ${x.name}`, onclick: () => openHostDialog({ host: x, onSaved: load }) }, icon('edit'))));
        }))
        : h('div', { class: 'empty' }, h('strong', {}, 'No servers yet'),
          h('span', {}, 'Paste an address or an ssh command above and Cotta will fill in the rest.')));
    const count = document.querySelector('.sidebar .count');
    if (count) count.textContent = String(state.hosts.length || '');
  }

  let alive = true, painted = '';
  async function load() {
    try {
      const [hosts, sessions] = await Promise.all([api('GET', '/api/hosts'), api('GET', '/api/sessions')]);
      if (!alive) return;
      state.hosts = hosts;
      state.sessions = sessions;
      // Repaint only when something changed (or a minute ticked over), so a
      // button is never swapped out from under a click.
      const snapshot = JSON.stringify([hosts, sessions, Math.floor(Date.now() / 60000)]);
      if (snapshot !== painted) { painted = snapshot; paint(); }
    } catch { /* 401 is handled by api(); other errors retry on the next tick */ }
  }
  const timer = setInterval(load, 5000);
  mount(shell('home', main), { destroy() { alive = false; clearInterval(timer); } });
  paint();
  await load();
  if (!coarse) quickInput.focus();
}

async function endSession(s) {
  const ok = await modal({
    title: `End the session on ${s.name}?`,
    body: h('p', { class: 'muted' }, 'The shell is closed on the server and anything running in it stops.'),
    actions: [{ label: 'Keep it running', value: false }, { label: 'End session', primary: true, value: true }],
  });
  if (!ok) return false;
  try { await api('DELETE', '/api/sessions/' + s.id); } catch (e) { toast(e.message); }
  return true;
}

// ---------- add / edit a server ----------

async function openHostDialog({ host = null, prefill = null, pasted = '', onSaved = () => {} } = {}) {
  try { state.keys = await api('GET', '/api/keys'); } catch { return; }
  const isNew = !host;
  const v = host || {
    name: prefill?.name || '', user: prefill?.user || '', host: prefill?.host || '', port: prefill?.port || 22,
    authType: state.keys.length ? 'key' : 'password', keyId: state.keys[0]?.id ?? null,
  };
  let nameTouched = !isNew;
  let probe = null; // last probe result for the host/port currently typed

  const paste = h('input', { class: 'input mono', type: 'text', value: pasted, placeholder: 'ssh deploy@example.com -p 2222', autocapitalize: 'off', autocomplete: 'off', spellcheck: 'false' });
  const pasteNote = h('p', { class: 'error' });
  const user = h('input', { class: 'input mono', type: 'text', value: v.user, required: true, autocapitalize: 'off', autocomplete: 'off', spellcheck: 'false' });
  const hostIn = h('input', { class: 'input mono', type: 'text', value: v.host, required: true, autocapitalize: 'off', autocomplete: 'off', spellcheck: 'false' });
  const port = h('input', { class: 'input mono', type: 'text', inputmode: 'numeric', value: String(v.port), required: true });
  const name = h('input', { class: 'input', type: 'text', value: v.name, placeholder: 'Shown on the card', oninput: () => { nameTouched = true; } });
  const radio = (val) => h('input', { type: 'radio', name: 'auth', value: val, checked: v.authType === val, onchange: syncAuth });
  const keySel = h('select', { class: 'input' }, state.keys.map((k) => h('option', { value: String(k.id) }, k.name)));
  if (v.keyId != null) keySel.value = String(v.keyId);
  const password = h('input', { class: 'input', type: 'password', autocomplete: 'off', placeholder: host?.hasPassword ? 'Leave empty to keep the stored one' : 'Stored encrypted' });
  const rKey = radio('key'), rPw = radio('password'), rAsk = radio('ask');
  if (!state.keys.length) rKey.disabled = true;
  const check = h('div', { class: 'check' }, 'Enter a host and Cotta will check it.');
  const err = h('p', { class: 'error', role: 'alert' });
  const saveBtn = h('button', { class: 'btn quiet', type: 'button', onclick: () => submit(false) }, 'Save');
  const goBtn = h('button', { class: 'btn primary', type: 'submit' }, 'Save & connect');

  function syncAuth() {
    keySel.style.display = rKey.checked ? '' : 'none';
    password.style.display = rPw.checked ? '' : 'none';
  }

  const applyParsed = debounce(async () => {
    pasteNote.textContent = '';
    if (!paste.value.trim()) return;
    try {
      const p = await api('POST', '/api/parse', { text: paste.value });
      if (p.user) user.value = p.user;
      hostIn.value = p.host;
      port.value = String(p.port);
      if (!nameTouched) name.value = p.name;
      pasteNote.textContent = (p.warnings || []).join(' ');
      runProbe();
    } catch (e) { pasteNote.textContent = e.message; }
  }, 250);
  paste.addEventListener('input', applyParsed);

  let probeSeq = 0;
  const runProbe = debounce(async () => {
    const hv = hostIn.value.trim(), pv = Number(port.value);
    probe = null;
    paintCheck();
    if (!hv || !pv) return;
    const seq = ++probeSeq;
    check.className = 'check';
    check.replaceChildren('Checking from your Cotta server…');
    try {
      const res = await api('POST', '/api/probe', { host: hv, port: pv });
      if (seq !== probeSeq) return;
      probe = { ...res, host: hv, port: pv };
    } catch (e) {
      if (seq !== probeSeq) return;
      probe = { reachable: false, error: e.message };
    }
    paintCheck();
  }, 500);
  hostIn.addEventListener('input', runProbe);
  port.addEventListener('input', runProbe);

  // What the probe means for trust: 'new' key to pin, 'same' as pinned, or 'changed'.
  function trustState() {
    if (!probe?.reachable) return null;
    const sameTarget = host && host.host === probe.host && host.port === probe.port;
    if (!sameTarget || !host.trusted) return 'new';
    return host.fingerprint === probe.fingerprint ? 'same' : 'changed';
  }

  function paintCheck() {
    const t = trustState();
    goBtn.textContent = t === 'new' ? 'Trust, save & connect' : t === 'changed' ? 'Trust new key & connect' : 'Save & connect';
    if (!probe) { check.className = 'check'; check.replaceChildren('Enter a host and Cotta will check it.'); return; }
    if (!probe.reachable) {
      check.className = 'check warn';
      check.replaceChildren(h('strong', {}, 'Not reachable right now'), h('span', {}, probe.error), h('span', {}, 'You can still save it; Cotta will ask you to trust its key on first connect.'));
      return;
    }
    const fp = h('span', { class: 'mono' }, `${probe.keyType} ${probe.fingerprint}`);
    if (t === 'changed') {
      check.className = 'check warn';
      check.replaceChildren(h('strong', {}, "This server's key has changed since you trusted it"),
        h('span', {}, 'If you did not reinstall it, do not continue. Trusted before:'), h('span', { class: 'mono' }, host.fingerprint),
        h('span', {}, 'Offered now:'), fp);
    } else if (t === 'same') {
      check.className = 'check ok';
      check.replaceChildren(h('strong', {}, `Reachable from your Cotta server · ${probe.latencyMs} ms`), h('span', {}, 'Its key matches the one you trusted.'));
    } else {
      check.className = 'check ok';
      check.replaceChildren(h('strong', {}, `Reachable from your Cotta server · ${probe.latencyMs} ms`),
        h('span', {}, 'First time seeing this server. Check its fingerprint matches:'), fp);
    }
  }

  async function submit(thenConnect) {
    err.textContent = '';
    if (!form.reportValidity()) return;
    const authType = rKey.checked ? 'key' : rPw.checked ? 'password' : 'ask';
    const body = {
      name: name.value, user: user.value, host: hostIn.value, port: Number(port.value) || 0,
      authType, keyId: authType === 'key' ? Number(keySel.value) : null, password: password.value,
      hostKey: trustState() === 'new' || trustState() === 'changed' ? probe.hostKey : '',
    };
    saveBtn.disabled = goBtn.disabled = true;
    try {
      const saved = isNew ? await api('POST', '/api/hosts', body) : await api('PUT', '/api/hosts/' + host.id, body);
      dlg.close();
      await onSaved();
      if (thenConnect) connect(saved);
    } catch (e) {
      err.textContent = e.message;
      saveBtn.disabled = goBtn.disabled = false;
    }
  }

  const form = h('form', { onsubmit: (ev) => { ev.preventDefault(); submit(true); } },
    h('div', { class: 'dialog-head' }, h('h2', {}, isNew ? 'Add a server' : `Edit ${host.name}`),
      h('button', { class: 'btn ghost icon-only', type: 'button', 'aria-label': 'Close', onclick: () => dlg.close() }, icon('close'))),
    isNew && h('label', { class: 'field' }, h('span', {}, 'Paste anything'), paste),
    isNew && pasteNote,
    h('div', { class: 'row3' },
      h('label', { class: 'field' }, h('span', {}, 'User'), user),
      h('label', { class: 'field' }, h('span', {}, 'Host'), hostIn),
      h('label', { class: 'field' }, h('span', {}, 'Port'), port)),
    h('fieldset', {}, h('legend', {}, 'Sign in with'),
      h('label', { class: 'choice' }, rKey, h('b', {}, 'Key'), state.keys.length ? keySel : h('small', {}, 'No keys yet — add one under Keys')),
      h('label', { class: 'choice' }, rPw, h('b', {}, 'Password'), password),
      h('label', { class: 'choice' }, rAsk, h('b', {}, 'Ask each time'), h('small', {}, 'nothing saved'))),
    check,
    h('label', { class: 'field' }, h('span', {}, 'Name'), name),
    err,
    h('div', { class: 'dialog-actions' },
      !isNew && h('button', {
        class: 'btn danger', type: 'button', style: 'margin-right:auto',
        onclick: async () => {
          const ok = await modal({ title: `Delete ${host.name}?`, body: h('p', { class: 'muted' }, 'Its saved password and trusted key are removed. Running sessions are not affected.'), actions: [{ label: 'Cancel', value: false }, { label: 'Delete', primary: true, value: true }] });
          if (!ok) return;
          try { await api('DELETE', '/api/hosts/' + host.id); dlg.close(); onSaved(); } catch (e) { err.textContent = e.message; }
        },
      }, 'Delete'),
      saveBtn, goBtn));

  const dlg = h('dialog', { onclose: () => dlg.remove() }, form);
  document.body.append(dlg);
  syncAuth();
  dlg.showModal();
  if (v.host) runProbe();
  (isNew && !pasted ? paste : v.user ? name : user).focus();
}

// ---------- keys ----------

async function keysView() {
  const list = h('div', { class: 'section' });
  const main = h('main', { class: 'main' },
    h('section', { class: 'section' },
      h('div', { class: 'section-head' }, h('h1', { class: 'title', style: 'display:block' }, 'Keys'),
        h('div', { class: 'card-actions' },
          h('button', { class: 'btn quiet', type: 'button', onclick: importKey }, 'Import a key'),
          h('button', { class: 'btn primary', type: 'button', style: 'flex:none', onclick: generateKey }, 'Generate a key'))),
      h('p', { class: 'muted' }, 'Private keys are stored encrypted in your vault and never leave your Cotta server.')),
    list);

  function paint() {
    list.replaceChildren(...(state.keys.length ? state.keys.map((k) =>
      h('article', { class: 'card' },
        h('div', { class: 'keyrow' },
          h('div', { class: 'card-text' }, h('strong', {}, k.name), h('span', { class: 'mono' }, `${k.keyType} ${k.fingerprint}`)),
          h('button', { class: 'btn', type: 'button', onclick: () => copy(k.publicKey) }, 'Copy public key'),
          h('button', {
            class: 'btn danger', type: 'button',
            onclick: async () => {
              const ok = await modal({ title: `Delete ${k.name}?`, body: h('p', { class: 'muted' }, 'Servers that sign in with this key will need another way in.'), actions: [{ label: 'Cancel', value: false }, { label: 'Delete', primary: true, value: true }] });
              if (ok) { try { await api('DELETE', '/api/keys/' + k.id); } catch (e) { toast(e.message); } load(); }
            },
          }, 'Delete'))))
      : [h('div', { class: 'empty' }, h('strong', {}, 'No keys yet'), h('span', {}, 'Generate one here and add its public half to your servers, or import a key you already use.'))]));
  }
  async function load() {
    try { state.keys = await api('GET', '/api/keys'); paint(); } catch { /* handled by api() */ }
  }
  async function copy(text) {
    try { await navigator.clipboard.writeText(text); toast('Public key copied.'); } catch { toast('Copy failed — select the key text instead.'); }
  }

  async function generateKey() {
    const name = h('input', { class: 'input', type: 'text', required: true, placeholder: 'e.g. cotta' });
    setTimeout(() => name.focus());
    const val = await modal({ title: 'Generate a key', body: [h('p', { class: 'muted' }, 'Creates a new ed25519 key pair inside your vault.'), h('label', { class: 'field' }, h('span', {}, 'Name'), name)], actions: [{ label: 'Cancel', value: null }, { label: 'Generate', primary: true, value: () => name.value.trim() }] });
    if (!val) return;
    try {
      const k = await api('POST', '/api/keys', { name: val, generate: true });
      await load();
      await modal({
        title: 'Add this to your server',
        body: [h('p', { class: 'muted' }, 'Append this line to ~/.ssh/authorized_keys on each server that should accept the key.'), h('div', { class: 'pubkey' }, k.publicKey)],
        actions: [{ label: 'Done', value: null }, { label: 'Copy', primary: true, value: () => { copy(k.publicKey); return null; } }],
      });
    } catch (e) { toast(e.message); }
  }

  async function importKey(_ev, carry = {}) {
    const name = h('input', { class: 'input', type: 'text', required: true, value: carry.name || '' });
    const pem = h('textarea', { class: 'input mono', required: true, spellcheck: 'false', placeholder: '-----BEGIN OPENSSH PRIVATE KEY-----' });
    pem.value = carry.pem || '';
    const pass = h('input', { class: 'input', type: 'password', autocomplete: 'off', placeholder: 'Only if the key has one' });
    setTimeout(() => (carry.needPass ? pass : name).focus());
    const val = await modal({
      title: 'Import a key',
      body: [carry.error && h('p', { class: 'error' }, carry.error),
        h('label', { class: 'field' }, h('span', {}, 'Name'), name),
        h('label', { class: 'field' }, h('span', {}, 'Private key'), pem),
        h('label', { class: 'field' }, h('span', {}, 'Key passphrase'), pass)],
      actions: [{ label: 'Cancel', value: null }, { label: 'Import', primary: true, value: () => ({ name: name.value.trim(), pem: pem.value, pass: pass.value }) }],
    });
    if (!val) return;
    try {
      await api('POST', '/api/keys', { name: val.name, privateKey: val.pem, passphrase: val.pass });
      load();
    } catch (e) {
      if (e.status !== 401) importKey(null, { name: val.name, pem: val.pem, error: e.message, needPass: e.code === 'key_passphrase' });
    }
  }

  mount(shell('keys', main));
  paint();
  load();
}

// ---------- session (terminal) ----------

const THEME = {
  background: '#1F1612', foreground: '#EADCCF', cursor: '#EADCCF', cursorAccent: '#1F1612', selectionBackground: '#5A4438',
  black: '#1F1612', red: '#E06C5A', green: '#9DC183', yellow: '#E5B567', blue: '#7FA6C9', magenta: '#C792B4', cyan: '#7FBFB3', white: '#EADCCF',
  brightBlack: '#A8917F', brightRed: '#F08A78', brightGreen: '#B5D69B', brightYellow: '#F2CD8A', brightBlue: '#9CC0E0', brightMagenta: '#DDB0CE', brightCyan: '#9FD8CD', brightWhite: '#FFF6EF',
};
const CLOSE_ENDED = 4000;

async function sessionView(id) {
  // gone: this view was replaced. closed: stop reconnecting (gone, or the session ended).
  let gone = false, closed = false, ws = null, attempt = 0, pingTimer = 0, pongTimer = 0, retryTimer = 0, listTimer = 0, ctrl = false, noted = false;
  let term = null, ro = null;
  const enc = new TextEncoder();

  const tabs = h('div', { class: 'session-tabs', role: 'tablist', 'aria-label': 'Running sessions' });
  const title = h('strong', {}, '…');
  const addr = h('span', { class: 'addr' });
  const pill = h('span', { class: 'pill busy' }, 'Connecting…');
  const devices = h('span', { class: 'pill busy', style: 'display:none' });
  const note = h('div', { class: 'term-note', style: 'display:none' });
  const termEl = h('div', { class: 'term' });
  const overlay = h('div', { class: 'term-overlay', style: 'display:none' });
  const ctrlBtn = h('button', { type: 'button', 'aria-pressed': 'false' }, 'ctrl');

  const keyBtn = (label, seq, aria) => h('button', {
    type: 'button', 'aria-label': aria || null,
    onpointerdown: (ev) => ev.preventDefault(), // keep focus (and the on-screen keyboard) on the terminal
    onclick: () => { send(enc.encode(typeof seq === 'function' ? seq() : seq)); term.focus(); },
  }, label);
  const arrow = (c) => () => (term.modes.applicationCursorKeysMode ? '\x1bO' : '\x1b[') + c;
  ctrlBtn.addEventListener('pointerdown', (ev) => ev.preventDefault());
  ctrlBtn.addEventListener('click', () => { setCtrl(!ctrl); term.focus(); });
  const keys = h('div', { class: 'keys', role: 'toolbar', 'aria-label': 'Extra keys' },
    keyBtn('esc', '\x1b'), ctrlBtn, keyBtn('tab', '\t'),
    keyBtn(icon('left'), arrow('D'), 'Left'), keyBtn(icon('up'), arrow('A'), 'Up'),
    keyBtn(icon('down'), arrow('B'), 'Down'), keyBtn(icon('right'), arrow('C'), 'Right'),
    keyBtn('|', '|'), keyBtn('~', '~'), keyBtn('/', '/'));

  // Saved commands: one press types the command and presses Enter.
  const cmdBar = h('div', { class: 'cmds', role: 'toolbar', 'aria-label': 'Saved commands' });
  let hostId = null, commands = [], editingCmds = false;
  const keepFocus = (ev) => ev.preventDefault(); // don't take focus (or the on-screen keyboard) from the terminal

  function paintCmds() {
    cmdBar.className = 'cmds' + (editingCmds ? ' editing' : '');
    cmdBar.replaceChildren(...[
      h('button', { class: 'cmd clip', type: 'button', title: 'Review your clipboard, then send it', onpointerdown: keepFocus, onclick: openClipboard }, icon('clipboard'), 'Clipboard'),
      ...commands.map((c) => h('button', {
        class: 'cmd', type: 'button', title: editingCmds ? 'Edit this command' : c.command,
        onpointerdown: keepFocus,
        onclick: () => (editingCmds ? editCommand(c) : sendDialog({ title: c.label || 'Send saved command', text: c.command, enter: true })),
      }, c.label || c.command.split('\n')[0])),
      h('button', { class: 'tool', type: 'button', onpointerdown: keepFocus, onclick: () => editCommand(null) }, commands.length ? '+ Add' : '+ Save a command'),
      commands.length ? h('button', { class: 'tool', type: 'button', 'aria-pressed': String(editingCmds), onpointerdown: keepFocus, onclick: () => { editingCmds = !editingCmds; paintCmds(); } }, editingCmds ? 'Done' : 'Edit') : null,
      editingCmds ? h('span', { class: 'hint' }, 'Pick a command to change it') : null,
    ].filter(Boolean)); // replaceChildren would print a bare null as text
  }

  // sendDialog is the only route for pasted or saved text into the session:
  // it shows the text so it can be checked or changed, and nothing is sent
  // until Send is pressed.
  let sendOpen = false;
  async function sendDialog({ title: heading, text, enter, hint }) {
    if (sendOpen || !term) return;
    sendOpen = true;
    const area = h('textarea', { class: 'input mono', required: true, spellcheck: 'false', autocapitalize: 'off', autocomplete: 'off' });
    area.value = text;
    area.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter' && (ev.ctrlKey || ev.metaKey)) { ev.preventDefault(); area.form.requestSubmit(); }
    });
    const tick = h('input', { type: 'checkbox', checked: enter });
    setTimeout(() => area.focus());
    const val = await modal({
      title: heading,
      body: [
        hint && h('p', { class: 'muted' }, hint),
        h('label', { class: 'field' }, h('span', {}, `Will be typed into ${title.textContent}`), area),
        h('label', { class: 'tick' }, tick, 'Press Enter after sending'),
      ],
      actions: [{ label: 'Cancel', value: null }, { label: 'Send', primary: true, value: () => ({ text: area.value, enter: tick.checked }) }],
    });
    sendOpen = false;
    if (gone) return;
    if (val && val.text) {
      term.paste(val.text); // honours the remote app's bracketed-paste mode
      if (val.enter) send(enc.encode('\r'));
    }
    term.focus();
  }

  async function openClipboard() {
    let text = '', hint = '';
    try {
      if (!navigator.clipboard?.readText) throw new Error('unavailable');
      text = await navigator.clipboard.readText();
      if (!text) hint = 'Your clipboard has no text in it. Type or paste below.';
    } catch {
      hint = "Your browser didn't let Cotta read the clipboard. Paste into the box below instead.";
    }
    sendDialog({ title: 'Send from clipboard', text, enter: false, hint });
  }

  async function loadCmds() {
    if (hostId == null) return;
    try { commands = await api('GET', '/api/commands?hostId=' + hostId); } catch { return; }
    if (!commands.length) editingCmds = false;
    if (!gone) paintCmds();
  }

  async function editCommand(c) {
    const label = h('input', { class: 'input', type: 'text', value: c?.label || '', maxlength: '80', placeholder: 'Optional — shown on the button' });
    const text = h('textarea', { class: 'input mono', required: true, spellcheck: 'false', autocapitalize: 'off', placeholder: 'sudo systemctl restart nginx' });
    text.value = c?.command || '';
    const everywhere = h('input', { type: 'checkbox', checked: c ? c.hostId == null : false });
    setTimeout(() => text.focus());
    const val = await modal({
      title: c ? 'Edit saved command' : 'Save a command',
      body: [
        h('label', { class: 'field' }, h('span', {}, 'Command'), text),
        h('label', { class: 'field' }, h('span', {}, 'Button label'), label),
        h('label', { class: 'tick' }, everywhere, `Show on every server, not just ${title.textContent}`),
        h('p', { class: 'muted' }, 'Pressing the button shows the command so you can check or change it before sending.'),
      ],
      actions: [
        c && { label: 'Delete', value: 'delete', class: 'danger' },
        { label: 'Cancel', value: null },
        { label: 'Save', primary: true, value: () => ({ label: label.value, command: text.value, hostId: everywhere.checked ? null : hostId }) },
      ].filter(Boolean),
    });
    if (!val) { term.focus(); return; }
    try {
      if (val === 'delete') await api('DELETE', '/api/commands/' + c.id);
      else if (c) await api('PUT', '/api/commands/' + c.id, val);
      else await api('POST', '/api/commands', val);
    } catch (e) { if (e.status !== 401) toast(e.message); }
    await loadCmds();
    term.focus();
  }

  const root = h('div', { class: 'session' },
    tabs,
    h('div', { class: 'session-head' }, title, addr, pill, devices, h('div', { class: 'spacer' }),
      h('button', {
        class: 'btn danger', type: 'button',
        onclick: async () => { if (await endSession({ id, name: title.textContent })) location.hash = '#/'; },
      }, 'End session')),
    h('section', { class: 'term-wrap', 'aria-label': 'Terminal' }, note, termEl, cmdBar, keys, overlay));

  function setCtrl(on) { ctrl = on; ctrlBtn.setAttribute('aria-pressed', String(on)); }
  function setStatus(ok, text) { pill.className = 'pill' + (ok ? '' : ' busy'); pill.textContent = text; }
  function showOverlay(...children) { overlay.replaceChildren(...children); overlay.style.display = ''; }
  function hideOverlay() { overlay.style.display = 'none'; }

  mount(root, {
    destroy() {
      gone = closed = true;
      clearInterval(pingTimer); clearTimeout(pongTimer); clearTimeout(retryTimer); clearInterval(listTimer);
      if (ws) ws.close();
      if (ro) ro.disconnect();
      window.visualViewport?.removeEventListener('resize', onViewport);
      window.visualViewport?.removeEventListener('scroll', onViewport);
      document.removeEventListener('visibilitychange', onVisible);
      document.title = 'Cotta';
      if (term) term.dispose();
    },
  });

  // Keep the terminal above the on-screen keyboard.
  function onViewport() {
    const vv = window.visualViewport;
    root.style.bottom = 'auto';
    root.style.top = vv.offsetTop + 'px';
    root.style.height = vv.height + 'px';
  }
  if (window.visualViewport && coarse) {
    window.visualViewport.addEventListener('resize', onViewport);
    window.visualViewport.addEventListener('scroll', onViewport);
    onViewport();
  }

  // xterm measures its font when opened, so the web font must be ready.
  await Promise.race([document.fonts.load('14px "JetBrains Mono"'), new Promise((r) => setTimeout(r, 1500))]);
  if (gone) return;
  term = new Terminal({ fontFamily: '"JetBrains Mono", ui-monospace, Menlo, Consolas, monospace', fontSize: coarse ? 13 : 14, lineHeight: 1.2, theme: THEME, cursorBlink: true, scrollback: 10000 });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.loadAddon(new WebLinksAddon.WebLinksAddon());
  term.open(termEl);
  fit.fit();
  paintCmds();

  // Pasting straight into the terminal (Ctrl+V, right-click, middle-click)
  // is diverted to the review dialog instead of being typed blind.
  termEl.addEventListener('paste', (ev) => {
    ev.preventDefault();
    ev.stopPropagation();
    sendDialog({ title: 'Paste into session', text: ev.clipboardData?.getData('text/plain') || '', enter: false });
  }, true);

  function send(bytes) { if (ws && ws.readyState === WebSocket.OPEN) ws.send(bytes); }
  function sendJSON(obj) { if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj)); }
  const sendSize = () => sendJSON({ type: 'resize', cols: term.cols, rows: term.rows });

  term.onData((d) => {
    if (ctrl) {
      setCtrl(false);
      if (d.length === 1) {
        const c = d.toUpperCase().charCodeAt(0);
        if (c >= 64 && c <= 95) d = String.fromCharCode(c - 64);
        else if (d === ' ') d = '\x00';
      }
    }
    send(enc.encode(d));
  });
  term.onBinary((d) => send(Uint8Array.from(d, (ch) => ch.charCodeAt(0))));
  term.onResize(sendSize);
  ro = new ResizeObserver(debounce(() => { if (!gone) fit.fit(); }, 60));
  ro.observe(termEl);

  function open() {
    if (closed) return;
    setStatus(false, attempt ? 'Reconnecting…' : 'Connecting…');
    let fresh = true; // the server replays scrollback first, so start from a clean screen
    const sock = ws = new WebSocket(`${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/api/sessions/${id}/ws`);
    sock.binaryType = 'arraybuffer';
    sock.onmessage = (ev) => {
      if (fresh) { term.reset(); fresh = false; }
      if (typeof ev.data !== 'string') { term.write(new Uint8Array(ev.data)); return; }
      const msg = JSON.parse(ev.data);
      if (msg.type === 'ready') {
        attempt = 0;
        hideOverlay();
        setStatus(true, msg.latencyMs ? `Connected · ${msg.latencyMs} ms` : 'Connected');
        fit.fit();
        sendSize();
        term.focus();
        clearInterval(pingTimer);
        pingTimer = setInterval(() => {
          sendJSON({ type: 'ping' });
          clearTimeout(pongTimer);
          pongTimer = setTimeout(() => sock.close(), 8000); // no answer: treat the link as dead
        }, 15000);
      } else if (msg.type === 'pong') {
        clearTimeout(pongTimer);
        setStatus(true, `Connected · ${msg.latencyMs} ms`);
        devices.style.display = msg.attached > 1 ? '' : 'none';
        devices.textContent = `Open on ${msg.attached} devices`;
      }
    };
    sock.onclose = (ev) => {
      clearInterval(pingTimer); clearTimeout(pongTimer);
      if (closed || sock !== ws) return;
      if (ev.code === CLOSE_ENDED) return ended('The shell exited or the connection to the server was lost.');
      retry();
    };
  }

  function retry() {
    if (closed) return;
    attempt++;
    setStatus(false, 'Reconnecting…');
    showOverlay('Reconnecting…', h('small', {}, 'Your session is still running on your Cotta server.'));
    clearTimeout(retryTimer);
    retryTimer = setTimeout(async () => {
      if (closed) return;
      try {
        const list = await api('GET', '/api/sessions'); // a 401 here sends us to the unlock screen
        if (closed) return;
        if (!list.some((s) => s.id === id)) return ended('It is no longer running on your Cotta server.');
        open();
      } catch (e) {
        if (e.status !== 401) retry();
      }
    }, Math.min(5000, 250 * 2 ** attempt));
  }

  function ended(why) {
    closed = true;
    setStatus(false, 'Ended');
    showOverlay('Session ended', h('small', {}, why), h('a', { class: 'btn primary', href: '#/' }, 'Back to servers'));
  }

  // Coming back to the tab (or unlocking the phone) should reattach at once.
  function onVisible() {
    if (document.visibilityState === 'visible' && !closed && ws && ws.readyState > WebSocket.OPEN) {
      attempt = 0;
      retry();
    }
  }
  document.addEventListener('visibilitychange', onVisible);

  async function refreshList() {
    let list;
    try { list = await api('GET', '/api/sessions'); } catch { return false; }
    if (gone) return false;
    state.sessions = list;
    const me = list.find((s) => s.id === id);
    if (me) {
      title.textContent = me.name;
      addr.textContent = me.addr;
      document.title = `${me.name} — Cotta`;
      if (hostId == null) { hostId = me.hostId; loadCmds(); }
      if (!noted && !createdHere.has(id)) {
        noted = true;
        note.replaceChildren(h('span', {}, `Picked up where you left off — this session has been running ${runningFor(me.startedAt)}.`),
          h('button', { type: 'button', onclick: () => { note.style.display = 'none'; } }, 'Dismiss'));
        note.style.display = '';
      }
    }
    tabs.replaceChildren(
      h('a', { class: 'btn ghost icon-only', href: '#/', 'aria-label': 'All servers' }, icon('grid')),
      ...list.map((s) => h('a', { class: 'tab', role: 'tab', href: '#/s/' + s.id, 'aria-current': s.id === id ? 'page' : null, 'aria-selected': String(s.id === id) }, h('span', { class: 'dot' }), s.name)),
      h('a', { class: 'btn ghost icon-only', href: '#/', 'aria-label': 'New session' }, icon('plus')));
    return !!me;
  }
  listTimer = setInterval(refreshList, 5000);

  if (await refreshList()) open();
  else if (!gone) ended('It is no longer running on your Cotta server.');
}

boot();
