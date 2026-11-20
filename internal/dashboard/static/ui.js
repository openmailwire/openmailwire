// OpenMailWire dashboard: the shared core. Requests to the same-origin API
// with the signed-in key, and DOM helpers that only ever insert text.
//
// Every value that comes from the API can be attacker-controlled: subjects,
// addresses, SMTP replies, webhook URLs, inbound mail, DMARC reports. Such
// values reach the page only as text (text nodes, textContent, plain
// attributes set by h()). Nothing from the API is ever parsed as markup,
// and links are only built to in-app routes. tests/safety.test.mjs fails
// the build if an HTML sink appears in any script.

import * as L from './lib.js';

const KEY_STORAGE = 'openmailwire.apiKey';
export const API = L.apiBase(location.pathname);
export const PAGE_SIZE = 25;

// ---- the API key: this tab only ----

let memoryKey = '';

export function getKey() {
  try {
    return sessionStorage.getItem(KEY_STORAGE) || memoryKey;
  } catch {
    return memoryKey;
  }
}

export function setKey(key) {
  memoryKey = key;
  try {
    if (key) sessionStorage.setItem(KEY_STORAGE, key);
    else sessionStorage.removeItem(KEY_STORAGE);
  } catch {
    // Storage is blocked: the key lives in memory until the tab closes.
  }
}

// ---- requests ----

export class ApiError extends Error {
  constructor(status, code, message, retryAfter = 0) {
    super(message);
    this.status = status;
    this.code = code;
    this.retryAfter = retryAfter;
  }
}

export const isAbort = (err) => err && err.name === 'AbortError';

// request calls the same-origin API with key. It returns the parsed JSON
// body (or the Response, with raw) and throws ApiError on failure.
export async function request(key, method, path, body, { signal, idempotencyKey, raw = false } = {}) {
  const headers = { Authorization: `Bearer ${key}`, Accept: 'application/json' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (idempotencyKey) headers['Idempotency-Key'] = idempotencyKey;
  let res;
  try {
    res = await fetch(API + path.replace(/^\//, ''), {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
      credentials: 'omit',
      cache: 'no-store',
      redirect: 'error',
    });
  } catch (err) {
    if (isAbort(err)) throw err;
    throw new ApiError(0, 'network_error', String(err && err.message));
  }
  if (res.ok && raw) return res;
  let data = null;
  if ((res.headers.get('Content-Type') || '').includes('application/json')) {
    try {
      data = await res.json();
    } catch {
      data = null;
    }
  }
  if (!res.ok) {
    throw new ApiError(res.status, (data && data.name) || '', (data && data.message) || '',
      Number(res.headers.get('Retry-After')) || 0);
  }
  return data;
}

let unauthorized = () => {};

// onUnauthorized sets what happens when the server rejects the key.
export function onUnauthorized(fn) {
  unauthorized = fn;
}

// api is request with the signed-in key. A 401 ends the session.
export async function api(method, path, body, opts) {
  try {
    return await request(getKey(), method, path, body, opts);
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) unauthorized(L.classifyError(err).detail);
    throw err;
  }
}

export function randomID() {
  if (crypto.randomUUID) return crypto.randomUUID();
  const b = crypto.getRandomValues(new Uint8Array(16));
  return Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
}

export function sleep(ms, signal) {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    signal.addEventListener('abort', () => {
      clearTimeout(t);
      resolve();
    }, { once: true });
  });
}

// ---- DOM helpers ----

export const $ = (sel) => document.querySelector(sel);

// safeHref allows in-app routes only. API data never becomes a link target.
function safeHref(v) {
  const s = String(v);
  if (s.startsWith('#/')) return s;
  throw new TypeError(`refusing link target ${s}`);
}

// h builds an element. Children are nodes or text; strings never become
// markup.
export function h(tag, props, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = String(v);
    else if (k.startsWith('on')) {
      if (typeof v !== 'function') throw new TypeError(`${k} must be a function`);
      el.addEventListener(k.slice(2), v);
    } else if (k === 'style' || k === 'srcdoc') throw new TypeError(`${k} is not allowed`);
    else if (k === 'href') el.setAttribute('href', safeHref(v));
    else el.setAttribute(k, v === true ? '' : String(v));
  }
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false || c === '') continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

const SVG_NS = 'http://www.w3.org/2000/svg';
const SVG_TAGS = new Set(['svg', 'rect', 'line']);
const SVG_ATTRS = new Set(['viewBox', 'width', 'height', 'x', 'y', 'x1', 'x2', 'y1', 'y2', 'class',
  'preserveAspectRatio', 'aria-hidden', 'focusable']);

// svg builds an SVG element for small bars: only shapes and geometry, no
// links, scripts or styles. Colours come from classes in styles.css.
export function svg(tag, attrs, ...children) {
  if (!SVG_TAGS.has(tag)) throw new TypeError(`svg ${tag} is not allowed`);
  const el = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null) continue;
    if (!SVG_ATTRS.has(k)) throw new TypeError(`svg attribute ${k} is not allowed`);
    el.setAttribute(k, String(v));
  }
  for (const c of children.flat(Infinity)) if (c) el.append(c);
  return el;
}

// put replaces el's children, skipping empty ones: replaceChildren alone
// would turn null into the text "null".
export function put(el, ...nodes) {
  el.replaceChildren(...nodes.flat(Infinity).filter((n) => n != null && n !== false && n !== ''));
}

let uid = 0;
export const nextID = (p) => `${p}-${++uid}`;

export function announce(msg) {
  const live = $('#live');
  live.textContent = '';
  setTimeout(() => { live.textContent = msg; }, 50);
}

export const link = (target, ...children) => h('a', { href: target }, ...children);
export const route = (...parts) => `#/${parts.map((p) => encodeURIComponent(p)).join('/')}`;
export const mono = (text) => h('span', { class: 'mono' }, text);
export const muted = (...children) => h('span', { class: 'muted' }, ...children);

export function badge(status, label) {
  return h('span', { class: `badge tone-${L.statusTone(status)}` }, label ?? L.statusLabel(status));
}

// toneBadge is a badge with an explicit tone: ok, info, warn, bad or muted.
export function toneBadge(tone, label, title) {
  return h('span', { class: `badge tone-${tone}`, title }, label);
}

// senderTag marks text written by whoever sent a message or report: it is
// shown as text only and proves nothing.
export function senderTag(what = 'sender-provided') {
  return h('span', { class: 'tag-untrusted', title: 'Written by the sender: not verified. Shown as text only.' }, what);
}

// untrusted wraps sender-written text so it reads as such.
export function untrusted(...children) {
  return h('span', { class: 'untrusted' }, ...children);
}

// when shows a relative time, with the exact time on hover.
export function when(value, empty = '—') {
  if (!value) return muted(empty);
  return h('time', { datetime: value, title: L.formatDateTime(value) }, L.relativeTime(value));
}

export function exact(value) {
  if (!value) return muted('—');
  return h('time', { datetime: value, title: L.relativeTime(value) }, L.formatDateTime(value));
}

async function copyText(value) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(value);
      return true;
    }
  } catch {
    // fall through to the selection fallback
  }
  const back = document.activeElement;
  const ta = h('textarea', { class: 'offscreen', readonly: true, 'aria-hidden': 'true', tabindex: '-1' });
  ta.value = value;
  document.body.append(ta);
  ta.select();
  let ok = false;
  try {
    ok = document.execCommand('copy');
  } catch {
    ok = false;
  }
  ta.remove();
  if (back && back.focus) back.focus();
  return ok;
}

export function copyButton(value, what) {
  const btn = h('button', { type: 'button', class: 'btn btn-small btn-copy', 'aria-label': `Copy ${what}` }, 'Copy');
  let timer;
  btn.addEventListener('click', async () => {
    const ok = await copyText(String(value));
    btn.textContent = ok ? 'Copied' : 'Select and copy';
    announce(ok ? `${what} copied` : `Could not copy ${what}; select it and copy by hand`);
    clearTimeout(timer);
    timer = setTimeout(() => { btn.textContent = 'Copy'; }, 1800);
  });
  return btn;
}

// copyable shows a value in monospace with a copy button.
export function copyable(value, what) {
  return h('span', { class: 'copyable' }, h('code', { class: 'mono' }, value), copyButton(value, what));
}

export function panel(title, ...children) {
  return h('section', { class: 'panel' }, title ? h('h2', { class: 'panel-title' }, title) : null, ...children);
}

export function pageHead(title, sub, ...actions) {
  return h('div', { class: 'page-head' },
    h('div', { class: 'page-title' }, h('h1', { tabindex: '-1' }, title), sub ? h('p', { class: 'sub' }, sub) : null),
    actions.length ? h('div', { class: 'actions' }, ...actions) : null);
}

export function backLink(target, label) {
  return h('p', { class: 'back' }, link(target, h('span', { 'aria-hidden': 'true' }, '← '), label));
}

// loadingLine is a skeleton placeholder, announced as text.
export function loadingLine(text = 'Loading…', lines = 3) {
  return h('div', { class: 'skeleton', role: 'status' },
    h('span', { class: 'sr-only' }, text),
    Array.from({ length: lines }, (_, i) => h('span', { class: `sk-line sk-${i % 3}`, 'aria-hidden': 'true' })));
}

// emptyState says why a list is empty and, when given, what to do next.
export function emptyState(text, ...actions) {
  const acts = actions.flat().filter(Boolean);
  if (!acts.length) return h('p', { class: 'empty' }, text);
  return h('div', { class: 'empty empty-rich' }, h('p', {}, text), h('p', { class: 'empty-actions' }, acts));
}

// kv renders label/value pairs, skipping empty values.
export function kv(pairs) {
  return h('dl', { class: 'kv' },
    pairs.filter((p) => p && p[1] != null && p[1] !== '' && !(Array.isArray(p[1]) && !p[1].length))
      .map(([k, v]) => h('div', { class: 'kv-row' }, h('dt', {}, k), h('dd', {}, v))));
}

// notice kinds: error, ok, warn, calm.
export function notice(kind, title, detail, ...extra) {
  return h('div', { class: `notice notice-${kind}`, role: kind === 'error' ? 'alert' : 'status' },
    h('p', { class: 'notice-title' }, title), detail ? h('p', {}, detail) : null, ...extra);
}

// errorView shows an API failure. Missing endpoints and missing permission
// get a calm panel rather than an error.
export function errorView(err, what, verb) {
  if (isAbort(err)) return null;
  const c = L.classifyError(err, what, verb);
  if (c.kind === 'auth') return null;
  if (c.kind === 'unavailable') return notice('calm', c.title, c.detail);
  if (c.kind === 'forbidden') {
    return notice('calm', c.title, c.detail, c.message ? h('p', { class: 'small muted' }, c.message) : null);
  }
  if (c.kind === 'own') return notice('warn', c.title, c.detail);
  if (c.kind === 'paused') {
    return notice('warn', c.title, c.detail, h('p', {}, link('#/pauses', 'See the pauses')));
  }
  return notice('error', c.title, c.detail, c.code ? h('p', { class: 'small muted' }, 'Error ', h('code', {}, c.code)) : null);
}

// dataTable renders rows as a table that becomes a list of cards on narrow
// screens. columns: [{label, cell(row), class}].
export function dataTable(columns, rows, caption) {
  const tbody = h('tbody');
  const table = h('table', { class: 'data' },
    caption ? h('caption', { class: 'sr-only' }, caption) : null,
    h('thead', {}, h('tr', {}, columns.map((c) => h('th', { scope: 'col', class: c.class }, c.label)))),
    tbody);
  const add = (items) => {
    for (const r of items) {
      tbody.append(h('tr', {}, columns.map((c) => h('td', { 'data-label': c.label, class: c.class }, c.cell(r)))));
    }
  };
  add(rows);
  return { el: h('div', { class: 'table-wrap' }, table), add };
}

// pager adds a "Load more" button for cursor lists. sink.add(items) shows
// a page: a dataTable, or any list with an add method.
export function pager(ctx, sink, first, fetchPage, what) {
  const box = h('div', { class: 'pager' });
  let next = first.next_before;
  const more = h('button', { type: 'button', class: 'btn' }, 'Load more');
  const status = h('div', { 'aria-live': 'polite' });
  more.addEventListener('click', async () => {
    more.disabled = true;
    more.textContent = 'Loading…';
    status.replaceChildren();
    try {
      const page = await fetchPage(next);
      if (!ctx.live) return;
      sink.add(page.data || []);
      next = page.next_before;
      if (!page.has_more || !next) more.remove();
      announce(`${(page.data || []).length} more loaded`);
    } catch (err) {
      if (ctx.live) status.replaceChildren(errorView(err, what) || '');
    } finally {
      more.disabled = false;
      more.textContent = 'Load more';
    }
  });
  if (first.has_more && next) box.append(more);
  box.append(status);
  return box;
}

// ---- forms ----

export function field(label, control, hint) {
  if (!control.id) control.id = nextID('f');
  let hintEl = null;
  if (hint) {
    hintEl = h('p', { class: 'hint', id: `${control.id}-hint` }, hint);
    control.setAttribute('aria-describedby', hintEl.id);
  }
  return h('div', { class: 'field' }, h('label', { for: control.id }, label), control, hintEl);
}

export function input(props) {
  return h('input', { class: 'input', type: 'text', autocomplete: 'off', spellcheck: 'false', autocapitalize: 'off', ...props });
}

export function textarea(props, value = '') {
  const el = h('textarea', { class: 'input', rows: '3', spellcheck: 'false', ...props });
  el.value = value;
  return el;
}

// select builds a <select> from [value, label] pairs.
export function select(props, options, value) {
  const el = h('select', { class: 'input', ...props },
    options.map(([v, label]) => h('option', { value: String(v) }, label)));
  if (value !== undefined) el.value = String(value);
  return el;
}

// checkbox returns {cb, el}: the input and its labelled row.
export function checkbox(label, { checked = false, name, hint } = {}) {
  const cb = h('input', { type: 'checkbox', id: nextID('cb'), name });
  cb.checked = Boolean(checked);
  const hintEl = hint ? h('span', { class: 'muted', id: `${cb.id}-hint` }, ` ${hint}`) : null;
  if (hintEl) cb.setAttribute('aria-describedby', hintEl.id);
  return { cb, el: h('div', { class: 'check' }, cb, h('label', { for: cb.id }, label), hintEl) };
}

export function formStatus() {
  return h('div', { class: 'form-status', 'aria-live': 'polite' });
}

export function fieldError(text) {
  return h('p', { class: 'field-error' }, text);
}

// withBusy disables a form's buttons while fn runs.
export async function withBusy(form, fn) {
  const buttons = [...form.querySelectorAll('button')];
  for (const b of buttons) b.disabled = true;
  form.setAttribute('aria-busy', 'true');
  try {
    return await fn();
  } finally {
    for (const b of buttons) b.disabled = false;
    form.removeAttribute('aria-busy');
  }
}

// secretReveal shows a token or signing secret once.
export function secretReveal(title, value, what, note, ...extra) {
  const box = h('section', { class: 'panel reveal', tabindex: '-1' },
    h('h2', { class: 'panel-title' }, title),
    h('p', {}, h('strong', {}, 'Shown once.'), ' ', note),
    h('div', { class: 'secret' }, h('code', { class: 'mono' }, value), copyButton(value, what)),
    ...extra,
    h('p', {}, h('button', { type: 'button', class: 'btn', onclick: () => box.remove() }, 'I stored it, hide it')));
  setTimeout(() => box.focus(), 0);
  return box;
}

// ---- downloads ----

// download fetches path with the key and saves it as filename. The file is
// always a download, never rendered: whatever type the server or a
// sender gave it is replaced, so nothing can run in this origin.
export async function download(ctx, path, filename) {
  const res = await request(getKey(), 'GET', path, undefined, { signal: ctx.signal, raw: true });
  const blob = new Blob([await res.arrayBuffer()], { type: 'application/octet-stream' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = L.safeFilename(filename);
  a.rel = 'noopener';
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 30000);
}

// ---- page context ----

// Each page gets a context that dies when the person navigates away, so
// late responses and polls never touch the next page.
export function makeContext(signal, focusHeading) {
  const main = $('#main');
  const ctx = {
    signal,
    get live() { return !signal.aborted; },
    mount(...nodes) {
      if (signal.aborted) return;
      put(main, nodes);
      if (focusHeading) {
        const h1 = main.querySelector('h1');
        if (h1) {
          h1.focus({ preventScroll: true });
          focusHeading = false;
        }
      }
    },
    api(method, path, body, opts = {}) {
      return api(method, path, body, { ...opts, signal });
    },
  };
  return ctx;
}

// fill shows a loading line in box, then what build returns, or an error.
export async function fill(ctx, box, head, what, build) {
  put(box, head, loadingLine());
  try {
    const nodes = await build();
    if (ctx.live) put(box, head, nodes);
  } catch (err) {
    if (ctx.live && !isAbort(err)) put(box, head, errorView(err, what));
  }
}

// confirmed asks before a change, in a modal dialog. It resolves to true
// when the person confirms. danger marks a destructive action: the
// dialog then focuses Cancel.
export function confirmed(question, { title = 'Please confirm', confirm = 'Confirm', danger = false } = {}) {
  const titleID = nextID('dlg');
  const dlg = h('dialog', { class: 'dialog', 'aria-labelledby': titleID });
  if (typeof dlg.showModal !== 'function') return Promise.resolve(window.confirm(question));
  return new Promise((resolve) => {
    let result = false;
    const ok = h('button', { type: 'button', class: `btn ${danger ? 'btn-danger btn-danger-solid' : 'btn-primary'}` }, confirm);
    const cancel = h('button', { type: 'button', class: 'btn' }, 'Cancel');
    ok.addEventListener('click', () => { result = true; dlg.close(); });
    cancel.addEventListener('click', () => dlg.close());
    put(dlg, h('h2', { class: 'dialog-title', id: titleID }, title), h('p', { class: 'dialog-body' }, question),
      h('div', { class: 'dialog-actions' }, cancel, ok));
    const back = document.activeElement;
    dlg.addEventListener('close', () => {
      dlg.remove();
      if (back && back.isConnected && back.focus) back.focus();
      resolve(result);
    }, { once: true });
    document.body.append(dlg);
    dlg.showModal();
    (danger ? cancel : ok).focus();
  });
}

// showDialog opens an informational dialog with a Close button.
export function showDialog(title, ...content) {
  const titleID = nextID('dlg');
  const dlg = h('dialog', { class: 'dialog dialog-wide', 'aria-labelledby': titleID });
  const close = h('button', { type: 'button', class: 'btn' }, 'Close');
  close.addEventListener('click', () => dlg.close());
  put(dlg, h('h2', { class: 'dialog-title', id: titleID }, title), content, h('div', { class: 'dialog-actions' }, close));
  const back = document.activeElement;
  dlg.addEventListener('close', () => {
    dlg.remove();
    if (back && back.isConnected && back.focus) back.focus();
  }, { once: true });
  document.body.append(dlg);
  if (typeof dlg.showModal === 'function') dlg.showModal();
  close.focus();
  return dlg;
}

// toast shows a short message about an action that finished. tone: ok,
// warn or bad. It is announced by the live region it sits in.
export function toast(message, tone = 'ok') {
  const box = $('#toasts');
  if (!box) return;
  const close = h('button', { type: 'button', class: 'toast-close', 'aria-label': 'Dismiss' }, '×');
  const t = h('div', { class: `toast tone-${tone}` }, h('span', { class: 'toast-icon', 'aria-hidden': 'true' }), h('span', { class: 'toast-text' }, message), close);
  const remove = () => t.remove();
  close.addEventListener('click', remove);
  box.append(t);
  while (box.children.length > 4) box.firstElementChild.remove();
  setTimeout(remove, tone === 'bad' ? 9000 : 5000);
}

// ---- preferences: this browser, never secrets ----

const PREF_PREFIX = 'openmailwire.pref.';

// getPref and setPref keep small, non-secret settings (a dismissed
// checklist, when Mail was last read) in localStorage. The API key never
// goes there; tests/safety.test.mjs checks it.
export function getPref(name) {
  try {
    return localStorage.getItem(PREF_PREFIX + name) || '';
  } catch {
    return '';
  }
}

export function setPref(name, value) {
  try {
    if (value) localStorage.setItem(PREF_PREFIX + name, String(value));
    else localStorage.removeItem(PREF_PREFIX + name);
  } catch {
    // Storage is blocked: the setting lasts until the page reloads.
  }
}

// ---- HTML preview ----

// previewFrame is an iframe for HTML someone is writing in this tab. It is
// sandboxed with no permissions: no scripts, no same origin, no forms, no
// popups, no navigation of this page. It also inherits this page's CSP,
// so it loads nothing from elsewhere.
export function previewFrame(title) {
  return h('iframe', { class: 'preview-frame', sandbox: true, title, referrerpolicy: 'no-referrer', loading: 'lazy' });
}

// setPreview shows html in a preview frame. It refuses any frame that is
// not fully sandboxed.
export function setPreview(frame, html) {
  if (!frame || frame.tagName !== 'IFRAME' || frame.getAttribute('sandbox') !== '') {
    throw new TypeError('preview frames must be sandboxed with no permissions');
  }
  frame.srcdoc = String(html);
}

// heldBadge is set by the app: it refreshes the held count in the nav.
let heldRefresher = () => {};
export function onHeldChange(fn) {
  heldRefresher = fn;
}
export function heldChanged() {
  heldRefresher();
}
