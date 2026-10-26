// SPDX-License-Identifier: GPL-3.0-or-later
//
// Markdown for agents' messages: GitHub-style (tables, task lists,
// strikethrough, autolinks), footnotes, :emoji: shortcodes, highlighted
// code, KaTeX math and Mermaid diagrams. Raw HTML stays text, and the
// result is sanitized, so a message cannot run script or restyle the page.
// Everything renders under the page's CSP (no inline styles): math as
// MathML, diagrams as SVG images.
'use strict';

const mdVendor = (() => {
  // The vendor scripts' versioned directory, for loading Mermaid later.
  const s = document.querySelector('script[src*="vendor/markdown-it.min.js"]');
  return s ? s.src.replace(/markdown-it\.min\.js.*$/, '') : '/static/vendor/';
})();

// taskLists turns "[ ] " and "[x] " at the start of a list item into a
// checkbox, shown but not editable.
function taskLists(mdi) {
  mdi.core.ruler.after('inline', 'task_lists', (state) => {
    const toks = state.tokens;
    for (let i = 2; i < toks.length; i++) {
      const t = toks[i];
      if (t.type !== 'inline' || toks[i - 1].type !== 'paragraph_open' || toks[i - 2].type !== 'list_item_open') continue;
      const m = /^\[([ xX])\] /.exec(t.content);
      if (!m || !t.children.length || t.children[0].type !== 'text') continue;
      t.children[0].content = t.children[0].content.slice(4);
      const box = new state.Token('html_inline', '', 0);
      box.content = `<input type="checkbox" class="task" disabled${m[1] === ' ' ? '' : ' checked'}> `;
      t.children.unshift(box);
      toks[i - 2].attrJoin('class', 'task-item');
    }
  });
}

function katexHTML(src, display) {
  try {
    return window.katex.renderToString(src, {displayMode: display, output: 'mathml', throwOnError: false});
  } catch (e) {
    return mdEscape(src);
  }
}

function mdEscape(s) {
  return s.replace(/[&<>"]/g, (c) => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;'}[c]));
}

// math adds $…$ and \(…\) inline, $$…$$ and \[…\] blocks. An inline $ needs
// no space just inside either side and no digit after the closing one, so
// prices and shell variables stay text.
function math(mdi) {
  mdi.inline.ruler.before('escape', 'math_inline', (state, silent) => {
    const src = state.src, pos = state.pos;
    let open, close;
    if (src.startsWith('\\(', pos)) {
      open = '\\(';
      close = '\\)';
    } else if (src[pos] === '$' && src[pos + 1] !== '$') {
      open = close = '$';
    } else {
      return false;
    }
    const start = pos + open.length;
    if (open === '$' && (start >= src.length || /\s/.test(src[start]))) return false;
    let end = start;
    for (;;) {
      end = src.indexOf(close, end);
      if (end < 0) return false;
      if (src[end - 1] === '\\') { end++; continue; }
      if (open === '$' && (/\s/.test(src[end - 1]) || /\d/.test(src[end + 1] || ''))) { end++; continue; }
      break;
    }
    if (end === start) return false;
    if (!silent) {
      const t = state.push('math_inline', 'math', 0);
      t.content = src.slice(start, end);
    }
    state.pos = end + close.length;
    return true;
  });
  mdi.block.ruler.before('fence', 'math_block', (state, startLine, endLine, silent) => {
    const first = state.src.slice(state.bMarks[startLine] + state.tShift[startLine], state.eMarks[startLine]).trim();
    let close;
    if (first.startsWith('$$')) close = '$$';
    else if (first.startsWith('\\[')) close = '\\]';
    else return false;
    let body = first.slice(2);
    let line = startLine;
    let done = body.endsWith(close) && body.length >= close.length;
    if (done) body = body.slice(0, -close.length);
    while (!done) {
      line++;
      if (line >= endLine) return false;
      const l = state.src.slice(state.bMarks[line] + state.tShift[line], state.eMarks[line]);
      if (l.trim().endsWith(close)) {
        body += '\n' + l.trim().slice(0, -close.length);
        done = true;
      } else {
        body += '\n' + l;
      }
    }
    if (silent) return true;
    const t = state.push('math_block', 'math', 0);
    t.block = true;
    t.content = body.trim();
    t.map = [startLine, line + 1];
    state.line = line + 1;
    return true;
  }, {alt: ['paragraph', 'reference', 'blockquote', 'list']});
  mdi.renderer.rules.math_inline = (toks, i) => katexHTML(toks[i].content, false);
  mdi.renderer.rules.math_block = (toks, i) => `<div class="math">${katexHTML(toks[i].content, true)}</div>\n`;
}

let mdParser = null;
function mdit() {
  if (mdParser || !window.markdownit) return mdParser;
  const m = window.markdownit({
    html: false, linkify: true, breaks: true,
    highlight: (code, lang) => {
      const hl = window.hljs;
      if (!hl || lang === 'mermaid') return '';
      try {
        if (lang && hl.getLanguage(lang)) return hl.highlight(code, {language: lang, ignoreIllegals: true}).value;
        if (code.length < 20000) return hl.highlightAuto(code).value;
      } catch (e) { /* plain */ }
      return '';
    },
  });
  if (window.markdownitEmoji) m.use(window.markdownitEmoji);
  if (window.markdownitFootnote) m.use(window.markdownitFootnote);
  m.use(taskLists);
  if (window.katex) m.use(math);
  // Links leave the page; images become links (img-src is the page's own).
  const link = m.renderer.rules.link_open || ((t, i, o, e, self) => self.renderToken(t, i, o));
  m.renderer.rules.link_open = (toks, i, opts, env, self) => {
    toks[i].attrSet('target', '_blank');
    toks[i].attrSet('rel', 'noopener noreferrer');
    return link(toks, i, opts, env, self);
  };
  m.renderer.rules.image = (toks, i) => {
    const t = toks[i];
    const alt = t.children ? t.children.map((c) => c.content).join('') : t.content;
    return `<a href="${mdEscape(t.attrGet('src') || '')}" target="_blank" rel="noopener noreferrer">${mdEscape(alt || t.attrGet('src') || 'image')}</a>`;
  };
  // Fenced code: a language label, a copy button (added after sanitizing),
  // and Mermaid source kept for the diagram.
  m.renderer.rules.fence = (toks, i, opts) => {
    const t = toks[i];
    const lang = (t.info || '').trim().split(/\s+/)[0].toLowerCase();
    if (lang === 'mermaid') return `<div class="mermaid-src"><pre class="code" data-lang="mermaid"><code>${mdEscape(t.content)}</code></pre></div>\n`;
    const body = (opts.highlight && opts.highlight(t.content, lang)) || mdEscape(t.content);
    return `<pre class="code"${lang ? ` data-lang="${mdEscape(lang)}"` : ''}><code class="hljs">${body.replace(/\n$/, '')}</code></pre>\n`;
  };
  // Column alignment as a class: the CSP refuses style attributes.
  for (const cell of ['th_open', 'td_open']) {
    m.renderer.rules[cell] = (toks, i, opts, env, self) => {
      const st = toks[i].attrGet('style') || '';
      const al = /text-align:\s*(left|center|right)/.exec(st);
      if (st) toks[i].attrs = (toks[i].attrs || []).filter(([k]) => k !== 'style');
      if (al) toks[i].attrJoin('class', 'align-' + al[1]);
      return self.renderToken(toks, i, opts);
    };
  }
  // Tables scroll sideways on their own rather than widening the page.
  m.renderer.rules.table_open = () => '<div class="table-wrap"><table>\n';
  m.renderer.rules.table_close = () => '</table></div>\n';
  mdParser = m;
  return m;
}

// md renders Markdown as a DocumentFragment. Without the vendor scripts it
// falls back to the text as it is.
function md(text) {
  const out = document.createDocumentFragment();
  const m = mdit();
  if (!m || !window.DOMPurify) {
    out.append(text || '');
    return out;
  }
  const html = window.DOMPurify.sanitize(m.render(text || ''), {
    ADD_ATTR: ['target'], USE_PROFILES: {html: true, mathMl: true},
  });
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  for (const pre of tpl.content.querySelectorAll('pre.code:not(.mermaid-src pre)')) {
    const code = pre.querySelector('code');
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'copy';
    btn.textContent = 'Copy';
    btn.addEventListener('click', () => {
      navigator.clipboard.writeText(code ? code.textContent : '').then(() => {
        btn.textContent = 'Copied';
        setTimeout(() => { btn.textContent = 'Copy'; }, 1500);
      }, () => { btn.textContent = 'Copy failed'; });
    });
    pre.append(btn);
  }
  for (const d of tpl.content.querySelectorAll('div.mermaid-src')) mermaidBlock(d);
  out.append(tpl.content);
  return out;
}

// ---- Mermaid ----

const mermaidCache = new Map(); // theme + source -> data: URL, or {error}
let mermaidLoad = null;
let mermaidSeq = 0;
function mermaidTheme() {
  const t = document.documentElement.dataset.theme;
  const light = t === 'light' || (t === 'system' && window.matchMedia('(prefers-color-scheme: light)').matches);
  return light ? 'default' : 'dark';
}

function loadMermaid() {
  if (!mermaidLoad) {
    mermaidLoad = new Promise((resolve, reject) => {
      const s = document.createElement('script');
      s.src = mdVendor + 'mermaid.min.js';
      s.onload = () => (window.mermaid ? resolve(window.mermaid) : reject(new Error('Mermaid did not load')));
      s.onerror = () => { mermaidLoad = null; reject(new Error('Mermaid did not load')); };
      document.head.append(s);
    });
  }
  return mermaidLoad;
}

// Renders are one at a time: Mermaid keeps global state while it draws.
let mermaidQueue = Promise.resolve();
function renderMermaid(src, theme) {
  const key = theme + '\n' + src;
  const run = mermaidQueue.then(async () => {
    if (mermaidCache.has(key)) return mermaidCache.get(key);
    let res;
    try {
      const mm = await loadMermaid();
      // SVG text labels, not HTML ones: the diagram is shown as an image.
      mm.initialize({startOnLoad: false, securityLevel: 'strict', theme, htmlLabels: false, flowchart: {htmlLabels: false},
        fontFamily: 'system-ui, sans-serif'});
      const {svg} = await mm.render('swk-mermaid-' + (++mermaidSeq), src);
      const vb = /viewBox="[\d.-]+ [\d.-]+ ([\d.]+) /.exec(svg);
      res = {url: 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(svg), width: vb ? Math.ceil(+vb[1]) : 0};
    } catch (e) {
      res = {error: (e && e.message) || String(e)};
      // Mermaid leaves its scratch element behind when a diagram fails.
      document.getElementById('dswk-mermaid-' + mermaidSeq)?.remove();
    }
    mermaidCache.set(key, res);
    return res;
  });
  mermaidQueue = run.catch(() => {});
  return run;
}

function showMermaid(box, src, res) {
  box.replaceChildren();
  if (res && res.url) {
    const img = document.createElement('img');
    img.className = 'mermaid';
    img.alt = 'Diagram';
    img.src = res.url;
    if (res.width) img.width = res.width; // its own size, up to the column's
    img.addEventListener('click', () => (typeof openImage === 'function' ? openImage(res.url, 'Diagram') : window.open(res.url)));
    box.append(img);
    return;
  }
  const pre = document.createElement('pre');
  pre.className = 'code';
  pre.dataset.lang = 'mermaid';
  const code = document.createElement('code');
  code.textContent = src;
  pre.append(code);
  const note = document.createElement('div');
  note.className = 'mermaid-note';
  note.textContent = res ? 'Diagram not drawn: ' + res.error : 'Drawing the diagram…';
  box.append(pre, note);
}

// mermaidBlock shows a cached diagram at once, or the source until the
// message has stopped changing for a moment (it re-renders on every
// streamed delta), then draws it.
function mermaidBlock(box) {
  const src = box.textContent;
  const theme = mermaidTheme();
  const key = theme + '\n' + src;
  if (mermaidCache.has(key)) {
    showMermaid(box, src, mermaidCache.get(key));
    return;
  }
  showMermaid(box, src, null);
  setTimeout(() => {
    if (!box.isConnected) return; // replaced by a newer render meanwhile
    renderMermaid(src, theme).then((res) => { if (box.isConnected) showMermaid(box, src, res); });
  }, 600);
}
