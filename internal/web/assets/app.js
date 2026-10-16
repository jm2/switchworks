// SPDX-License-Identifier: GPL-3.0-or-later
// Switchyard web UI: a single page over the coordinator's JSON API and SSE
// streams. No framework and no build step. All text from sessions is put in
// the DOM with textContent, never as HTML.
'use strict';

const ME = 'user:web';

// ---------- small helpers ----------

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k === 'style') el.style.cssText = v; // CSSOM: allowed by the page's CSP
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid === undefined || kid === null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

// fill replaces el's children, flattening arrays and skipping empty values.
function fill(el, ...kids) {
  el.replaceChildren(...kids.flat(Infinity).filter((k) => k !== null && k !== undefined && k !== false));
}

function ago(ts) {
  if (!ts || ts.startsWith('0001')) return '-';
  const s = Math.max(0, (Date.now() - new Date(ts).getTime()) / 1000);
  if (s < 60) return Math.floor(s) + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm';
  if (s < 172800) return Math.floor(s / 3600) + 'h';
  return Math.floor(s / 86400) + 'd';
}

function human(n) {
  n = n || 0;
  if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
  if (n >= 1e4) return Math.round(n / 1e3) + 'k';
  return String(n);
}

function clip(s, n) {
  s = (s || '').replace(/\s+/g, ' ').trim();
  return s.length > n ? s.slice(0, n - 1) + '…' : s;
}

function toast(msg) {
  const t = h('div', {class: 'toast', role: 'status', text: msg});
  document.body.append(t);
  setTimeout(() => t.remove(), 3500);
}

async function api(method, path, body) {
  const opt = {method, credentials: 'same-origin', headers: {'X-Swyd-CSRF': '1'}};
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  const r = await fetch(path, opt);
  if (r.status === 401) {
    location.href = '/login';
    throw new Error('signed out');
  }
  const text = await r.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch (e) { /* not JSON */ }
  if (!r.ok) throw new Error((data && data.error) || r.statusText);
  return data;
}

// A small subset of Markdown, built as DOM: fenced code, inline code, bold.
function md(text) {
  const out = document.createDocumentFragment();
  const parts = (text || '').split(/```/);
  parts.forEach((part, i) => {
    if (i % 2 === 1) {
      const nl = part.indexOf('\n');
      const body = nl >= 0 && /^[\w+-]*$/.test(part.slice(0, nl)) ? part.slice(nl + 1) : part;
      out.append(h('pre', {}, h('code', {text: body.replace(/\n$/, '')})));
      return;
    }
    const re = /(`[^`\n]+`|\*\*[^*\n]+\*\*)/g;
    let last = 0, m;
    while ((m = re.exec(part)) !== null) {
      if (m.index > last) out.append(part.slice(last, m.index));
      const tok = m[0];
      out.append(tok[0] === '`' ? h('code', {text: tok.slice(1, -1)}) : h('strong', {text: tok.slice(2, -2)}));
      last = m.index + tok.length;
    }
    if (last < part.length) out.append(part.slice(last));
  });
  return out;
}

const statusLabel = {
  idle: 'idle', active: 'working', starting: 'starting', waiting_approval: 'needs approval',
  error: 'error', stopped: 'stopped', gone: 'gone', offline: 'offline',
};
const liveSession = (s) => s.status !== 'stopped' && s.status !== 'gone';

// ---------- fleet state, kept current by GET /v1/stream ----------

const S = {hosts: new Map(), sessions: new Map(), messages: new Map(), live: false, ready: false};
let fleetES = null;
let view = null; // the current view: {render(), update(type, data), close()}

function openAsks() {
  let n = 0;
  for (const m of S.messages.values()) {
    if (m.kind === 'ask' && m.to_user && (m.state === 'pending' || m.state === 'delivered')) n++;
  }
  return n;
}

function setConn(live) {
  S.live = live;
  changed('conn');
}

function refreshBadges() {
  const n = openAsks();
  const waiting = [...S.sessions.values()].filter((s) => (s.approvals || []).length).length;
  document.title = (waiting + n ? `(${waiting + n}) ` : '') + 'Switchyard';
  renderSide();
}

let pendingUpdate = new Map();
let updateQueued = false;
function changed(type, data) {
  pendingUpdate.set(type + ':' + (data && data.id), [type, data]);
  if (updateQueued) return;
  updateQueued = true;
  requestAnimationFrame(() => {
    updateQueued = false;
    const batch = [...pendingUpdate.values()];
    pendingUpdate = new Map();
    refreshBadges();
    for (const [t, d] of batch) if (view && view.update) view.update(t, d);
  });
}

function connectFleet() {
  if (fleetES) fleetES.close();
  fleetES = new EventSource('/v1/stream');
  fleetES.addEventListener('hello', (e) => {
    const d = JSON.parse(e.data);
    S.hosts = new Map(d.hosts.map((x) => [x.id, x]));
    S.sessions = new Map(d.sessions.map((x) => [x.id, x]));
    S.messages = new Map(d.messages.map((x) => [x.id, x]));
    S.ready = true;
    setConn(true);
    changed('all');
  });
  for (const type of ['session', 'host', 'message']) {
    fleetES.addEventListener(type, (e) => {
      const d = JSON.parse(e.data);
      ({session: S.sessions, host: S.hosts, message: S.messages})[type].set(d.id, d);
      changed(type, d);
    });
  }
  fleetES.addEventListener('host_removed', (e) => {
    const d = JSON.parse(e.data);
    S.hosts.delete(d.id);
    changed('host', d);
  });
  fleetES.onerror = () => {
    setConn(false);
    if (fleetES.readyState === EventSource.CLOSED) {
      // Closed for good (e.g. signed out): check, then retry.
      api('GET', '/v1/hosts').catch(() => {});
      setTimeout(connectFleet, 3000);
    }
  };
}

// ---------- icons (drawn for Switchyard: 24px grid, 1.6px strokes) ----------

const ICONS = {
  menu: 'M4 7h16M4 12h16M4 17h10',
  close: 'M6 6l12 12M18 6L6 18',
  plus: 'M12 5v14M5 12h14',
  back: 'M19 12H5M11 6l-6 6 6 6',
  send: 'M12 19V5M6 11l6-6 6 6',
  inbox: 'M4 13l2.5-7h11L20 13v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1zM4 13h4.5a3.5 3.5 0 0 0 7 0H20',
  traffic: 'M5 8h13M15 5l3 3-3 3M19 16H6M9 13l-3 3 3 3',
  usage: 'M5 20v-8M12 20V5M19 20v-5',
  settings: 'M4 7h9M17 7h3M4 17h3M11 17h9M15 5v4M9 15v4',
  host: 'M4 5h16v10H4zM9 19h6M12 15v4',
  chevron: 'M7 10l5 5 5-5',
  stop: 'M7 7h10v10H7z',
  pause: 'M9 6v12M15 6v12',
  copy: 'M9 9h10v10H9zM5 15V5h10',
  check: 'M5 12l5 5 9-10',
  terminal: 'M5 7l5 5-5 5M12 17h7',
  edit: 'M14 5l5 5L9 20H4v-5zM12 7l5 5',
  link: 'M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1',
  tool: 'M4 20l7-7M14.5 3.5a4.5 4.5 0 0 0-3.9 6.7L4 16.8 7.2 20l6.6-6.6a4.5 4.5 0 0 0 6.7-3.9l-3 1.5-2.4-2.4z',
  idea: 'M9 18h6M10 21h4M12 3a6 6 0 0 0-3.5 10.9V16h7v-2.1A6 6 0 0 0 12 3z',
  list: 'M9 6h11M9 12h11M9 18h11M4.5 6h.01M4.5 12h.01M4.5 18h.01',
  alert: 'M12 4l9 16H3zM12 10v4M12 17h.01',
  signout: 'M14 4h5v16h-5M10 8l-4 4 4 4M6 12h10',
  moon: 'M20 14.5A8 8 0 1 1 9.5 4a6.5 6.5 0 0 0 10.5 10.5z',
  sun: 'M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8zM12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5.3 5.3l1.4 1.4M17.3 17.3l1.4 1.4M5.3 18.7l1.4-1.4M17.3 6.7l1.4-1.4',
  auto: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM12 3v18',
  reply: 'M10 8L5 12l5 4M5 12h9a5 5 0 0 1 5 5v1',
};

function icon(name, cls) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('class', 'i' + (cls ? ' ' + cls : ''));
  svg.setAttribute('aria-hidden', 'true');
  const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
  path.setAttribute('d', ICONS[name] || '');
  svg.append(path);
  return svg;
}

function iconBtn(name, label, onclick, attrs) {
  return h('button', Object.assign({type: 'button', class: 'icon-btn', 'aria-label': label, title: label, onclick}, attrs || {}), icon(name));
}
function iconLink(name, label, href, extra) {
  return h('a', {class: 'icon-btn', href, 'aria-label': label, title: label}, icon(name), extra || null);
}

// ---------- shell: sidebar, top bar, menus ----------

const narrow = window.matchMedia('(max-width: 899px)');
let current = {tab: '', session: ''};

function hostFilter() {
  try { return localStorage.getItem('swyd-host') || ''; } catch (e) { return ''; }
}
function setHostFilter(v) {
  try { localStorage.setItem('swyd-host', v); } catch (e) { /* per page only */ }
  changed('all');
}

function openDrawer(open) {
  document.body.classList.toggle('menu-open', open);
  document.getElementById('scrim').hidden = !open;
}

function sortedHosts() {
  return [...S.hosts.values()].sort((a, b) => (b.connected - a.connected) || a.id.localeCompare(b.id));
}

function hostSessions(hostID, withStopped) {
  return [...S.sessions.values()].filter((s) => s.host === hostID && (withStopped || liveSession(s)))
    .sort((a, b) => (b.updated_at || '').localeCompare(a.updated_at || ''));
}

// picker: all hosts, or one host, like choosing which machine to look at.
function hostPicker() {
  const cur = hostFilter();
  const sel = h('select', {'aria-label': 'Show hosts', onchange: (e) => setHostFilter(e.target.value)},
    h('option', {value: '', text: 'All hosts'}),
    sortedHosts().map((x) => h('option', {value: x.id, text: x.id + (x.connected ? '' : ' (offline)')})));
  sel.value = S.hosts.has(cur) ? cur : '';
  const host = S.hosts.get(sel.value);
  const anyOnline = [...S.hosts.values()].some((x) => x.connected);
  return h('div', {class: 'picker'},
    h('span', {class: 'dot' + ((host ? host.connected : anyOnline) && S.live ? ' on' : '')}), sel, icon('chevron', 'chev'));
}

function spawnHref(hostID) { return '#/spawn' + (hostID ? '/' + encodeURIComponent(hostID) : ''); }

function renderSide() {
  const asks = openAsks();
  const nav = [['nav-inbox', 'inbox', 'Inbox', asks], ['nav-traffic', 'traffic', 'Traffic', 0], ['nav-usage', 'usage', 'Usage', 0]];
  for (const [id, ic, label, n] of nav) {
    const a = document.getElementById(id);
    fill(a, icon(ic), h('span', {text: label}), n ? h('span', {class: 'count', text: n}) : null);
    a.classList.toggle('on', current.tab === a.dataset.tab);
  }
  fill(document.getElementById('side-new'), icon('plus'), h('span', {text: 'New session'}));
  fill(document.getElementById('side-close'), icon('close'));
  fill(document.getElementById('settings'), icon('settings'), h('span', {text: 'Settings'}));
  fill(document.getElementById('side-picker'), S.ready && S.hosts.size > 1 ? hostPicker() : null);

  const tree = document.getElementById('side-tree');
  if (!S.ready) { fill(tree, h('div', {class: 'none', text: 'Connecting…'})); return; }
  const only = hostFilter();
  const hosts = sortedHosts().filter((x) => !only || x.id === only);
  fill(tree, hosts.length ? hosts.map((host) => {
    const list = hostSessions(host.id, false);
    return h('div', {class: 'host'},
      h('div', {class: 'host-row' + (host.connected ? '' : ' off')},
        icon('host'), h('span', {class: 'name', text: host.id}),
        host.connected ? iconLink('plus', `New session on ${host.id}`, spawnHref(host.id)) : null),
      list.length ? list.map((s) => h('a', {class: 'sess-row' + (s.id === current.session ? ' on' : ''), href: '#/s/' + s.id},
        h('span', {class: 'mark ' + s.status, title: statusLabel[s.status] || s.status}),
        h('span', {class: 'name', text: s.name}),
        h('span', {class: 'age', text: ago(s.updated_at)})))
        : h('div', {class: 'none', text: host.connected ? 'No sessions running' : 'Offline'}));
  }) : h('div', {class: 'none', text: 'No hosts enrolled yet'}));
}

function setTop(...kids) {
  fill(document.getElementById('top'), kids);
}

// The leading control: a menu button on phones (or back, inside a page).
function lead(back) {
  if (back) return h('span', {class: 'only-narrow'}, iconLink('back', 'Back', back));
  return h('span', {class: 'only-narrow'}, iconBtn('menu', 'Menu', () => openDrawer(true)));
}

function crumb(...parts) {
  const out = [];
  parts.filter(Boolean).forEach((p, i, all) => {
    const last = i === all.length - 1;
    if (i > 0) out.push(h('span', {class: 'sep ctx', text: '/'}));
    out.push(typeof p === 'string'
      ? h('span', {class: last ? 'here' : 'ctx', text: p})
      : Object.assign(p, {className: (p.className ? p.className + ' ' : '') + (last ? 'here' : 'ctx')}));
  });
  return h('div', {class: 'crumb'}, out);
}

// Phone-only shortcuts on the top bar (on wide screens they are in the sidebar).
function phoneActions() {
  const asks = openAsks();
  return h('span', {class: 'only-narrow'},
    iconLink('inbox', asks ? `Inbox, ${asks} open` : 'Inbox', '#/inbox', asks ? h('span', {class: 'dot-badge'}) : null),
    iconBtn('settings', 'Settings', (e) => openMenu(e.currentTarget)));
}

// --- settings menu ---

const themes = ['dark', 'light', 'system'];
const themeName = {dark: 'Dark', light: 'Light', system: 'Match system'};
const themeIcon = {dark: 'moon', light: 'sun', system: 'auto'};
// The active theme lives in memory; storage only restores and persists it,
// so the menu still works when the browser blocks storage.
let activeTheme = null;
function storedTheme() {
  if (activeTheme) return activeTheme;
  try { const t = localStorage.getItem('swyd-theme'); return themes.includes(t) ? t : 'dark'; } catch (e) { return 'dark'; }
}
function applyTheme(t) {
  activeTheme = t;
  document.documentElement.dataset.theme = t;
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.content = getComputedStyle(document.documentElement).getPropertyValue('--canvas').trim() || '#141517';
}
applyTheme(storedTheme());

function closeMenu() { document.getElementById('menu').hidden = true; }

function openMenu(anchor) {
  const menu = document.getElementById('menu');
  if (!menu.hidden) { closeMenu(); return; }
  const cur = storedTheme();
  fill(menu,
    h('div', {class: 'menu-label', text: 'Theme'}),
    themes.map((t) => h('button', {type: 'button', role: 'menuitemradio', 'aria-checked': String(t === cur), onclick: () => {
      try { localStorage.setItem('swyd-theme', t); } catch (e) { /* applies to this page only */ }
      applyTheme(t);
      closeMenu();
    }}, icon(themeIcon[t]), h('span', {text: themeName[t]}), t === cur ? h('span', {class: 'tick'}, icon('check')) : null)),
    h('hr'),
    h('button', {type: 'button', role: 'menuitem', onclick: async () => {
      await fetch('/ui/logout', {method: 'POST', credentials: 'same-origin', headers: {'X-Swyd-CSRF': '1'}});
      location.href = '/login';
    }}, icon('signout'), h('span', {text: 'Sign out'})));
  menu.hidden = false;
  const r = anchor.getBoundingClientRect();
  const w = menu.offsetWidth, hgt = menu.offsetHeight;
  menu.style.left = Math.max(8, Math.min(r.left, window.innerWidth - w - 8)) + 'px';
  menu.style.top = (r.bottom + hgt + 8 < window.innerHeight ? r.bottom + 6 : r.top - hgt - 6) + 'px';
}

// ---------- views ----------

function attentionList() {
  const out = [];
  for (const s of [...S.sessions.values()].filter(liveSession)) {
    const n = (s.approvals || []).length;
    if (n) out.push(h('a', {href: '#/s/' + s.id}, h('span', {class: 'mark waiting_approval'}),
      h('span', {class: 'grow', text: `${s.path} is waiting for your approval`}), h('span', {class: 'faint small', text: ago(s.updated_at)})));
  }
  const asks = openAsks();
  if (asks) out.push(h('a', {href: '#/inbox'}, h('span', {class: 'mark waiting_approval'}),
    h('span', {class: 'grow', text: asks === 1 ? 'An agent asked you a question' : `Agents asked you ${asks} questions`})));
  return out.length ? h('div', {class: 'attention'}, out) : null;
}

function fleetView() {
  const main = document.getElementById('main');
  let showStopped = false;
  const starter = newSessionComposer();
  const content = h('div', {class: 'home'});
  let mounted = false;
  function render() {
    if (!mounted) { fill(main, content, starter.dock); mounted = true; }
    setTop(lead(), h('span', {class: 'only-wide grow'}),
      h('span', {class: 'only-narrow grow'}, S.ready ? hostPicker() : null), phoneActions());
    if (!S.ready) {
      fill(content, h('p', {class: 'empty', text: 'Connecting to the coordinator…'}));
      return;
    }
    const only = hostFilter();
    const hosts = sortedHosts().filter((x) => !only || x.id === only);
    const sessions = [...S.sessions.values()];
    const live = sessions.filter(liveSession);
    const working = live.filter((s) => s.status === 'active').length;
    const online = hosts.filter((x) => x.connected).length;
    const tree = hosts.map((host) => {
      const list = hostSessions(host.id, showStopped);
      return h('div', {class: 'host'},
        h('div', {class: 'host-row' + (host.connected ? '' : ' off')},
          icon('host'), h('span', {class: 'name', text: host.id}),
          h('span', {class: 'hostinfo only-wide', text: host.connected
            ? (host.adapters || []).join(', ')
            : `offline, last seen ${ago(host.last_seen)} ago`}),
          host.connected ? iconLink('plus', `New session on ${host.id}`, spawnHref(host.id)) : null),
        list.length ? list.map((s) => h('a', {class: 'sess-row' + (liveSession(s) ? '' : ' stopped'), href: '#/s/' + s.id},
          h('span', {class: 'mark ' + s.status, title: statusLabel[s.status] || s.status}),
          h('span', {class: 'name', text: s.name}),
          h('span', {class: 'age', text: ago(s.updated_at)}),
          s.preview ? h('span', {class: 'preview', text: s.preview}) : null))
          : h('div', {class: 'none', text: host.connected ? 'No sessions running' : 'Offline'}));
    });
    fill(content,
      h('div', {},
        h('div', {class: 'home-head only-wide'}, h('h1', {text: 'Fleet'}),
          h('p', {text: `${live.length} session${live.length === 1 ? '' : 's'} on ${online} host${online === 1 ? '' : 's'} online` +
            (working ? `, ${working} working` : '')})),
        attentionList(),
        hosts.length ? tree : h('p', {class: 'empty', text: 'No hosts yet. Enroll one with swyd node enroll.'}),
        sessions.some((s) => !liveSession(s))
          ? h('p', {class: 'small'}, h('button', {class: 'link', type: 'button', onclick: () => { showStopped = !showStopped; render(); },
            text: showStopped ? 'Hide stopped sessions' : 'Show stopped sessions'}))
          : null));
    starter.refresh();
  }
  return {tab: 'fleet', render, update: () => render()};
}

// The home composer: describe a task, pick where, and start a session.
function newSessionComposer() {
  const input = h('textarea', {rows: 1, placeholder: 'Describe a task to start a new session', 'aria-label': 'First prompt'});
  const hostSel = h('select', {'aria-label': 'Host'});
  const harnessSel = h('select', {'aria-label': 'Harness'});
  const cwd = h('input', {type: 'text', placeholder: 'Folder on the host', 'aria-label': 'Working directory', autocapitalize: 'off', spellcheck: 'false'});
  const send = h('button', {type: 'submit', class: 'send', 'aria-label': 'Start session', title: 'Start session'}, icon('send'));
  const lastCwd = (host) => { try { return localStorage.getItem('swyd-cwd:' + host) || ''; } catch (e) { return ''; } };
  hostSel.addEventListener('change', () => { refreshHarness(); cwd.value = lastCwd(hostSel.value); });
  function refreshHarness() {
    const host = S.hosts.get(hostSel.value);
    const cur = harnessSel.value;
    fill(harnessSel, ((host && host.adapters) || ['codex']).map((a) => h('option', {value: a, text: a})));
    if (cur && [...harnessSel.options].some((o) => o.value === cur)) harnessSel.value = cur;
  }
  function refresh() {
    const online = sortedHosts().filter((x) => x.connected);
    const cur = hostSel.value || hostFilter();
    fill(hostSel, online.map((x) => h('option', {value: x.id, text: x.id})));
    if (online.some((x) => x.id === cur)) hostSel.value = cur;
    if (!cwd.value) cwd.value = lastCwd(hostSel.value);
    refreshHarness();
    send.disabled = online.length === 0;
  }
  input.addEventListener('input', () => { input.style.height = 'auto'; input.style.height = input.scrollHeight + 'px'; });
  input.addEventListener('keydown', (e) => {
    if (e.isComposing || e.keyCode === 229) return; // Enter confirms an IME candidate
    if (e.key === 'Enter' && !e.shiftKey && !narrow.matches) { e.preventDefault(); form.requestSubmit(); }
  });
  const form = h('form', {class: 'composer', onsubmit: async (e) => {
    e.preventDefault();
    const dir = cwd.value.trim();
    if (!dir) { toast('Enter the folder on the host to work in'); cwd.focus(); return; }
    const base = dir.replace(/[\\/]+$/, '').split(/[\\/]/).pop().toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^[-.]+|[-.]+$/g, '').slice(0, 56) || 'session';
    // Several sessions on one folder are common: number them.
    const taken = new Set([...S.sessions.values()].filter((x) => x.host === hostSel.value).map((x) => x.name));
    let name = base;
    for (let i = 2; taken.has(name); i++) name = `${base}-${i}`;
    send.disabled = true;
    try {
      const s = await api('POST', '/v1/sessions', {host: hostSel.value, harness: harnessSel.value, name, cwd: dir, prompt: input.value.trim()});
      try { localStorage.setItem('swyd-cwd:' + hostSel.value, dir); } catch (err) { /* convenience only */ }
      S.sessions.set(s.id, s);
      location.hash = '#/s/' + s.id;
    } catch (err) {
      toast(err.message);
      send.disabled = false;
    }
  }},
  input,
  h('div', {class: 'bar'},
    h('label', {class: 'chip lead', title: 'Host'}, icon('host'), hostSel, icon('chevron', 'chev')),
    h('label', {class: 'chip', title: 'Harness'}, harnessSel, icon('chevron', 'chev')),
    h('label', {class: 'chip', title: 'Working directory'}, cwd),
    h('a', {class: 'more-link', href: spawnHref(''), text: 'More options'}),
    send));
  return {dock: h('div', {class: 'dock'}, form), refresh};
}

// --- session ---

function parseRouted(text) {
  const nl = text.indexOf('\n');
  const head = nl >= 0 ? text.slice(0, nl) : text;
  const m = /^\[switchyard ([^\]]*)\]$/.exec(head);
  if (!m) return null;
  const f = {};
  for (const kv of m[1].split(' ')) {
    const i = kv.indexOf('=');
    if (i > 0) f[kv.slice(0, i)] = kv.slice(i + 1);
  }
  let body = nl >= 0 ? text.slice(nl + 1) : '';
  body = body.replace(/^\((?:.*?is waiting for an answer.*?|This answers your earlier fleet_ask.*?)\)\n/, '');
  return {from: f.from, kind: f.kind, msg: f.msg, re: f.re, body};
}

const defaultDecisions = ['accept', 'acceptForSession', 'decline', 'cancel'];
const decisionLabel = {
  accept: 'Allow once', acceptForSession: 'Allow for this session', acceptWithExecpolicyAmendment: 'Always allow this command',
  decline: 'Decline', declineAlways: 'Always decline', cancel: 'Decline and stop',
};

// Bidirectional overrides and isolates can make text read differently from
// what it is: an approval must show exactly what would run.
const BIDI = /[\u202A-\u202E\u2066-\u2069]/g;
function plain(s) { return (s || '').replace(BIDI, ''); }

function approvalCard(sessionID, a, onDone) {
  const card = h('div', {class: 'approval', role: 'group', 'aria-label': 'Approval'});
  const ask = a.kind === 'file_change' ? 'Allow these file changes?' : a.kind === 'command' ? 'Run this command?' : 'Allow this tool?';
  const what = plain(a.kind === 'file_change' ? (a.reason || a.command || 'file changes') : (a.command || a.kind));
  // Show the usual choice first, and the turn-stopping one last.
  const order = ['accept', 'acceptForSession', 'acceptWithExecpolicyAmendment', 'decline', 'declineAlways', 'cancel'];
  const rank = (d) => (order.indexOf(d) < 0 ? order.length - 1 : order.indexOf(d));
  const decisions = (a.decisions && a.decisions.length ? a.decisions : defaultDecisions).slice().sort((x, y) => rank(x) - rank(y));
  const buttons = decisions.map((d) =>
    h('button', {type: 'button', class: d === 'accept' ? 'primary' : '', text: decisionLabel[d] || d, title: d, onclick: async () => {
      for (const b of card.querySelectorAll('button')) b.disabled = true;
      try {
        await api('POST', `/v1/sessions/${encodeURIComponent(sessionID)}/approvals/${encodeURIComponent(a.approval_id)}`, {decision: d});
        card.classList.add('done');
        if (onDone) onDone();
      } catch (err) {
        toast(err.message);
        for (const b of card.querySelectorAll('button')) b.disabled = false;
      }
    }}));
  fill(card,
    h('div', {class: 'ask'}, icon('alert'), h('span', {text: ask})),
    h('div', {class: 'what', text: what}),
    a.kind !== 'file_change' && a.reason ? h('div', {class: 'small muted', text: plain(a.reason)}) : null,
    a.cwd ? h('div', {class: 'where', title: plain(a.cwd)}, h('span', {dir: 'ltr', text: plain(a.cwd)})) : null,
    a.grant_root ? h('div', {class: 'small muted'}, h('span', {text: '"Allow for this session" also allows writes under '}),
      h('span', {dir: 'ltr', text: plain(a.grant_root)})) : null,
    h('div', {class: 'row'}, buttons));
  return card;
}

function diffPre(text) {
  return h('pre', {class: 'diff'}, text.split('\n').map((l) => h('span', {
    class: l.startsWith('@@') ? 'hunk' : l.startsWith('+') && !l.startsWith('+++') ? 'add' : l.startsWith('-') && !l.startsWith('---') ? 'del' : null,
    text: l + '\n'})));
}

function toolStateText(raw) {
  const code = raw.exitCode === undefined || raw.exitCode === null ? '' : ` (exit ${raw.exitCode})`;
  switch (raw.status) {
    case 'inProgress': return 'running';
    case 'completed': return 'done' + (raw.exitCode ? code : '');
    case 'failed': return 'failed' + code;
    case 'declined': return 'declined';
    case 'interrupted': return 'stopped';
  }
  return raw.status || '';
}

function sessionView(id, full) {
  const main = document.getElementById('main');
  const info = h('details', {class: 'details'});
  const approvals = h('div', {});
  const log = h('div', {class: 'log'});
  const placeholder = h('p', {class: 'empty'});
  const items = new Map(); // item id -> {el, ...}
  let es = null;
  let lastSeq = 0;
  let follow = true;

  const nearBottom = () => window.innerHeight + window.scrollY >= document.body.scrollHeight - 140;
  window.addEventListener('scroll', onScroll, {passive: true});
  function onScroll() { follow = nearBottom(); }
  const scroll = () => { if (follow) window.scrollTo(0, document.body.scrollHeight); };

  // Composer
  const input = h('textarea', {rows: 1, placeholder: 'Message this session', 'aria-label': 'Message'});
  const mode = h('select', {'aria-label': 'How to deliver'},
    h('option', {value: 'queue', text: 'Next turn'}),
    h('option', {value: 'steer', text: 'Steer now'}),
    h('option', {value: 'interrupt', text: 'Interrupt and send'}),
    h('option', {value: 'ask', text: 'Ask, answer to Inbox'}));
  const sendBtn = h('button', {type: 'submit', class: 'send', 'aria-label': 'Send', title: 'Send'}, icon('send'));
  const form = h('form', {class: 'composer', onsubmit: send}, input,
    h('div', {class: 'bar'}, h('label', {class: 'chip', title: 'How to deliver'}, mode, icon('chevron', 'chev')), sendBtn));
  const dock = h('div', {class: 'dock'}, approvals, form);
  input.addEventListener('input', () => { input.style.height = 'auto'; input.style.height = input.scrollHeight + 'px'; });
  input.addEventListener('keydown', (e) => {
    if (e.key !== 'Enter' || e.isComposing || e.keyCode === 229) return;
    if (e.metaKey || e.ctrlKey || (!e.shiftKey && !narrow.matches)) { e.preventDefault(); form.requestSubmit(); }
  });

  async function send(e) {
    e.preventDefault();
    const text = input.value.trim();
    if (!text) return;
    sendBtn.disabled = true;
    try {
      let res;
      if (mode.value === 'ask') {
        res = await api('POST', '/v1/messages', {to: id, kind: 'ask', text, from: ME, wait_seconds: 0});
        toast(res.status === 'failed' ? 'Ask failed: ' + res.error : 'Asked. The answer will appear in Inbox');
      } else {
        res = await api('POST', `/v1/sessions/${encodeURIComponent(id)}/messages`, {text, mode: mode.value, from: ME});
        if (res.state === 'pending') toast('The host is offline. The message will be delivered when it returns');
        else if (mode.value === 'steer' && res.mode === 'queue') toast('Nothing was running, so it starts the next turn');
      }
      input.value = '';
      input.style.height = 'auto';
      follow = true;
    } catch (err) {
      toast(err.message);
    } finally {
      sendBtn.disabled = false;
    }
  }

  function renderHead() {
    const s = S.sessions.get(id);
    if (!s) {
      setTop(lead('#/'), crumb('Session'));
      fill(info);
      if (!placeholder.isConnected) log.append(placeholder);
      placeholder.textContent = S.ready ? 'This session does not exist.' : 'Loading…';
      return;
    }
    placeholder.remove();
    const busy = s.status === 'active' || s.status === 'waiting_approval';
    setTop(lead('#/'),
      h('span', {class: 'mark only-narrow ' + s.status, title: statusLabel[s.status] || s.status}),
      crumb(h('a', {href: '#/', text: s.host}), s.name),
      h('span', {class: 'pill only-wide ' + s.status, text: statusLabel[s.status] || s.status}),
      busy && s.caps && s.caps.interrupt ? iconBtn('pause', 'Interrupt the turn', () =>
        api('POST', `/v1/sessions/${encodeURIComponent(id)}/interrupt`).catch((e) => toast(e.message))) : null,
      liveSession(s) ? iconBtn('stop', 'Stop session', () => {
        if (confirm(`Stop ${s.path}? Its history is kept.`)) api('POST', `/v1/sessions/${encodeURIComponent(id)}/stop`).catch((e) => toast(e.message));
      }) : null);
    input.placeholder = `Message ${s.name}`;
    const parent = s.parent && S.sessions.get(s.parent);
    const kids = [...S.sessions.values()].filter((x) => x.parent === id);
    const u = s.usage;
    const open = info.open;
    const row = (k, v) => v ? [h('dt', {text: k}), h('dd', {}, v)] : null;
    fill(info,
      h('summary', {}, icon('chevron'), h('span', {text: [s.harness, s.model].filter(Boolean).join(', ') + (u ? `, ${human(u.total_tokens)} tokens` : '')})),
      h('dl', {},
        row('Session', s.path), row('Harness', s.harness), row('Model', s.model), row('Folder', s.cwd),
        row('Tokens', u ? `${human(u.input_tokens)} in (${human(u.cached_input_tokens)} cached), ${human(u.output_tokens)} out` : ''),
        row('Started by', parent ? h('a', {href: '#/s/' + parent.id, text: parent.path}) : ''),
        row('Started', kids.length ? kids.map((k, i) => [i ? ', ' : '', h('a', {href: '#/s/' + k.id, text: k.name})]) : ''),
        row('Labels', Object.keys(s.labels || {}).length ? Object.entries(s.labels).map(([k, v]) => h('code', {text: k + '=' + v})) : '')));
    info.open = open;
    fill(approvals, ...(s.approvals || []).map((a) => approvalCard(id, a)));
    form.hidden = !liveSession(s);
  }

  function add(el) { log.append(el); return el; }

  function event(ev) {
    lastSeq = Math.max(lastSeq, ev.seq);
    const p = ev.payload || {};
    switch (ev.kind) {
      case 'session.registered':
        add(h('div', {class: 'sys', text: `${p.resumed ? 'Resumed' : 'Started'}${p.model ? ' with ' + p.model : ''}`}));
        break;
      case 'session.status':
        if (['error', 'stopped', 'gone'].includes(p.status) || p.detail) {
          add(h('div', {class: 'sys' + (p.status === 'error' ? ' bad' : ''), text: (statusLabel[p.status] || p.status) + (p.detail ? ': ' + p.detail : '')}));
        }
        break;
      case 'turn.completed': {
        const secs = p.duration_ms ? ` in ${(p.duration_ms / 1000).toFixed(1)}s` : '';
        const what = {completed: 'Finished', interrupted: 'Interrupted', failed: 'Failed'}[p.status] || p.status;
        add(h('div', {class: 'sys turn' + (p.status === 'failed' ? ' bad' : ''), text: `${what}${secs}${p.error ? ': ' + p.error : ''}`}));
        break;
      }
      case 'item.started':
      case 'item.completed':
        item(ev.kind === 'item.completed', p, ev.ts);
        break;
      case 'item.delta': {
        const it = items.get(p.item_id);
        if (it && it.body) {
          it.text += p.text;
          it.body.textContent = it.text;
        }
        break;
      }
      case 'approval.resolved':
        if (p.decision !== 'cleared') add(h('div', {class: 'sys', text: `Approval: ${decisionLabel[p.decision] || p.decision}`}));
        break;
      case 'message.failed':
        add(h('div', {class: 'sys bad', text: `A message from ${p.from} failed: ${p.error}`}));
        break;
      case 'error':
      case 'warning':
        add(h('div', {class: 'sys bad', text: `${p.message}${p.will_retry ? ' (retrying)' : ''}`}));
        break;
    }
  }

  // toolCard builds (or updates) an outlined card: icon, title, state.
  function toolCard(p, ic, title, mono) {
    let it = items.get(p.item_id);
    if (!it) {
      const state = h('span', {class: 'state'});
      const head = h('div', {class: 'tool-h'}, h('span', {class: 'ico'}, icon(ic)), h('span', {class: 'title' + (mono ? ' mono' : ''), title, text: title}), state);
      const el = h('div', {class: 'tool running'}, head);
      it = {el, state, extra: h('div', {})};
      el.append(it.extra);
      items.set(p.item_id, it);
      add(h('div', {class: 'ev'}, el));
    }
    return it;
  }

  function finishTool(it, raw, sub, detailLabel, detail) {
    const failed = ['failed', 'declined', 'interrupted'].includes(raw.status);
    it.el.className = 'tool' + (raw.status === 'inProgress' ? ' running' : failed ? ' failed' : '');
    it.state.textContent = toolStateText(raw);
    it.state.className = 'state ' + (raw.status || '');
    fill(it.extra,
      sub ? h('div', {class: 'sub', text: clip(sub, 240)}) : null,
      detail ? h('details', {}, h('summary', {text: detailLabel}), typeof detail === 'string' ? h('pre', {text: detail}) : detail) : null);
  }

  function item(done, p, ts) {
    const raw = p.raw || {};
    const known = items.get(p.item_id);
    switch (p.item_type) {
      case 'userMessage': {
        if (done || known) return;
        const text = (raw.content || []).filter((c) => c.type === 'text').map((c) => c.text).join('\n');
        const r = parseRouted(text);
        const el = r
          ? h('div', {class: 'routed'}, h('div', {class: 'who'}, icon('reply'), h('span', {text: r.from}), h('span', {class: 'kind ' + r.kind, text: r.kind})),
            h('div', {class: 'body'}, md(r.body)))
          : h('div', {class: 'user-msg'}, h('div', {class: 'bubble', text}));
        items.set(p.item_id, {el});
        add(h('div', {class: 'ev'}, el));
        break;
      }
      case 'agentMessage': {
        let it = known;
        if (!it) {
          const body = h('div', {class: 'prose'});
          const foot = h('div', {class: 'msg-foot'});
          it = {el: h('div', {}, body, foot), body, foot, text: ''};
          items.set(p.item_id, it);
          add(h('div', {class: 'ev'}, it.el));
        }
        if (done) {
          it.text = p.text || it.text;
          fill(it.body, md(it.text));
          const text = it.text;
          fill(it.foot, ts ? h('span', {text: ago(ts) + ' ago'}) : null,
            iconBtn('copy', 'Copy', async (e) => {
              const b = e.currentTarget;
              try { await navigator.clipboard.writeText(text); fill(b, icon('check')); setTimeout(() => fill(b, icon('copy')), 1500); } catch (err) { toast('Copy is not available here'); }
            }));
        }
        break;
      }
      case 'commandExecution': {
        const it = toolCard(p, 'terminal', raw.command || 'command', true);
        if (done) finishTool(it, raw, '', 'Output', raw.aggregatedOutput ? raw.aggregatedOutput.slice(-8000) : '');
        break;
      }
      case 'mcpToolCall': {
        const it = toolCard(p, 'link', `${raw.server ? raw.server + ': ' : ''}${raw.tool || 'tool'}`);
        if (done) {
          let out = raw.error ? raw.error.message : '';
          if (!out && raw.result) out = (raw.result.content || []).filter((c) => c.type === 'text').map((c) => c.text).join('\n');
          const args = raw.arguments && Object.keys(raw.arguments).length ? JSON.stringify(raw.arguments, null, 2) + '\n\n' : '';
          finishTool(it, raw, out.length <= 240 ? out : '', 'Arguments and result', (args + out) || '');
        }
        break;
      }
      case 'fileChange': {
        const changes = raw.changes || [];
        const paths = changes.map((c) => c.path).join(', ') || 'files';
        const it = toolCard(p, 'edit', changes.length === 1 ? changes[0].path : `${changes.length} files`, true);
        if (done || raw.status) {
          const diff = changes.map((c) => c.diff || '').join('');
          finishTool(it, raw.status ? raw : Object.assign({}, raw, {status: 'completed'}), changes.length > 1 ? paths : '', 'Diff', diff ? diffPre(diff.slice(0, 40000)) : '');
        }
        break;
      }
      case 'toolCall': {
        // Tools of non-Codex harnesses that are not commands, edits or MCP.
        const it = toolCard(p, 'tool', raw.title || raw.tool || 'tool');
        if (done) {
          const out = raw.output || '';
          finishTool(it, raw, out.length <= 240 ? out : '', 'Output', out.length > 240 ? out.slice(-8000) : '');
        }
        break;
      }
      case 'reasoning': {
        if (!done || !p.text) return;
        add(h('div', {class: 'ev'}, h('details', {class: 'tool thinking'},
          h('summary', {class: 'tool-h'}, h('span', {class: 'ico'}, icon('idea')), h('span', {class: 'title', text: 'Thinking'})),
          h('pre', {text: p.text}))));
        break;
      }
      case 'plan': {
        if (!done) return;
        const mark = {completed: '✓', in_progress: '•', pending: '○'};
        add(h('div', {class: 'ev'}, h('div', {class: 'tool plan'},
          h('div', {class: 'tool-h'}, h('span', {class: 'ico'}, icon('list')), h('span', {class: 'title', text: 'Plan'})),
          h('ul', {}, (raw.entries || []).map((e) => h('li', {class: e.status}, h('span', {text: mark[e.status] || '○'}), h('span', {text: e.content})))))));
        break;
      }
    }
  }

  function connect() {
    if (es) es.close();
    const s = S.sessions.get(id);
    let after = lastSeq;
    if (!after && s && !full) after = Math.max(0, s.last_seq - 400);
    if (after > 0 && !lastSeq) {
      log.append(h('p', {class: 'more'}, h('a', {href: `#/s/${id}/all`, text: 'Show earlier history'})));
    }
    es = new EventSource(`/v1/sessions/${encodeURIComponent(id)}/stream?after=${after}`);
    const kinds = ['session.registered', 'session.status', 'turn.started', 'turn.completed', 'item.started', 'item.delta',
      'item.completed', 'turn.diff', 'approval.requested', 'approval.resolved', 'message.queued', 'message.delivered',
      'message.failed', 'usage', 'error', 'warning', 'session.settled'];
    for (const k of kinds) {
      es.addEventListener(k, (e) => {
        // EventSource's own connection errors share the name "error" but
        // carry no data; the stream reconnects by itself.
        if (!e.data) return;
        const ev = JSON.parse(e.data);
        if (ev.seq <= lastSeq) return;
        event(ev);
        scroll();
      });
    }
  }

  function render() {
    fill(main, info, log, dock);
    renderHead();
    if (S.ready && !es) connect();
  }
  return {
    tab: 'fleet', session: id, render,
    update: (t, d) => {
      if (t === 'all' || t === 'tick' || (t === 'session' && d && (d.id === id || d.parent === id)) || t === 'host' || t === 'message') renderHead();
      if (S.ready && !es) connect();
    },
    close: () => { if (es) es.close(); window.removeEventListener('scroll', onScroll); },
  };
}

// --- inbox ---

function sessionLink(path, sid) {
  return sid && S.sessions.has(sid) ? h('a', {href: '#/s/' + sid, text: path}) : h('span', {text: path});
}

function pageTop(title) {
  setTop(lead(), crumb(title), phoneActions());
}

function inboxView() {
  const main = document.getElementById('main');
  const drafts = new Map(); // ask id -> text being typed, kept across re-renders
  function render() {
    pageTop('Inbox');
    const all = [...S.messages.values()].filter((m) => m.to_user).sort((a, b) => b.created_at.localeCompare(a.created_at));
    const open = all.filter((m) => m.kind === 'ask' && (m.state === 'pending' || m.state === 'delivered'));
    const rest = all.filter((m) => !open.includes(m));
    const focused = document.activeElement && document.activeElement.dataset && document.activeElement.dataset.ask;
    fill(main,
      h('h1', {text: 'Inbox'}),
      h('h2', {text: 'Questions from agents'}),
      open.length ? h('div', {class: 'list'}, open.map((m) => {
        const box = h('textarea', {rows: 2, placeholder: 'Your answer', 'data-ask': m.id, 'aria-label': 'Answer'});
        box.value = drafts.get(m.id) || '';
        box.addEventListener('input', () => drafts.set(m.id, box.value));
        const btn = h('button', {type: 'submit', class: 'primary', text: 'Send answer'});
        const form = h('form', {class: 'reply-box', onsubmit: async (e) => {
          e.preventDefault();
          if (!box.value.trim()) return;
          btn.disabled = true;
          try {
            await api('POST', `/v1/messages/${encodeURIComponent(m.id)}/reply`, {text: box.value.trim(), from: ME});
            drafts.delete(m.id);
            toast('Answer sent to ' + m.from);
          } catch (err) {
            toast(err.message);
            btn.disabled = false;
          }
        }}, box, h('div', {class: 'row'}, h('span', {class: 'grow'}), btn));
        const el = h('div', {class: 'item'},
          h('div', {class: 'head'}, sessionLink(m.from, m.from_session), h('span', {text: 'asks ' + m.to_user.replace(/^user:/, '')}),
            h('span', {class: 'grow'}), h('span', {class: 'faint', text: ago(m.created_at) + ' ago'})),
          h('div', {class: 'text', text: m.text}), form);
        if (focused === m.id) setTimeout(() => box.focus(), 0);
        return el;
      })) : h('p', {class: 'empty', text: 'No questions waiting for you.'}),
      h('h2', {text: 'Earlier messages to people'}),
      rest.length ? h('div', {class: 'list'}, rest.slice(0, 50).map(messageItem)) : h('p', {class: 'empty', text: 'None yet.'}));
  }
  return {tab: 'inbox', render, update: (t) => { if (t === 'message' || t === 'all') render(); }};
}

function messageItem(m) {
  const el = h('div', {class: 'item clip', onclick: () => el.classList.toggle('clip')},
    h('div', {class: 'head'},
      h('span', {class: 'kind-tag ' + m.kind, text: m.kind}),
      sessionLink(m.from, m.from_session), h('span', {class: 'faint', text: 'to'}),
      sessionLink(m.to_path || m.to, m.to_session),
      h('span', {class: 'grow'}),
      h('span', {class: 'state ' + m.state, text: m.state + (m.inline ? ', inline' : '')}),
      h('span', {class: 'faint', text: ago(m.created_at)})),
    h('div', {class: 'text', text: m.text}),
    m.error ? h('div', {class: 'small error', text: m.error}) : null);
  return el;
}

// --- traffic ---

function trafficView() {
  const main = document.getElementById('main');
  const filter = h('input', {class: 'filter', type: 'search', placeholder: 'Filter by session, kind or text', 'aria-label': 'Filter'});
  const list = h('div', {class: 'list'});
  filter.addEventListener('input', refill);
  function refill() {
    const q = filter.value.toLowerCase();
    const msgs = [...S.messages.values()].sort((a, b) => b.created_at.localeCompare(a.created_at))
      .filter((m) => !q || [m.kind, m.from, m.to, m.to_path, m.text, m.state].join(' ').toLowerCase().includes(q));
    fill(list, ...(msgs.length ? msgs.slice(0, 200).map(messageItem) : [h('p', {class: 'empty', text: q ? 'No messages match.' : 'No messages between agents yet.'})]));
  }
  function render() {
    pageTop('Traffic');
    fill(main, h('h1', {text: 'Traffic'}), h('p', {class: 'muted', text: 'Every message routed between agents and people.'}), filter, list);
    refill();
  }
  return {tab: 'traffic', render, update: (t) => { if (t === 'message' || t === 'all') refill(); }};
}

// --- usage ---

function usageView() {
  const main = document.getElementById('main');
  let timer = null;
  async function render() {
    pageTop('Usage');
    let u;
    try {
      u = await api('GET', '/v1/usage');
    } catch (e) {
      fill(main, h('p', {class: 'empty error', text: e.message}));
      return;
    }
    const table = (title, m) => {
      const rows = Object.entries(m || {}).sort((a, b) => b[1].total_tokens - a[1].total_tokens);
      return h('table', {},
        h('thead', {}, h('tr', {}, h('th', {text: title}), h('th', {class: 'num', text: 'In'}), h('th', {class: 'num', text: 'Cached'}),
          h('th', {class: 'num', text: 'Out'}), h('th', {class: 'num', text: 'Total'}))),
        h('tbody', {}, rows.map(([k, v]) => h('tr', {}, h('td', {text: k}), h('td', {class: 'num', text: human(v.input_tokens)}),
          h('td', {class: 'num', text: human(v.cached_input_tokens)}), h('td', {class: 'num', text: human(v.output_tokens)}),
          h('td', {class: 'num', text: human(v.total_tokens)})))));
    };
    const days = [];
    for (let i = 13; i >= 0; i--) {
      const d = new Date(Date.now() - i * 86400000).toISOString().slice(0, 10);
      days.push([d, (u.daily || {})[d] || 0]);
    }
    const max = Math.max(1, ...days.map((d) => d[1]));
    const pct = u.max_daily_tokens ? Math.min(100, 100 * u.today_tokens / u.max_daily_tokens) : 0;
    const top = [...S.sessions.values()].filter((s) => s.usage).sort((a, b) => b.usage.total_tokens - a.usage.total_tokens).slice(0, 10);
    fill(main,
      h('h1', {text: 'Usage'}),
      h('h2', {text: `Today (${u.today} UTC)`}),
      h('div', {}, h('span', {class: 'big', text: human(u.today_tokens)}), h('span', {class: 'muted', text: ' tokens' +
        (u.max_daily_tokens ? ` of the ${human(u.max_daily_tokens)} daily budget` : ', no daily budget set')})),
      u.max_daily_tokens ? h('div', {class: 'bar-meter' + (pct >= 100 ? ' over' : '')}, h('span', {style: `width:${pct}%`})) : null,
      h('h2', {text: 'Last 14 days'}),
      h('div', {class: 'days', 'aria-label': 'Tokens per day'}, days.map(([d, n]) =>
        h('div', {style: `height:${Math.max(2, 100 * n / max)}%`, title: `${d}: ${human(n)}`}))),
      h('h2', {text: 'By host'}), table('Host', u.by_host),
      h('h2', {text: 'By model'}), table('Model', u.by_model),
      h('h2', {text: 'Top sessions'}),
      h('table', {}, h('tbody', {}, top.map((s) => h('tr', {},
        h('td', {}, h('a', {href: '#/s/' + s.id, text: s.path})), h('td', {class: 'num', text: human(s.usage.total_tokens)}))))));
  }
  timer = setInterval(() => { if (document.visibilityState === 'visible') render(); }, 30000);
  return {tab: 'usage', render, close: () => clearInterval(timer)};
}

// --- spawn: every option ---

function spawnView(presetHost) {
  const main = document.getElementById('main');
  const f = {
    host: h('select', {id: 'f-host', required: true}),
    harness: h('select', {id: 'f-harness'}),
    name: h('input', {id: 'f-name', placeholder: 'The folder name', autocapitalize: 'off', spellcheck: 'false'}),
    cwd: h('input', {id: 'f-cwd', required: true, placeholder: '~/src/project', autocapitalize: 'off', spellcheck: 'false'}),
    model: h('input', {id: 'f-model', placeholder: "The host's default", autocapitalize: 'off', spellcheck: 'false'}),
    labels: h('input', {id: 'f-labels', placeholder: 'role=reviewer, team=ios', autocapitalize: 'off', spellcheck: 'false'}),
    approval: h('select', {id: 'f-approval'}, ['', 'never', 'on-request', 'untrusted'].map((v) => h('option', {value: v, text: v || "The host's default"}))),
    sandbox: h('select', {id: 'f-sandbox'}, ['', 'read-only', 'workspace-write', 'danger-full-access'].map((v) => h('option', {value: v, text: v || "The host's default"}))),
    prompt: h('textarea', {id: 'f-prompt', placeholder: 'What should it do? You can also leave this empty and message it later.'}),
  };
  const btn = h('button', {type: 'submit', class: 'primary', text: 'Start session'});
  function fillHosts() {
    const cur = f.host.value || presetHost || hostFilter();
    const online = sortedHosts().filter((x) => x.connected);
    fill(f.host, ...online.map((x) => h('option', {value: x.id, text: x.id})));
    if (cur && online.some((x) => x.id === cur)) f.host.value = cur;
    fillHarness();
    btn.disabled = online.length === 0;
  }
  function fillHarness() {
    const host = S.hosts.get(f.host.value);
    const cur = f.harness.value;
    fill(f.harness, ...((host && host.adapters) || ['codex']).map((a) => h('option', {value: a, text: a})));
    if (cur) f.harness.value = cur;
  }
  f.host.addEventListener('change', fillHarness);
  async function submit(e) {
    e.preventDefault();
    const labels = {};
    for (const kv of f.labels.value.split(',')) {
      const [k, ...v] = kv.split('=');
      if (k.trim()) labels[k.trim()] = v.join('=').trim();
    }
    let name = f.name.value.trim();
    if (!name) {
      name = f.cwd.value.replace(/[\\/]+$/, '').split(/[\\/]/).pop().toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^[-.]+|[-.]+$/g, '') || 'session';
    }
    btn.disabled = true;
    try {
      const s = await api('POST', '/v1/sessions', {
        host: f.host.value, harness: f.harness.value, name, cwd: f.cwd.value.trim(), model: f.model.value.trim(),
        prompt: f.prompt.value.trim(), labels, approval_policy: f.approval.value, sandbox: f.sandbox.value,
      });
      try { localStorage.setItem('swyd-cwd:' + f.host.value, f.cwd.value.trim()); } catch (err) { /* convenience only */ }
      S.sessions.set(s.id, s);
      location.hash = '#/s/' + s.id;
    } catch (err) {
      toast(err.message);
      btn.disabled = false;
    }
  }
  const field = (label, el, hint) => [h('label', {for: el.id, text: label}), el, hint ? h('div', {class: 'hint', text: hint}) : null];
  function render() {
    setTop(lead('#/'), crumb('New session'), phoneActions());
    fill(main, h('h1', {text: 'New session'}),
      h('form', {class: 'stack', onsubmit: submit},
        field('Host', f.host), field('Harness', f.harness), field('Folder on that host', f.cwd),
        field('Name', f.name), field('Model', f.model), field('Labels', f.labels, 'Other agents can address sessions by label, like @role=reviewer.'),
        field('Approval policy', f.approval), field('Sandbox', f.sandbox, 'Codex and agy only.'), field('First prompt', f.prompt),
        h('div', {class: 'row'}, h('a', {href: '#/', text: 'Cancel'}), h('span', {class: 'grow'}), btn)));
    fillHosts();
  }
  return {tab: 'spawn', render, update: (t) => { if (t === 'host' || t === 'all') fillHosts(); }};
}

// ---------- routing ----------

function route() {
  if (view && view.close) view.close();
  closeMenu();
  openDrawer(false);
  const parts = location.hash.replace(/^#\/?/, '').split('/');
  switch (parts[0]) {
    case 's': view = sessionView(decodeURIComponent(parts[1] || ''), parts[2] === 'all'); break;
    case 'inbox': view = inboxView(); break;
    case 'traffic': view = trafficView(); break;
    case 'usage': view = usageView(); break;
    case 'spawn': view = spawnView(decodeURIComponent(parts[1] || '')); break;
    default: view = fleetView();
  }
  current = {tab: view.tab, session: view.session || ''};
  window.scrollTo(0, 0);
  renderSide();
  view.render();
}

document.addEventListener('DOMContentLoaded', () => {
  applyTheme(storedTheme());
  document.getElementById('settings').addEventListener('click', (e) => openMenu(e.currentTarget));
  document.getElementById('side-close').addEventListener('click', () => openDrawer(false));
  document.getElementById('scrim').addEventListener('click', () => openDrawer(false));
  document.addEventListener('click', (e) => {
    const menu = document.getElementById('menu');
    if (!menu.hidden && !menu.contains(e.target) && !e.target.closest('[aria-label="Settings"], #settings')) closeMenu();
  });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') { closeMenu(); openDrawer(false); } });
  window.addEventListener('scroll', () => document.getElementById('top').classList.toggle('scrolled', window.scrollY > 4), {passive: true});
  window.addEventListener('hashchange', route);
  connectFleet();
  route();
  // Relative times go stale; redraw list views now and then.
  setInterval(() => {
    if (document.visibilityState === 'visible' && view && view.tab !== 'usage' && !document.activeElement.matches('textarea, input, select')) {
      changed('tick');
    }
  }, 30000);
});
