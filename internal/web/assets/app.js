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
const st = (s) => h('span', {class: 'st ' + s, text: statusLabel[s] || s});
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
  document.getElementById('conn').classList.toggle('live', live);
}

function refreshBadges() {
  const n = openAsks();
  const b = document.getElementById('inbox-badge');
  b.hidden = n === 0;
  b.textContent = n;
  const waiting = [...S.sessions.values()].filter((s) => (s.approvals || []).length).length;
  document.title = (waiting + n ? `(${waiting + n}) ` : '') + 'Switchyard';
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
  fleetES.onerror = () => {
    setConn(false);
    if (fleetES.readyState === EventSource.CLOSED) {
      // Closed for good (e.g. signed out): check, then retry.
      api('GET', '/v1/hosts').catch(() => {});
      setTimeout(connectFleet, 3000);
    }
  };
}

// ---------- views ----------

function sessionRow(s) {
  const approvals = (s.approvals || []).length;
  return h('a', {class: 'sess', href: '#/s/' + s.id},
    h('div', {class: 'row'},
      h('strong', {class: 'grow', text: s.name}),
      approvals && s.status !== 'waiting_approval' ? h('span', {class: 'badge', text: approvals === 1 ? 'approval' : approvals + ' approvals'}) : null,
      st(s.status)),
    h('div', {class: 'row small muted'},
      h('span', {class: 'grow', text: [s.harness, s.model].filter(Boolean).join(' · ')}),
      h('span', {class: 'meta', text: (s.usage ? human(s.usage.total_tokens) + ' tok · ' : '') + ago(s.updated_at)})),
    s.preview ? h('div', {class: 'preview', text: s.preview}) : null);
}

function fleetView() {
  const main = document.getElementById('main');
  let showStopped = false;
  function render() {
    if (!S.ready) {
      fill(main, h('p', {class: 'empty', text: 'Connecting…'}));
      return;
    }
    const sessions = [...S.sessions.values()];
    const live = sessions.filter(liveSession);
    const working = live.filter((s) => s.status === 'active').length;
    const approvals = live.filter((s) => (s.approvals || []).length).length;
    const asks = openAsks();
    const hosts = [...S.hosts.values()].sort((a, b) => (b.connected - a.connected) || a.id.localeCompare(b.id));
    const cards = hosts.map((host) => {
      const list = sessions.filter((s) => s.host === host.id && (showStopped || liveSession(s)))
        .sort((a, b) => a.name.localeCompare(b.name));
      return h('section', {class: 'card'},
        h('div', {class: 'card-h'},
          h('span', {class: 'dot' + (host.connected ? ' on' : '')}),
          h('span', {class: 'name grow', text: host.id}),
          h('span', {class: 'small muted', text: host.connected
            ? [host.os && host.os + '/' + host.arch, (host.adapters || []).join(', ')].filter(Boolean).join(' · ')
            : 'offline · seen ' + ago(host.last_seen) + ' ago'})),
        list.length ? list.map(sessionRow) : h('div', {class: 'sess muted small', text: 'No sessions'}));
    });
    fill(main, 
      h('div', {class: 'summary'},
        h('span', {class: 'chip', text: `${live.length} sessions · ${working} working`}),
        approvals ? h('span', {class: 'chip alert', text: `${approvals} waiting for approval`}) : null,
        asks ? h('a', {class: 'chip alert', href: '#/inbox', text: `${asks} question${asks > 1 ? 's' : ''} for you`}) : null),
      hosts.length ? cards : h('p', {class: 'empty', text: 'No hosts yet. Enroll one with swyd node enroll.'}),
      sessions.some((s) => !liveSession(s))
        ? h('p', {class: 'small'}, h('button', {class: 'link', type: 'button', onclick: () => { showStopped = !showStopped; render(); },
          text: showStopped ? 'Hide stopped sessions' : 'Show stopped sessions'}))
        : null,
      h('a', {class: 'fab', href: '#/spawn', text: '+ New session'}));
  }
  return {tab: 'fleet', render, update: (t) => { if (t !== 'message') render(); }};
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
  decline: 'Decline', declineAlways: 'Always decline', cancel: 'Decline and stop the turn',
};

function approvalCard(sessionID, a, onDone) {
  const card = h('div', {class: 'approval'});
  const what = a.kind === 'file_change' ? `File changes${a.reason ? ': ' + a.reason : ''}` : (a.command || a.kind);
  const buttons = (a.decisions && a.decisions.length ? a.decisions : defaultDecisions).map((d) =>
    h('button', {type: 'button', class: d.startsWith('accept') ? '' : 'secondary', text: decisionLabel[d] || d, title: d, onclick: async (e) => {
      for (const b of card.querySelectorAll('button')) b.disabled = true;
      try {
        await api('POST', `/v1/sessions/${sessionID}/approvals/${encodeURIComponent(a.approval_id)}`, {decision: d});
        card.classList.add('done');
        if (onDone) onDone();
      } catch (err) {
        toast(err.message);
        for (const b of card.querySelectorAll('button')) b.disabled = false;
      }
    }}));
  fill(card,
    h('div', {class: 'k', text: 'Approval needed'}),
    h('div', {class: 'what', text: what}),
    a.reason ? h('div', {class: 'small muted', text: a.reason}) : null,
    a.cwd ? h('div', {class: 'small muted cwd', text: 'in ' + a.cwd}) : null,
    h('div', {class: 'row'}, buttons));
  return card;
}

function sessionView(id, full) {
  const main = document.getElementById('main');
  const head = h('div', {class: 'sess-head'});
  const bar = h('div', {});
  const log = h('div', {class: 'log'});
  const items = new Map(); // item id -> {el, text}
  let es = null;
  let lastSeq = 0;
  let follow = true;

  const nearBottom = () => window.innerHeight + window.scrollY >= document.body.scrollHeight - 120;
  window.addEventListener('scroll', onScroll, {passive: true});
  function onScroll() { follow = nearBottom(); }
  const scroll = () => { if (follow) window.scrollTo(0, document.body.scrollHeight); };

  // Composer
  const input = h('textarea', {rows: 2, placeholder: 'Message this session…', 'aria-label': 'Message'});
  const mode = h('select', {'aria-label': 'Delivery'},
    h('option', {value: 'queue', text: 'Queue'}),
    h('option', {value: 'steer', text: 'Steer'}),
    h('option', {value: 'interrupt', text: 'Interrupt'}),
    h('option', {value: 'ask', text: 'Ask (answer → Inbox)'}));
  const sendBtn = h('button', {type: 'submit', text: 'Send'});
  const form = h('form', {onsubmit: send}, input,
    h('div', {class: 'row'}, mode, h('span', {class: 'grow'}), sendBtn));
  // Pending approvals sit with the composer, at the bottom, within reach.
  const composer = h('div', {class: 'composer'}, bar, form);
  input.addEventListener('input', () => { input.style.height = 'auto'; input.style.height = input.scrollHeight + 'px'; });
  input.addEventListener('keydown', (e) => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) send(e); });

  async function send(e) {
    e.preventDefault();
    const text = input.value.trim();
    if (!text) return;
    sendBtn.disabled = true;
    try {
      let res;
      if (mode.value === 'ask') {
        res = await api('POST', '/v1/messages', {to: id, kind: 'ask', text, from: ME, wait_seconds: 0});
        toast(res.status === 'failed' ? 'Ask failed: ' + res.error : 'Asked; the answer will appear in Inbox');
      } else {
        res = await api('POST', `/v1/sessions/${id}/messages`, {text, mode: mode.value, from: ME});
        if (res.state === 'pending') toast('Host is offline; the message will be delivered when it returns');
        else if (mode.value === 'steer' && res.mode === 'queue') toast('Nothing was running, so it was queued');
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
      fill(head, h('a', {href: '#/', text: '‹ Fleet'}), h('p', {class: 'empty', text: S.ready ? 'No such session.' : 'Loading…'}));
      return;
    }
    const parent = s.parent && S.sessions.get(s.parent);
    const kids = [...S.sessions.values()].filter((x) => x.parent === id);
    const u = s.usage;
    const busy = s.status === 'active' || s.status === 'waiting_approval';
    fill(head, 
      h('div', {class: 'row'},
        h('a', {href: '#/', text: '‹ Fleet'}),
        h('span', {class: 'grow'}),
        busy && s.caps && s.caps.interrupt ? h('button', {type: 'button', class: 'secondary', text: 'Interrupt', onclick: () =>
          api('POST', `/v1/sessions/${id}/interrupt`).catch((e) => toast(e.message))}) : null,
        liveSession(s) ? h('button', {type: 'button', class: 'danger', text: 'Stop', onclick: () => {
          if (confirm(`Stop ${s.path}? Its history is kept.`)) api('POST', `/v1/sessions/${id}/stop`).catch((e) => toast(e.message));
        }}) : null),
      h('div', {class: 'row'}, h('span', {class: 'path grow', text: s.path}), st(s.status)),
      h('div', {class: 'meta', text: [s.harness, s.model].filter(Boolean).join(' · ')}),
      s.cwd ? h('div', {class: 'meta cwd', title: s.cwd, text: s.cwd}) : null,
      u ? h('div', {class: 'meta', text: `tokens: ${human(u.input_tokens)} in (${human(u.cached_input_tokens)} cached) · ${human(u.output_tokens)} out`}) : null,
      parent || kids.length || (s.labels && Object.keys(s.labels).length)
        ? h('div', {class: 'meta chips'},
          parent ? ['parent ', h('a', {href: '#/s/' + parent.id, text: parent.path}), ' '] : null,
          kids.length ? ['children ', kids.map((k) => h('a', {href: '#/s/' + k.id, text: k.name}))] : null,
          Object.entries(s.labels || {}).map(([k, v]) => h('code', {text: k + '=' + v})))
        : null);
    fill(bar, ...(s.approvals || []).map((a) => approvalCard(id, a)));
    form.hidden = !liveSession(s);
  }

  function add(el) { log.append(el); return el; }

  function event(ev) {
    lastSeq = Math.max(lastSeq, ev.seq);
    const p = ev.payload || {};
    switch (ev.kind) {
      case 'session.registered':
        add(h('div', {class: 'sys', text: `${p.resumed ? 'resumed' : 'started'} · ${p.model || ''}`}));
        break;
      case 'session.status':
        if (['error', 'stopped', 'gone'].includes(p.status) || p.detail) {
          add(h('div', {class: 'sys' + (p.status === 'error' ? ' bad' : ''), text: p.status + (p.detail ? ': ' + p.detail : '')}));
        }
        break;
      case 'turn.completed': {
        const secs = p.duration_ms ? ` in ${(p.duration_ms / 1000).toFixed(1)}s` : '';
        add(h('div', {class: 'sys' + (p.status === 'failed' ? ' bad' : ''), text: `turn ${p.status}${secs}${p.error ? ': ' + p.error : ''}`}));
        break;
      }
      case 'session.settled':
        add(h('div', {class: 'sys', text: '✓ all queued work done'}));
        break;
      case 'item.started':
      case 'item.completed':
        item(ev.kind === 'item.completed', p);
        break;
      case 'item.delta': {
        const it = items.get(p.item_id);
        if (it) {
          it.text += p.text;
          it.body.textContent = it.text;
        }
        break;
      }
      case 'approval.requested':
        add(h('div', {class: 'sys bad', text: `approval requested: ${clip(p.kind === 'file_change' ? 'file changes' : p.command, 120)}`}));
        break;
      case 'approval.resolved':
        add(h('div', {class: 'sys', text: `approval: ${decisionLabel[p.decision] || p.decision}`}));
        break;
      case 'message.failed':
        add(h('div', {class: 'sys bad', text: `message from ${p.from} failed: ${p.error}`}));
        break;
      case 'error':
      case 'warning':
        add(h('div', {class: 'sys bad', text: `${ev.kind}: ${p.message}${p.will_retry ? ' (retrying)' : ''}`}));
        break;
    }
  }

  function item(done, p) {
    const raw = p.raw || {};
    const known = items.get(p.item_id);
    switch (p.item_type) {
      case 'userMessage': {
        if (done || known) return;
        const text = (raw.content || []).filter((c) => c.type === 'text').map((c) => c.text).join('\n');
        const r = parseRouted(text);
        const el = r
          ? h('div', {class: 'bubble routed'}, h('span', {class: 'who'}, r.from, ' · ', h('span', {class: 'k ' + r.kind, text: r.kind})), md(r.body))
          : h('div', {class: 'bubble user', text});
        items.set(p.item_id, {el});
        add(h('div', {class: 'ev'}, el));
        break;
      }
      case 'agentMessage': {
        let it = known;
        if (!it) {
          const body = h('span', {});
          const el = h('div', {class: 'bubble agent'}, body);
          it = {el, body, text: ''};
          items.set(p.item_id, it);
          add(h('div', {class: 'ev'}, el));
        }
        if (done) {
          it.text = p.text || it.text;
          fill(it.body, md(it.text));
        }
        break;
      }
      case 'commandExecution': {
        let it = known;
        if (!it) {
          const el = h('div', {class: 'block'}, h('div', {text: '$ ' + (raw.command || '')}));
          it = {el};
          items.set(p.item_id, it);
          add(h('div', {class: 'ev'}, el));
        }
        if (done) {
          const code = raw.exitCode === undefined || raw.exitCode === null ? '' : ` (exit ${raw.exitCode})`;
          it.el.append(h('div', {class: 'res', text: `└ ${raw.status || 'done'}${code}`}));
          if (raw.aggregatedOutput) {
            it.el.append(h('details', {}, h('summary', {text: 'output'}), h('pre', {text: raw.aggregatedOutput.slice(-8000)})));
          }
        }
        break;
      }
      case 'mcpToolCall': {
        let it = known;
        if (!it) {
          const args = raw.arguments ? JSON.stringify(raw.arguments) : '';
          const el = h('div', {class: 'block tool'}, h('div', {text: `⇄ ${raw.server}.${raw.tool} ${clip(args, 200)}`}));
          it = {el};
          items.set(p.item_id, it);
          add(h('div', {class: 'ev'}, el));
        }
        if (done) {
          let out = raw.error ? raw.error.message : '';
          if (!out && raw.result) out = (raw.result.content || []).filter((c) => c.type === 'text').map((c) => c.text).join('\n');
          it.el.append(h('div', {class: 'res', text: `└ ${raw.status}: ${clip(out, 240)}`}));
          if (out.length > 240) it.el.append(h('details', {}, h('summary', {text: 'full result'}), h('pre', {text: out})));
        }
        break;
      }
      case 'fileChange': {
        if (!done) return;
        const changes = raw.changes || [];
        const paths = changes.map((c) => c.path).join(', ');
        const el = h('div', {class: 'block', text: `✎ ${raw.status || ''}: ${paths}`});
        const diff = changes.map((c) => c.diff || '').join('');
        if (diff) el.append(h('details', {}, h('summary', {text: 'diff'}), h('pre', {text: diff.slice(0, 20000)})));
        add(h('div', {class: 'ev'}, el));
        break;
      }
      case 'toolCall': {
        // Tools of non-Codex harnesses that are not commands, edits or MCP.
        let it = known;
        if (!it) {
          const el = h('div', {class: 'block tool'}, h('div', {text: `⚙ ${raw.title || raw.tool || 'tool'}`}));
          it = {el};
          items.set(p.item_id, it);
          add(h('div', {class: 'ev'}, el));
        }
        if (done) {
          const out = raw.output || '';
          it.el.append(h('div', {class: 'res', text: `└ ${raw.status}${out ? ': ' + clip(out, 240) : ''}`}));
          if (out.length > 240) it.el.append(h('details', {}, h('summary', {text: 'full output'}), h('pre', {text: out.slice(-8000)})));
        }
        break;
      }
      case 'reasoning': {
        if (!done || !p.text) return;
        add(h('div', {class: 'ev'}, h('details', {class: 'block'}, h('summary', {text: 'thinking'}), h('pre', {text: p.text}))));
        break;
      }
      case 'plan': {
        if (!done) return;
        const mark = {completed: '✓', in_progress: '▸', pending: '·'};
        const lines = (raw.entries || []).map((e) => `${mark[e.status] || '·'} ${e.content}`).join('\n');
        add(h('div', {class: 'ev'}, h('div', {class: 'block'}, h('div', {text: 'plan'}), h('pre', {text: lines}))));
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
      log.append(h('p', {class: 'small'}, h('a', {href: `#/s/${id}/all`, text: 'Show the whole history'})));
    }
    es = new EventSource(`/v1/sessions/${id}/stream?after=${after}`);
    const kinds = ['session.registered', 'session.status', 'turn.started', 'turn.completed', 'item.started', 'item.delta',
      'item.completed', 'turn.diff', 'approval.requested', 'approval.resolved', 'message.queued', 'message.delivered',
      'message.failed', 'usage', 'error', 'warning', 'session.settled'];
    for (const k of kinds) {
      es.addEventListener(k, (e) => {
        const ev = JSON.parse(e.data);
        if (ev.seq <= lastSeq) return;
        event(ev);
        scroll();
      });
    }
  }

  function render() {
    fill(main, head, log, composer);
    renderHead();
    if (S.ready && !es) connect();
  }
  return {
    tab: 'fleet', render,
    update: (t, d) => {
      if (t === 'all' || (t === 'session' && d && (d.id === id || d.parent === id)) || t === 'host') renderHead();
      if (S.ready && !es) connect();
    },
    close: () => { if (es) es.close(); window.removeEventListener('scroll', onScroll); },
  };
}

// --- inbox ---

function sessionLink(path, sid) {
  return sid && S.sessions.has(sid) ? h('a', {href: '#/s/' + sid, text: path}) : h('span', {text: path});
}

function inboxView() {
  const main = document.getElementById('main');
  const drafts = new Map(); // ask id -> text being typed, kept across re-renders
  function render() {
    const all = [...S.messages.values()].filter((m) => m.to_user).sort((a, b) => b.created_at.localeCompare(a.created_at));
    const open = all.filter((m) => m.kind === 'ask' && (m.state === 'pending' || m.state === 'delivered'));
    const rest = all.filter((m) => !open.includes(m));
    const focused = document.activeElement && document.activeElement.dataset && document.activeElement.dataset.ask;
    fill(main, 
      h('h2', {text: 'Questions for you'}),
      open.length ? h('div', {class: 'list'}, open.map((m) => {
        const box = h('textarea', {rows: 2, placeholder: 'Your answer…', 'data-ask': m.id, 'aria-label': 'Answer'});
        box.value = drafts.get(m.id) || '';
        box.addEventListener('input', () => drafts.set(m.id, box.value));
        const btn = h('button', {type: 'submit', text: 'Answer'});
        const form = h('form', {class: 'reply-box', onsubmit: async (e) => {
          e.preventDefault();
          if (!box.value.trim()) return;
          btn.disabled = true;
          try {
            await api('POST', `/v1/messages/${m.id}/reply`, {text: box.value.trim(), from: ME});
            drafts.delete(m.id);
            toast('Answer sent to ' + m.from);
          } catch (err) {
            toast(err.message);
            btn.disabled = false;
          }
        }}, box, h('div', {class: 'row'}, h('span', {class: 'grow'}), btn));
        const el = h('div', {class: 'item'},
          h('div', {class: 'row small'}, sessionLink(m.from, m.from_session), h('span', {class: 'muted', text: '→ ' + m.to_user}),
            h('span', {class: 'grow'}), h('span', {class: 'muted', text: ago(m.created_at)})),
          h('div', {class: 'text', text: m.text}), form);
        if (focused === m.id) setTimeout(() => box.focus(), 0);
        return el;
      })) : h('p', {class: 'empty', text: 'No open questions.'}),
      h('h2', {text: 'Recent messages to people'}),
      rest.length ? h('div', {class: 'list'}, rest.slice(0, 50).map(messageItem)) : h('p', {class: 'empty', text: 'Nothing yet.'}));
  }
  return {tab: 'inbox', render, update: (t) => { if (t === 'message' || t === 'all') render(); }};
}

function messageItem(m) {
  const el = h('div', {class: 'item clip', onclick: () => el.classList.toggle('clip')},
    h('div', {class: 'row small'},
      h('span', {class: 'k ' + m.kind, text: m.kind}),
      sessionLink(m.from, m.from_session), h('span', {class: 'muted', text: '→'}),
      sessionLink(m.to_path || m.to, m.to_session),
      h('span', {class: 'grow'}),
      h('span', {class: 'st ' + m.state, text: m.state + (m.inline ? ' inline' : '')}),
      h('span', {class: 'muted', text: ago(m.created_at)})),
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
    fill(list, ...(msgs.length ? msgs.slice(0, 200).map(messageItem) : [h('p', {class: 'empty', text: 'No messages.'})]));
  }
  function render() {
    fill(main, h('h2', {text: 'Message traffic'}), filter, list);
    refill();
  }
  return {tab: 'traffic', render, update: (t) => { if (t === 'message' || t === 'all') refill(); }};
}

// --- usage ---

function usageView() {
  const main = document.getElementById('main');
  let timer = null;
  async function render() {
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
        h('thead', {}, h('tr', {}, h('th', {text: title}), h('th', {class: 'num', text: 'in'}), h('th', {class: 'num', text: 'cached'}),
          h('th', {class: 'num', text: 'out'}), h('th', {class: 'num', text: 'total'}))),
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
      h('h2', {text: `Today (${u.today} UTC)`}),
      h('p', {}, h('strong', {text: human(u.today_tokens)}), ' tokens',
        u.max_daily_tokens ? ` of a ${human(u.max_daily_tokens)} daily budget` : ' (no daily budget set)'),
      u.max_daily_tokens ? h('div', {class: 'bar' + (pct >= 100 ? ' over' : '')}, h('span', {style: `width:${pct}%`})) : null,
      h('h2', {text: 'Last 14 days'}),
      h('div', {class: 'days', 'aria-label': 'tokens per day'}, days.map(([d, n]) =>
        h('div', {style: `height:${Math.max(2, 100 * n / max)}%`, title: `${d}: ${human(n)}`}))),
      h('h2', {text: 'By host'}), table('host', u.by_host),
      h('h2', {text: 'By model'}), table('model', u.by_model),
      h('h2', {text: 'Top sessions'}),
      h('table', {}, h('tbody', {}, top.map((s) => h('tr', {},
        h('td', {}, h('a', {href: '#/s/' + s.id, text: s.path})), h('td', {class: 'num', text: human(s.usage.total_tokens)}))))));
  }
  timer = setInterval(() => { if (document.visibilityState === 'visible') render(); }, 30000);
  return {tab: 'usage', render, close: () => clearInterval(timer)};
}

// --- spawn ---

function spawnView() {
  const main = document.getElementById('main');
  const f = {
    host: h('select', {id: 'f-host', required: true}),
    harness: h('select', {id: 'f-harness'}),
    name: h('input', {id: 'f-name', placeholder: 'default: the directory name', autocapitalize: 'off', spellcheck: 'false'}),
    cwd: h('input', {id: 'f-cwd', required: true, placeholder: '~/src/project', autocapitalize: 'off', spellcheck: 'false'}),
    model: h('input', {id: 'f-model', placeholder: "default: the host's", autocapitalize: 'off', spellcheck: 'false'}),
    labels: h('input', {id: 'f-labels', placeholder: 'role=reviewer, team=ios', autocapitalize: 'off', spellcheck: 'false'}),
    approval: h('select', {id: 'f-approval'}, ['', 'never', 'on-request', 'untrusted'].map((v) => h('option', {value: v, text: v || "host's default"}))),
    sandbox: h('select', {id: 'f-sandbox'}, ['', 'read-only', 'workspace-write', 'danger-full-access'].map((v) => h('option', {value: v, text: v || "host's default"}))),
    prompt: h('textarea', {id: 'f-prompt', placeholder: 'What should it do? (optional)'}),
  };
  const btn = h('button', {type: 'submit', text: 'Start session'});
  function fillHosts() {
    const cur = f.host.value;
    const online = [...S.hosts.values()].filter((x) => x.connected).sort((a, b) => a.id.localeCompare(b.id));
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
      S.sessions.set(s.id, s);
      location.hash = '#/s/' + s.id;
    } catch (err) {
      toast(err.message);
      btn.disabled = false;
    }
  }
  const field = (label, el) => [h('label', {for: el.id, text: label}), el];
  function render() {
    fill(main, h('h2', {text: 'New session'}),
      h('form', {class: 'stack', onsubmit: submit},
        field('Host', f.host), field('Harness', f.harness), field('Working directory on that host', f.cwd),
        field('Name', f.name), field('Model', f.model), field('Labels', f.labels),
        field('Approval policy', f.approval), field('Sandbox', f.sandbox), field('First prompt', f.prompt),
        h('div', {class: 'row'}, h('a', {href: '#/', text: 'Cancel'}), h('span', {class: 'grow'}), btn)));
    fillHosts();
  }
  return {tab: 'fleet', render, update: (t) => { if (t === 'host' || t === 'all') fillHosts(); }};
}

// ---------- routing ----------

function route() {
  if (view && view.close) view.close();
  const parts = location.hash.replace(/^#\/?/, '').split('/');
  switch (parts[0]) {
    case 's': view = sessionView(decodeURIComponent(parts[1] || ''), parts[2] === 'all'); break;
    case 'inbox': view = inboxView(); break;
    case 'traffic': view = trafficView(); break;
    case 'usage': view = usageView(); break;
    case 'spawn': view = spawnView(); break;
    default: view = fleetView();
  }
  for (const a of document.querySelectorAll('.tabs a')) a.classList.toggle('on', a.dataset.tab === view.tab);
  window.scrollTo(0, 0);
  view.render();
}

// --- theme: dark by default; light or system per browser ---

const themes = ['dark', 'light', 'system'];
const themeLabel = {dark: 'Dark', light: 'Light', system: 'Auto'};
function storedTheme() {
  try { return themes.includes(localStorage.getItem('swyd-theme')) ? localStorage.getItem('swyd-theme') : 'dark'; } catch (e) { return 'dark'; }
}
function applyTheme(t) {
  document.documentElement.dataset.theme = t;
  const b = document.getElementById('theme');
  if (b) { b.textContent = themeLabel[t]; b.title = `Theme: ${themeLabel[t]} (tap to change)`; }
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.content = getComputedStyle(document.documentElement).getPropertyValue('--panel').trim() || '#1c2027';
}
applyTheme(storedTheme());

document.addEventListener('DOMContentLoaded', () => {
  applyTheme(storedTheme());
  document.getElementById('theme').addEventListener('click', () => {
    const next = themes[(themes.indexOf(storedTheme()) + 1) % themes.length];
    try { localStorage.setItem('swyd-theme', next); } catch (e) { /* storage unavailable: applies to this page only */ }
    applyTheme(next);
  });
  document.getElementById('signout').addEventListener('click', async () => {
    await fetch('/ui/logout', {method: 'POST', credentials: 'same-origin', headers: {'X-Swyd-CSRF': '1'}});
    location.href = '/login';
  });
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
