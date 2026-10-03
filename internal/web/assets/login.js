// SPDX-License-Identifier: GPL-3.0-or-later
'use strict';

async function signIn(body) {
  const r = await fetch('/ui/login', {
    method: 'POST', credentials: 'same-origin',
    headers: {'Content-Type': 'application/json', 'X-Swyd-CSRF': '1'},
    body: JSON.stringify(body),
  });
  if (r.ok) {
    location.replace('/');
    return;
  }
  let msg = r.statusText;
  try { msg = (await r.json()).error || msg; } catch (e) { /* not JSON */ }
  document.getElementById('msg').textContent = msg;
}

document.addEventListener('DOMContentLoaded', () => {
  const m = /code=([A-Za-z0-9_-]+)/.exec(location.hash);
  if (m) {
    history.replaceState(null, '', '/login');
    signIn({code: m[1]});
  }
  document.getElementById('login').addEventListener('submit', (e) => {
    e.preventDefault();
    signIn({token: document.getElementById('token').value.trim()});
  });
});
