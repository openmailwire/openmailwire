// OpenMailWire dashboard: plain ES modules, no build step, no dependencies.
//
// Every value that comes from the API can be attacker-controlled: subjects,
// addresses, SMTP replies, webhook URLs, inbound mail. Such values reach
// the page only as text (text nodes, textContent, plain attributes set by
// h()). Nothing from the API is ever parsed as markup, and links are only
// built to in-app routes. tests/safety.test.mjs fails the build if an HTML
// sink appears in this file.

import * as L from './lib.js';

const KEY_STORAGE = 'openmailwire.apiKey';
const API = L.apiBase(location.pathname);
const PAGE_SIZE = 25;
const POLL_MS = 5000;
const POLL_FOR_MS = 10 * 60 * 1000;

// ---- the API key: this tab only ----

let memoryKey = '';

function getKey() {
  try {
    return sessionStorage.getItem(KEY_STORAGE) || memoryKey;
  } catch {
    return memoryKey;
  }
}

function setKey(key) {
  memoryKey = key;
  try {
    if (key) sessionStorage.setItem(KEY_STORAGE, key);
    else sessionStorage.removeItem(KEY_STORAGE);
  } catch {
    // Storage is blocked: the key lives in memory until the tab closes.
  }
}

// ---- requests ----

class ApiError extends Error {
  constructor(status, code, message, retryAfter = 0) {
    super(message);
    this.status = status;
    this.code = code;
    this.retryAfter = retryAfter;
  }
}

const isAbort = (err) => err && err.name === 'AbortError';

// request calls the same-origin API with key. It returns the parsed JSON
// body (or the Response, with raw) and throws ApiError on failure.
async function request(key, method, path, body, { signal, idempotencyKey, raw = false } = {}) {
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

// api is request with the signed-in key. A 401 ends the session.
async function api(method, path, body, opts) {
  try {
    return await request(getKey(), method, path, body, opts);
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      signOut('The server rejected the API key: it is invalid or was revoked. Sign in again.');
    }
    throw err;
  }
}

function randomID() {
  if (crypto.randomUUID) return crypto.randomUUID();
  const b = crypto.getRandomValues(new Uint8Array(16));
  return Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
}

function sleep(ms, signal) {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    signal.addEventListener('abort', () => {
      clearTimeout(t);
      resolve();
    }, { once: true });
  });
}

// ---- DOM helpers ----

const $ = (sel) => document.querySelector(sel);

// safeHref allows in-app routes only. API data never becomes a link target.
function safeHref(v) {
  const s = String(v);
  if (s.startsWith('#/')) return s;
  throw new TypeError(`refusing link target ${s}`);
}

// h builds an element. Children are nodes or text; strings never become
// markup.
function h(tag, props, ...children) {
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

// put replaces el's children, skipping empty ones: replaceChildren alone
// would turn null into the text "null".
function put(el, ...nodes) {
  el.replaceChildren(...nodes.flat(Infinity).filter((n) => n != null && n !== false && n !== ''));
}

let uid = 0;
const nextID = (p) => `${p}-${++uid}`;

function announce(msg) {
  const live = $('#live');
  live.textContent = '';
  setTimeout(() => { live.textContent = msg; }, 50);
}

const link = (route, ...children) => h('a', { href: route }, ...children);
const route = (...parts) => `#/${parts.map((p) => encodeURIComponent(p)).join('/')}`;
const mono = (text) => h('span', { class: 'mono' }, text);
const muted = (...children) => h('span', { class: 'muted' }, ...children);

function badge(status, label) {
  return h('span', { class: `badge tone-${L.statusTone(status)}` }, label ?? L.statusLabel(status));
}

// when shows a relative time, with the exact time on hover.
function when(value, empty = '—') {
  if (!value) return muted(empty);
  return h('time', { datetime: value, title: L.formatDateTime(value) }, L.relativeTime(value));
}

function exact(value) {
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

function copyButton(value, what) {
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
function copyable(value, what) {
  return h('span', { class: 'copyable' }, h('code', { class: 'mono' }, value), copyButton(value, what));
}

function panel(title, ...children) {
  return h('section', { class: 'panel' }, title ? h('h2', { class: 'panel-title' }, title) : null, ...children);
}

function pageHead(title, sub, ...actions) {
  return h('div', { class: 'page-head' },
    h('div', { class: 'page-title' }, h('h1', { tabindex: '-1' }, title), sub ? h('p', { class: 'sub' }, sub) : null),
    actions.length ? h('div', { class: 'actions' }, ...actions) : null);
}

function backLink(target, label) {
  return h('p', { class: 'back' }, link(target, h('span', { 'aria-hidden': 'true' }, '← '), label));
}

function loadingLine(text = 'Loading…') {
  return h('p', { class: 'loading', role: 'status' }, text);
}

function emptyState(text) {
  return h('p', { class: 'empty' }, text);
}

// kv renders label/value pairs, skipping empty values.
function kv(pairs) {
  return h('dl', { class: 'kv' },
    pairs.filter((p) => p && p[1] != null && p[1] !== '' && !(Array.isArray(p[1]) && !p[1].length))
      .map(([k, v]) => h('div', { class: 'kv-row' }, h('dt', {}, k), h('dd', {}, v))));
}

function notice(kind, title, detail, ...extra) {
  return h('div', { class: `notice notice-${kind}`, role: kind === 'error' ? 'alert' : 'status' },
    h('p', { class: 'notice-title' }, title), detail ? h('p', {}, detail) : null, ...extra);
}

// errorView shows an API failure. Missing endpoints and missing permission
// get a calm panel rather than an error.
function errorView(err, what) {
  if (isAbort(err)) return null;
  const c = L.classifyError(err, what);
  if (c.kind === 'auth') return null;
  if (c.kind === 'unavailable') return notice('calm', c.title, c.detail);
  if (c.kind === 'forbidden') {
    return notice('calm', c.title, c.detail, c.message ? h('p', { class: 'small muted' }, c.message) : null);
  }
  return notice('error', c.title, c.detail, c.code ? h('p', { class: 'small muted' }, 'Error ', h('code', {}, c.code)) : null);
}

// dataTable renders rows as a table that becomes a list of cards on narrow
// screens. columns: [{label, cell(row), class}].
function dataTable(columns, rows, caption) {
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

// pager adds a "Load more" button for cursor lists.
function pager(ctx, table, first, fetchPage, what) {
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
      table.add(page.data || []);
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

function field(label, control, hint) {
  if (!control.id) control.id = nextID('f');
  let hintEl = null;
  if (hint) {
    hintEl = h('p', { class: 'hint', id: `${control.id}-hint` }, hint);
    control.setAttribute('aria-describedby', hintEl.id);
  }
  return h('div', { class: 'field' }, h('label', { for: control.id }, label), control, hintEl);
}

function input(props) {
  return h('input', { class: 'input', type: 'text', autocomplete: 'off', spellcheck: 'false', autocapitalize: 'off', ...props });
}

function textarea(props, value = '') {
  const el = h('textarea', { class: 'input', rows: '3', spellcheck: 'false', ...props });
  el.value = value;
  return el;
}

function formStatus() {
  return h('div', { class: 'form-status', 'aria-live': 'polite' });
}

// withBusy disables a form's buttons while fn runs.
async function withBusy(form, fn) {
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
function secretReveal(title, value, what, note) {
  const box = h('section', { class: 'panel reveal', tabindex: '-1' },
    h('h2', { class: 'panel-title' }, title),
    h('p', {}, h('strong', {}, 'Shown once.'), ' ', note),
    h('div', { class: 'secret' }, h('code', { class: 'mono' }, value), copyButton(value, what)),
    h('p', {}, h('button', { type: 'button', class: 'btn', onclick: () => box.remove() }, 'I stored it, hide it')));
  setTimeout(() => box.focus(), 0);
  return box;
}

// ---- pages ----

// Each page gets a context that dies when the person navigates away, so
// late responses and polls never touch the next page.
function makeContext(signal, focusHeading) {
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
async function fill(ctx, box, head, what, build) {
  put(box, head, loadingLine());
  try {
    const nodes = await build();
    if (ctx.live) put(box, head, nodes);
  } catch (err) {
    if (ctx.live && !isAbort(err)) put(box, head, errorView(err, what));
  }
}

// ---- emails ----

function lookupForm() {
  const id = input({ name: 'id', placeholder: 'Email ID', 'aria-label': 'Email ID', class: 'input mono' });
  const status = formStatus();
  const form = h('form', { class: 'inline-form', 'aria-label': 'Open an email by ID' },
    id, h('button', { type: 'submit', class: 'btn' }, 'Open'), status);
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    const v = id.value.trim().toLowerCase();
    if (!L.isUUID(v)) {
      status.replaceChildren(h('p', { class: 'field-error' }, 'An email ID looks like ed01a077-…, 36 characters.'));
      return;
    }
    location.hash = route('emails', v);
  });
  return form;
}

function sendTestPanel(ctx) {
  const from = input({ name: 'from', required: true, inputmode: 'email', placeholder: 'test@your-domain.com' });
  const to = input({ name: 'to', required: true, inputmode: 'email', placeholder: 'you@example.org' });
  const subject = input({ name: 'subject', required: true, value: 'OpenMailWire test' });
  const text = textarea({ name: 'text', required: true, rows: '4', spellcheck: 'true' },
    'This is a test email sent from the OpenMailWire dashboard.');
  const status = formStatus();
  let idem = randomID();
  const form = h('form', { class: 'form', novalidate: true },
    h('div', { class: 'grid-2' },
      field('From', from, 'An address on a verified domain.'),
      field('To', to, 'Separate several addresses with commas.')),
    field('Subject', subject),
    field('Text', text),
    h('div', { class: 'form-actions' },
      h('button', { type: 'submit', class: 'btn btn-primary' }, 'Send test email'),
      h('p', { class: 'hint' }, 'This sends real email.')),
    status);
  form.addEventListener('input', () => { idem = randomID(); });
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    status.replaceChildren();
    const toList = L.parseList(to.value, { lower: false });
    if (!from.value.trim() || !toList.length || !subject.value.trim() || !text.value.trim()) {
      status.replaceChildren(h('p', { class: 'field-error' }, 'Fill in from, to, subject and text.'));
      return;
    }
    await withBusy(form, async () => {
      try {
        const res = await ctx.api('POST', '/emails',
          { from: from.value.trim(), to: toList, subject: subject.value, text: text.value },
          { idempotencyKey: idem });
        location.hash = route('emails', res.id);
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'emails') || '');
      }
    });
  });
  const details = h('details', { class: 'panel collapsible' },
    h('summary', {}, h('span', { class: 'panel-title' }, 'Send a test email')), form);
  let prefilled = false;
  details.addEventListener('toggle', () => {
    if (!details.open || prefilled) return;
    prefilled = true;
    ctx.api('GET', '/domains').then((res) => {
      const d = ((res && res.data) || []).find((x) => x.status === 'verified');
      if (d && !from.value) from.value = `test@${d.name}`;
    }).catch(() => {});
  });
  return details;
}

const emailColumns = [
  {
    label: 'Status',
    class: 'col-status',
    cell: (e) => {
      if (e.scheduled_at && new Date(e.scheduled_at) > new Date() && (!e.last_event || e.last_event === 'queued' || e.last_event === 'scheduled')) {
        return badge('scheduled', 'Scheduled');
      }
      return badge(e.last_event || 'queued');
    },
  },
  { label: 'Subject', class: 'col-main', cell: (e) => link(route('emails', e.id), e.subject || '(no subject)') },
  { label: 'To', class: 'col-addr', cell: (e) => mono(L.summarizeAddresses(e.to)) },
  { label: 'From', class: 'col-addr', cell: (e) => mono(e.from || '—') },
  { label: 'Sent', class: 'col-time', cell: (e) => (e.scheduled_at ? h('span', {}, when(e.created_at), muted(' · for '), exact(e.scheduled_at)) : when(e.created_at)) },
];

async function pageEmails(ctx) {
  const list = h('section', { class: 'panel' });
  ctx.mount(
    pageHead('Emails', 'Messages sent with this project, newest first.', lookupForm()),
    sendTestPanel(ctx),
    list);
  const head = h('h2', { class: 'panel-title' }, 'Recent emails');
  const fetchPage = (before) => ctx.api('GET', L.listPath('/emails', { limit: PAGE_SIZE, before }));
  await fill(ctx, list, head, 'an email list', async () => {
    let first;
    try {
      first = await fetchPage();
    } catch (err) {
      if (L.classifyError(err).kind !== 'unavailable') throw err;
      return [errorView(err, 'an email list'), h('p', { class: 'hint' }, 'You can still open an email by its ID, above.')];
    }
    if (!first.data || !first.data.length) return emptyState('No emails yet. Send a test email above, or call POST /emails.');
    const table = dataTable(emailColumns, first.data, 'Recent emails');
    return [table.el, pager(ctx, table, first, fetchPage, 'emails')];
  });
}

function attemptEntry(entry) {
  const a = entry.attempt;
  const outcome = a.outcome || (a.finished_at ? 'unknown' : 'sending');
  const reply = L.smtpReply(a.smtp_code, a.enhanced_code, a.response);
  const facts = [['Worker', a.worker], ['Source IP', a.source_ip], ['MX', a.mx_host], ['TLS', a.tls || 'none']]
    .filter(([, v]) => v);
  return h('li', { class: `tl-item tone-${L.statusTone(outcome)}` },
    h('div', { class: 'tl-when' }, exact(a.started_at)),
    h('div', { class: 'tl-what' },
      h('p', { class: 'tl-head' },
        h('strong', {}, `Attempt ${entry.n}`), ' ',
        badge(outcome, a.outcome ? undefined : 'In progress'),
        entry.duration != null ? muted(` · ${L.formatDuration(entry.duration)}`) : null),
      h('dl', { class: 'facts' }, facts.map(([k, v]) => h('div', {}, h('dt', {}, k), h('dd', { class: 'mono' }, v)))),
      reply ? h('pre', { class: 'smtp', 'aria-label': 'SMTP reply' }, reply) : null));
}

function eventEntry(entry) {
  const e = entry.event;
  const details = L.eventDetails(e.data, { fromAttempt: Boolean(entry.attemptNo) });
  return h('li', { class: `tl-item tl-event tone-${L.statusTone(e.type)}` },
    h('div', { class: 'tl-when' }, exact(e.created_at)),
    h('div', { class: 'tl-what' },
      h('p', { class: 'tl-head' },
        badge(e.type),
        entry.attemptNo ? muted(` reported by attempt ${entry.attemptNo}`) : null,
        e.recipient ? null : muted(' (whole email)')),
      details.length
        ? h('dl', { class: 'facts' }, details.map(([k, v]) => h('div', {}, h('dt', {}, k), h('dd', {}, v))))
        : null));
}

function renderEmail(box, email, { pending, onRefresh }) {
  const recipients = email.recipients || [];
  const groups = L.buildTimeline(email);
  const tags = Object.entries(email.tags || {});
  const refresh = h('button', { type: 'button', class: 'btn', onclick: onRefresh }, 'Refresh');
  put(box,
    pageHead(email.subject || '(no subject)',
      h('span', {}, badge(email.last_event || 'queued'), ' Created ', when(email.created_at)),
      refresh),
    pending ? h('p', { class: 'live-note' }, h('span', { class: 'pulse', 'aria-hidden': 'true' }),
      'Refreshing every 5 s until every recipient has a final status.') : null,
    panel('Message', kv([
      ['ID', copyable(email.id, 'email ID')],
      ['From', mono(email.from)],
      ['To', mono(L.addressList(email.to).join(', '))],
      ['Cc', L.addressList(email.cc).length ? mono(L.addressList(email.cc).join(', ')) : null],
      ['Bcc', L.addressList(email.bcc).length ? mono(L.addressList(email.bcc).join(', ')) : null],
      ['Message-ID', email.message_id ? copyable(email.message_id, 'Message-ID') : null],
      ['Created', exact(email.created_at)],
      ['Scheduled for', email.scheduled_at ? exact(email.scheduled_at) : null],
      ['Expires', exact(email.expires_at)],
      ['Tags', tags.length ? h('span', { class: 'chips' }, tags.map(([k, v]) => h('span', { class: 'chip mono' }, `${k}=${v}`))) : null],
      ['Body', email.body_purged ? 'Purged after the retention window' : 'Stored, encrypted'],
    ])),
    panel(`Recipients (${recipients.length})`, recipients.length
      ? dataTable([
        { label: 'Recipient', class: 'col-main col-addr', cell: (r) => mono(r.email) },
        { label: 'Status', class: 'col-status', cell: (r) => badge(r.status) },
        { label: 'Attempts', class: 'col-num', cell: (r) => String(r.attempts ?? 0) },
        { label: 'Next attempt', class: 'col-time', cell: (r) => when(r.next_attempt_at) },
        { label: 'Last reply', cell: (r) => (r.last_code || r.last_response ? h('span', { class: 'mono small' }, L.smtpReply(r.last_code, '', r.last_response)) : muted('—')) },
      ], recipients, 'Recipients').el
      : emptyState('No recipients.')),
    panel('Timeline', groups.length
      ? groups.map((g) => h('section', { class: 'tl-group' },
        h('h3', { class: 'tl-recipient' }, mono(g.recipient), ' ', g.status ? badge(g.status) : null),
        g.entries.length
          ? h('ol', { class: 'timeline' }, g.entries.map((en) => (en.kind === 'attempt' ? attemptEntry(en) : eventEntry(en))))
          : emptyState('Nothing has happened yet.')))
      : emptyState('Nothing has happened yet.')));
}

async function pageEmail(ctx, id) {
  if (!L.isUUID(id)) return pageNotFound(ctx, 'That is not an email ID.');
  const box = h('div');
  const path = `/emails/${encodeURIComponent(id)}`;
  let email = await ctx.api('GET', path);
  let shown = '';
  let polling = true;
  const show = (force) => {
    const snapshot = JSON.stringify(email);
    if (!force && snapshot === shown) return;
    shown = snapshot;
    renderEmail(box, email, { pending: polling && L.isPending(email), onRefresh: refresh });
  };
  const refresh = async () => {
    try {
      email = await ctx.api('GET', path);
      show(true);
      announce('Refreshed');
    } catch (err) {
      if (ctx.live) box.prepend(errorView(err, 'emails') || '');
    }
  };
  show(true);
  ctx.mount(backLink('#/emails', 'All emails'), box);
  const started = Date.now();
  while (ctx.live && L.isPending(email) && Date.now() - started < POLL_FOR_MS) {
    await sleep(POLL_MS, ctx.signal);
    if (!ctx.live) return;
    if (document.hidden) continue;
    try {
      email = await ctx.api('GET', path);
    } catch {
      break; // a failure ends live updates; Refresh still works
    }
    show(false);
  }
  polling = false;
  if (ctx.live) show(true);
}

// ---- domains ----

async function pageDomains(ctx) {
  const name = input({ name: 'name', required: true, placeholder: 'example.com', inputmode: 'url' });
  const status = formStatus();
  const form = h('form', { class: 'form', novalidate: true },
    h('div', { class: 'row' }, field('Domain name', name, 'The domain in your From addresses.'),
      h('div', { class: 'row-end' }, h('button', { type: 'submit', class: 'btn btn-primary' }, 'Add domain'))),
    status);
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    status.replaceChildren();
    if (!name.value.trim()) {
      status.replaceChildren(h('p', { class: 'field-error' }, 'Enter a domain name.'));
      return;
    }
    await withBusy(form, async () => {
      try {
        const d = await ctx.api('POST', '/domains', { name: name.value.trim() });
        location.hash = route('domains', d.id);
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'domains') || '');
      }
    });
  });
  const list = h('section', { class: 'panel' });
  ctx.mount(
    pageHead('Domains', 'Domains this project sends from. Each needs its DKIM and SPF records before it can send.'),
    panel('Add a domain', form),
    list);
  await fill(ctx, list, h('h2', { class: 'panel-title' }, 'Your domains'), 'domains', async () => {
    const res = await ctx.api('GET', '/domains');
    const rows = res.data || [];
    if (!rows.length) return emptyState('No domains yet. Add one above.');
    return dataTable([
      { label: 'Domain', class: 'col-main', cell: (d) => link(route('domains', d.id), d.name) },
      { label: 'Status', class: 'col-status', cell: (d) => badge(d.status) },
      { label: 'Added', class: 'col-time', cell: (d) => when(d.created_at) },
      { label: 'Last check', class: 'col-time', cell: (d) => when(d.checked_at, 'never') },
    ], rows, 'Domains').el;
  });
}

function recordsTable(records) {
  return dataTable([
    {
      label: 'Record',
      cell: (r) => h('span', {}, h('strong', {}, r.record), r.required ? null : muted(' recommended')),
    },
    { label: 'Type', cell: (r) => (r.priority ? `${r.type} ${r.priority}` : r.type) },
    { label: 'Name', class: 'col-name', cell: (r) => copyable(r.name, `${r.record} record name`) },
    { label: 'Value', class: 'col-value', cell: (r) => copyable(r.value, `${r.record} record value`) },
    {
      label: 'Status',
      class: 'col-status',
      cell: (r) => h('span', { class: 'status-cell' }, badge(r.status), r.detail ? h('span', { class: 'small muted' }, r.detail) : null),
    },
  ], records, 'DNS records');
}

async function pageDomain(ctx, id) {
  if (!L.isUUID(id)) return pageNotFound(ctx, 'That is not a domain ID.');
  const path = `/domains/${encodeURIComponent(id)}`;
  const box = h('div');
  const render = (d, message) => {
    const verify = h('button', { type: 'button', class: 'btn btn-primary' }, 'Verify DNS now');
    const del = h('button', { type: 'button', class: 'btn btn-danger' }, 'Delete');
    const status = formStatus();
    if (message) status.append(message);
    verify.addEventListener('click', async () => {
      verify.disabled = true;
      verify.textContent = 'Checking DNS…';
      status.replaceChildren(loadingLine('Looking up the records. This can take up to 20 s.'));
      try {
        const nd = await ctx.api('POST', `${path}/verify`);
        if (!ctx.live) return;
        render(nd, nd.status === 'verified'
          ? notice('ok', 'Verified', 'This domain can send.')
          : notice('calm', 'Not verified yet', 'Check the records marked missing or failed below. DNS changes can take a while to appear.'));
      } catch (err) {
        if (!ctx.live) return;
        status.replaceChildren(errorView(err, 'domains') || '');
        verify.disabled = false;
        verify.textContent = 'Verify DNS now';
      }
    });
    del.addEventListener('click', async () => {
      if (!window.confirm(`Delete ${d.name}? Mail from it is refused until it is added and verified again.`)) return;
      del.disabled = true;
      try {
        await ctx.api('DELETE', path);
        location.hash = '#/domains';
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'domains') || '');
        del.disabled = false;
      }
    });
    put(box,
      pageHead(d.name, h('span', {}, badge(d.status), ' Last checked ', when(d.checked_at, 'never')), verify, del),
      status,
      panel('DNS records',
        h('p', { class: 'hint' }, 'Publish these records at your DNS provider, then verify. The domain can send once every required record is found.'),
        recordsTable(d.records || []).el),
      panel('Details', kv([
        ['ID', copyable(d.id, 'domain ID')],
        ['Added', exact(d.created_at)],
        ['Verified', exact(d.verified_at)],
        ['Last check', exact(d.checked_at)],
      ])));
    if (message) announce(message.textContent);
  };
  render(await ctx.api('GET', path));
  ctx.mount(backLink('#/domains', 'All domains'), box);
}

// ---- webhooks ----

function eventChips(events) {
  if (!events || !events.length) return muted('All events');
  return h('span', { class: 'chips' }, events.map((e) => h('span', { class: 'chip' }, L.statusLabel(e))));
}

async function pageWebhooks(ctx) {
  const endpoint = input({ name: 'endpoint', type: 'url', required: true, inputmode: 'url', placeholder: 'https://example.com/webhooks/openmailwire' });
  const boxes = L.WEBHOOK_EVENTS.map(([type, help]) => {
    const cb = h('input', { type: 'checkbox', name: 'events', value: type, id: nextID('ev') });
    return { type, cb, el: h('div', { class: 'check' }, cb, h('label', { for: cb.id }, h('span', { class: 'mono' }, type), muted(` ${help}`))) };
  });
  const status = formStatus();
  const reveal = h('div');
  const form = h('form', { class: 'form', novalidate: true },
    field('Endpoint URL', endpoint, 'Must use https and resolve to a public address.'),
    h('fieldset', { class: 'fieldset' },
      h('legend', {}, 'Events'),
      h('p', { class: 'hint' }, 'Leave all unchecked to receive every event type.'),
      h('div', { class: 'checks' }, boxes.map((b) => b.el))),
    h('div', { class: 'form-actions' }, h('button', { type: 'submit', class: 'btn btn-primary' }, 'Create webhook')),
    status);
  const list = h('section', { class: 'panel' });
  const listHead = h('h2', { class: 'panel-title' }, 'Webhooks');
  const create = h('details', { class: 'panel collapsible' }, h('summary', {}, h('span', { class: 'panel-title' }, 'Create a webhook')), form);
  const loadList = () => fill(ctx, list, listHead, 'webhooks', async () => {
    let res;
    try {
      res = await ctx.api('GET', '/webhooks');
    } catch (err) {
      if (L.classifyError(err).kind === 'forbidden') create.hidden = true;
      throw err;
    }
    const rows = res.data || [];
    if (!rows.length) return emptyState('No webhooks yet.');
    return dataTable([
      { label: 'Endpoint', class: 'col-main', cell: (w) => link(route('webhooks', w.id), h('span', { class: 'mono' }, w.endpoint)) },
      { label: 'Events', cell: (w) => eventChips(w.events) },
      { label: 'Created', class: 'col-time', cell: (w) => when(w.created_at) },
      { label: 'Actions', class: 'col-actions', cell: (w) => link(route('webhooks', w.id), 'Deliveries') },
    ], rows, 'Webhooks').el;
  });
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    status.replaceChildren();
    if (!endpoint.value.trim()) {
      status.replaceChildren(h('p', { class: 'field-error' }, 'Enter the endpoint URL.'));
      return;
    }
    const events = boxes.filter((b) => b.cb.checked).map((b) => b.type);
    await withBusy(form, async () => {
      try {
        const res = await ctx.api('POST', '/webhooks', { endpoint: endpoint.value.trim(), events });
        if (!ctx.live) return;
        form.reset();
        reveal.replaceChildren(secretReveal('Signing secret', res.signing_secret, 'signing secret',
          'Copy it now. Your handler needs it to verify the svix-signature or webhook-signature header.'));
        loadList();
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'webhooks') || '');
      }
    });
  });
  ctx.mount(
    pageHead('Webhooks', 'Signed HTTPS callbacks for every delivery event, verifiable by handlers written for Resend.'),
    reveal,
    create,
    list);
  await loadList();
}

async function pageWebhook(ctx, id) {
  if (!L.isUUID(id)) return pageNotFound(ctx, 'That is not a webhook ID.');
  const path = `/webhooks/${encodeURIComponent(id)}`;
  const box = h('div');
  const status = formStatus();
  const render = (w) => {
    const del = h('button', { type: 'button', class: 'btn btn-danger' }, 'Delete');
    const refresh = h('button', { type: 'button', class: 'btn' }, 'Refresh');
    refresh.addEventListener('click', async () => {
      refresh.disabled = true;
      try {
        render(await ctx.api('GET', path));
        announce('Refreshed');
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'webhooks') || '');
        refresh.disabled = false;
      }
    });
    del.addEventListener('click', async () => {
      if (!window.confirm('Delete this webhook? Pending deliveries to it stop.')) return;
      del.disabled = true;
      try {
        await ctx.api('DELETE', path);
        location.hash = '#/webhooks';
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'webhooks') || '');
        del.disabled = false;
      }
    });
    const deliveries = w.recent_deliveries || [];
    put(box,
      pageHead('Webhook', h('span', { class: 'mono' }, w.endpoint), refresh, del),
      status,
      panel('Details', kv([
        ['ID', copyable(w.id, 'webhook ID')],
        ['Endpoint', copyable(w.endpoint, 'endpoint URL')],
        ['Events', eventChips(w.events)],
        ['Created', exact(w.created_at)],
      ])),
      panel('Recent deliveries', deliveries.length
        ? dataTable([
          { label: 'Event', cell: (d) => h('span', { class: 'mono' }, d.type) },
          { label: 'Status', class: 'col-status', cell: (d) => badge(d.status) },
          { label: 'Attempts', class: 'col-num', cell: (d) => String(d.attempts ?? 0) },
          { label: 'Last HTTP status', class: 'col-num', cell: (d) => (d.last_status ? String(d.last_status) : muted('—')) },
          { label: 'Last error', cell: (d) => (d.last_error ? h('span', { class: 'mono small' }, d.last_error) : muted('—')) },
          { label: 'Created', class: 'col-time', cell: (d) => when(d.created_at) },
          { label: 'Delivered', class: 'col-time', cell: (d) => when(d.delivered_at) },
        ], deliveries, 'Recent deliveries').el
        : emptyState('No deliveries yet. They appear here when an event matches this webhook.')));
  };
  render(await ctx.api('GET', path));
  ctx.mount(backLink('#/webhooks', 'All webhooks'), box);
}

// ---- API keys ----

function keyForm(ctx, onCreated) {
  const name = input({ name: 'name', required: true, placeholder: 'support-agent' });
  const full = h('input', { type: 'radio', name: 'permission', value: 'full_access', id: nextID('perm') });
  const sending = h('input', { type: 'radio', name: 'permission', value: 'sending_access', id: nextID('perm'), checked: true });
  const perHour = input({ name: 'max_per_hour', type: 'number', min: '1', step: '1', inputmode: 'numeric', placeholder: 'No limit' });
  const perDay = input({ name: 'max_per_day', type: 'number', min: '1', step: '1', inputmode: 'numeric', placeholder: 'No limit' });
  const allowTo = textarea({ name: 'allowed_recipients', placeholder: '*@acme.com\nops@example.org' });
  const allowFrom = textarea({ name: 'allowed_from', placeholder: 'agent@acme.com' });
  const status = formStatus();
  const form = h('form', { class: 'form', novalidate: true },
    field('Name', name, 'Who or what uses this key, e.g. the agent\'s name.'),
    h('fieldset', { class: 'fieldset' },
      h('legend', {}, 'Access'),
      h('div', { class: 'check' }, sending, h('label', { for: sending.id }, h('strong', {}, 'Sending only'),
        muted(' Send and read its own emails, list domains, read and reply in inboxes.'))),
      h('div', { class: 'check' }, full, h('label', { for: full.id }, h('strong', {}, 'Full access'),
        muted(' Everything, including domains, webhooks and API keys.')))),
    h('fieldset', { class: 'fieldset' },
      h('legend', {}, 'Sending limits'),
      h('p', { class: 'hint' }, 'Optional. Enforced by the server; empty means no limit. Patterns match lowercase addresses, and * matches any characters in the local part or the domain.'),
      h('div', { class: 'grid-2' },
        field('Max recipients per hour', perHour),
        field('Max recipients per day', perDay),
        field('Allowed recipients', allowTo, 'One pattern per line.'),
        field('Allowed senders', allowFrom, 'One pattern per line.'))),
    h('div', { class: 'form-actions' }, h('button', { type: 'submit', class: 'btn btn-primary' }, 'Create key')),
    status);
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    status.replaceChildren();
    if (!name.value.trim()) {
      status.replaceChildren(h('p', { class: 'field-error' }, 'Give the key a name.'));
      return;
    }
    let policy;
    try {
      policy = L.buildPolicy({
        maxPerHour: perHour.value, maxPerDay: perDay.value,
        allowedRecipients: allowTo.value, allowedFrom: allowFrom.value,
      });
    } catch (err) {
      status.replaceChildren(h('p', { class: 'field-error' }, err.message));
      return;
    }
    await withBusy(form, async () => {
      try {
        const res = await ctx.api('POST', '/api-keys', {
          name: name.value.trim(), permission: full.checked ? 'full_access' : 'sending_access', policy,
        });
        if (!ctx.live) return;
        form.reset();
        onCreated(res);
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'API keys') || '');
      }
    });
  });
  return form;
}

async function pageAPIKeys(ctx) {
  // Load first: without the endpoint or the permission, the page is just
  // the calm notice from the router.
  let res = await ctx.api('GET', '/api-keys');
  const reveal = h('div');
  const list = h('section', { class: 'panel' });
  const listHead = h('h2', { class: 'panel-title' }, 'Keys');
  const status = formStatus();
  const renderList = () => {
    const rows = res.data || [];
    const key = getKey();
    const table = rows.length
      ? dataTable([
        { label: 'Name', class: 'col-main', cell: (k) => h('strong', {}, k.name || '(unnamed)') },
        {
          label: 'Key',
          cell: (k) => h('span', {}, mono(`${k.prefix || ''}…`), L.isCurrentKey(k.prefix, key) ? h('span', { class: 'badge tone-info' }, 'This session') : null),
        },
        {
          label: 'Access',
          cell: (k) => h('span', { class: `badge tone-${k.permission === 'sending_access' ? 'info' : 'warn'}` }, L.statusLabel(k.permission || 'full_access')),
        },
        { label: 'Limits', cell: (k) => L.policySummary(k.policy).join(' · ') },
        { label: 'Created', class: 'col-time', cell: (k) => when(k.created_at) },
        { label: 'Last used', class: 'col-time', cell: (k) => when(k.last_used_at, 'never') },
        { label: 'Actions', class: 'col-actions', cell: (k) => revokeButton(k) },
      ], rows, 'API keys').el
      : emptyState('No keys.');
    list.replaceChildren(listHead, status, table);
  };
  const reload = async () => {
    try {
      res = await ctx.api('GET', '/api-keys');
      if (ctx.live) renderList();
    } catch (err) {
      if (ctx.live) status.replaceChildren(errorView(err, 'API keys') || '');
    }
  };
  const revokeButton = (k) => {
    const current = L.isCurrentKey(k.prefix, getKey());
    const btn = h('button', { type: 'button', class: 'btn btn-small btn-danger', 'aria-label': `Revoke ${k.name || k.prefix}` }, 'Revoke');
    btn.addEventListener('click', async () => {
      const q = current
        ? `Revoke ${k.name}? This session uses it, so you will be signed out.`
        : `Revoke ${k.name}? Anything using it stops working at once.`;
      if (!window.confirm(q)) return;
      btn.disabled = true;
      try {
        await ctx.api('DELETE', `/api-keys/${encodeURIComponent(k.id)}`);
        if (current) {
          signOut('You revoked the key this session used.');
          return;
        }
        announce(`${k.name} revoked`);
        await reload();
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'API keys') || '');
        btn.disabled = false;
      }
    });
    return btn;
  };
  renderList();
  ctx.mount(
    pageHead('API keys', 'Give each agent its own key with only the access it needs, so you can revoke one without touching the others.'),
    reveal,
    h('details', { class: 'panel collapsible' }, h('summary', {}, h('span', { class: 'panel-title' }, 'Create a key')),
      keyForm(ctx, (created) => {
        reveal.replaceChildren(secretReveal(`Key for ${created.name}`, created.token, 'API key',
          'Copy it now. Only a hash is stored, so it cannot be shown again.'));
        reload();
      })),
    list);
}

// ---- inboxes ----

const inboxAddress = (i) => i.address || i.email || i.id;

async function pageInboxes(ctx) {
  const address = input({ name: 'address', required: true, inputmode: 'email', placeholder: 'agent@example.com' });
  const status = formStatus();
  const form = h('form', { class: 'form', novalidate: true },
    h('div', { class: 'row' }, field('Address', address, 'On one of your domains, whose MX points at your inbound server.'),
      h('div', { class: 'row-end' }, h('button', { type: 'submit', class: 'btn btn-primary' }, 'Add inbox'))),
    status);
  const list = h('section', { class: 'panel' });
  const listHead = h('h2', { class: 'panel-title' }, 'Inboxes');
  const listStatus = formStatus();
  let prefetched = null;
  const loadList = () => fill(ctx, list, listHead, 'inboxes', async () => {
    const res = prefetched || await ctx.api('GET', '/inboxes');
    prefetched = null;
    const rows = res.data || [];
    if (!rows.length) return emptyState('No inboxes yet.');
    return [listStatus, dataTable([
      { label: 'Address', class: 'col-main', cell: (i) => link(route('inboxes', i.id), h('span', { class: 'mono' }, inboxAddress(i))) },
      { label: 'Created', class: 'col-time', cell: (i) => when(i.created_at) },
      { label: 'Actions', class: 'col-actions', cell: (i) => deleteInbox(i) },
    ], rows, 'Inboxes').el];
  });
  const deleteInbox = (i) => {
    const btn = h('button', { type: 'button', class: 'btn btn-small btn-danger', 'aria-label': `Delete ${inboxAddress(i)}` }, 'Delete');
    btn.addEventListener('click', async () => {
      if (!window.confirm(`Delete the inbox ${inboxAddress(i)}? Mail to it will be refused.`)) return;
      btn.disabled = true;
      try {
        await ctx.api('DELETE', `/inboxes/${encodeURIComponent(i.id)}`);
        loadList();
      } catch (err) {
        if (ctx.live) listStatus.replaceChildren(errorView(err, 'inboxes') || '');
        btn.disabled = false;
      }
    });
    return btn;
  };
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    status.replaceChildren();
    if (!address.value.trim()) {
      status.replaceChildren(h('p', { class: 'field-error' }, 'Enter an address.'));
      return;
    }
    await withBusy(form, async () => {
      try {
        await ctx.api('POST', '/inboxes', { address: address.value.trim() });
        if (!ctx.live) return;
        form.reset();
        loadList();
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'inboxes') || '');
      }
    });
  });
  // Without the endpoint the router shows the calm notice instead.
  prefetched = await ctx.api('GET', '/inboxes');
  ctx.mount(
    pageHead('Inboxes', 'Addresses that receive mail for your agents. Replies are threaded to the email they answer.'),
    h('details', { class: 'panel collapsible' }, h('summary', {}, h('span', { class: 'panel-title' }, 'Add an inbox')), form),
    list);
  await loadList();
}

async function pageInbox(ctx, id) {
  if (!L.isUUID(id)) return pageNotFound(ctx, 'That is not an inbox ID.');
  const fetchPage = (before) => ctx.api('GET', L.listPath(`/inboxes/${encodeURIComponent(id)}/messages`, { limit: PAGE_SIZE, before }));
  const first = await fetchPage();
  let title = 'Inbox';
  try {
    const all = await ctx.api('GET', '/inboxes');
    const found = ((all && all.data) || []).find((i) => i.id === id);
    if (found) title = inboxAddress(found);
  } catch (err) {
    if (isAbort(err)) throw err;
  }
  const rows = first.data || [];
  const refresh = h('button', { type: 'button', class: 'btn', onclick: () => render() }, 'Refresh');
  let content = emptyState('No messages yet. Mail sent to this address appears here.');
  if (rows.length) {
    const table = dataTable([
      { label: 'From', class: 'col-addr', cell: (m) => mono(m.from || '—') },
      {
        label: 'Subject',
        class: 'col-main',
        cell: (m) => h('span', { class: 'subject-cell' }, link(route('messages', m.id), m.subject || '(no subject)'),
          m.snippet ? h('span', { class: 'snippet' }, L.truncate(m.snippet, 160)) : null),
      },
      { label: 'Thread', cell: (m) => (m.in_reply_to_email_id ? badge('received', 'Reply to your email') : muted('New')) },
      { label: 'Received', class: 'col-time', cell: (m) => when(m.received_at) },
    ], rows, 'Messages');
    content = [table.el, pager(ctx, table, first, fetchPage, 'inbox messages')];
  }
  ctx.mount(
    backLink('#/inboxes', 'All inboxes'),
    pageHead(title, 'Received messages, newest first.', refresh),
    panel('Messages', content));
}

async function downloadAttachment(ctx, messageID, att) {
  const res = await request(getKey(), 'GET',
    `/messages/${encodeURIComponent(messageID)}/attachments/${encodeURIComponent(att.index)}`, undefined,
    { signal: ctx.signal, raw: true });
  // Always a download, never rendered: the content type from the sender is
  // ignored, so an HTML attachment cannot run in this origin.
  const blob = new Blob([await res.arrayBuffer()], { type: 'application/octet-stream' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = L.safeFilename(att.filename);
  a.rel = 'noopener';
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 30000);
}

function messageBody(m) {
  const parts = [];
  if (m.text) parts.push(h('pre', { class: 'body' }, m.text));
  if (m.html) {
    if (!m.text) parts.push(notice('calm', 'HTML-only message', 'It is shown as source below. The dashboard never renders mail as HTML.'));
    parts.push(h('details', { class: 'source', open: !m.text }, h('summary', {}, 'HTML source'), h('pre', { class: 'body' }, m.html)));
  }
  if (!m.text && !m.html) parts.push(emptyState('No body. It may have been purged after the retention window.'));
  return parts;
}

function attachmentList(ctx, m, status) {
  const atts = m.attachments || [];
  if (!atts.length) return null;
  return panel(`Attachments (${atts.length})`, h('ul', { class: 'attachments' }, atts.map((a) => {
    const btn = h('button', { type: 'button', class: 'btn btn-small', 'aria-label': `Download ${a.filename || 'attachment'}` }, 'Download');
    btn.addEventListener('click', async () => {
      btn.disabled = true;
      try {
        await downloadAttachment(ctx, m.id, a);
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'attachments') || '');
      } finally {
        btn.disabled = false;
      }
    });
    return h('li', {}, h('span', { class: 'mono' }, a.filename || `attachment ${a.index}`),
      muted(` ${a.content_type || ''} ${L.formatBytes(a.size)}`), btn);
  })));
}

function replyForm(ctx, m) {
  const text = textarea({ name: 'text', required: true, rows: '5', spellcheck: 'true' });
  const status = formStatus();
  const form = h('form', { class: 'form', novalidate: true },
    field('Your reply', text, 'Sent from this inbox to the sender (or their Reply-To) as "Re: …", in the same thread.'),
    h('div', { class: 'form-actions' }, h('button', { type: 'submit', class: 'btn btn-primary' }, 'Send reply')),
    status);
  let idem = randomID();
  form.addEventListener('input', () => { idem = randomID(); });
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    status.replaceChildren();
    if (!text.value.trim()) {
      status.replaceChildren(h('p', { class: 'field-error' }, 'Write a reply first.'));
      return;
    }
    await withBusy(form, async () => {
      try {
        const res = await ctx.api('POST', `/messages/${encodeURIComponent(m.id)}/reply`, { text: text.value }, { idempotencyKey: idem });
        if (!ctx.live) return;
        form.reset();
        idem = randomID();
        status.replaceChildren(notice('ok', 'Reply queued', null, h('p', {}, link(route('emails', res.id), 'Track its delivery'))));
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'replies') || '');
      }
    });
  });
  return form;
}

async function threadPanel(ctx, m) {
  if (!m.thread_id || !m.inbox_id) return null;
  let items = [];
  try {
    const res = await ctx.api('GET', L.listPath(`/inboxes/${encodeURIComponent(m.inbox_id)}/messages`, { thread_id: m.thread_id, limit: 50 }));
    items = (res && res.data) || [];
  } catch (err) {
    if (isAbort(err)) throw err;
    return null;
  }
  items.sort((a, b) => new Date(a.received_at) - new Date(b.received_at));
  if (items.length < 2 && !m.in_reply_to_email_id) return null;
  return panel('Thread',
    m.in_reply_to_email_id
      ? h('p', {}, 'Answers ', link(route('emails', m.in_reply_to_email_id), 'an email you sent'), '.')
      : null,
    h('ol', { class: 'thread' }, items.map((t) => {
      const current = t.id === m.id;
      return h('li', { class: current ? 'thread-item current' : 'thread-item', 'aria-current': current ? 'true' : null },
        h('div', { class: 'thread-head' },
          current ? h('strong', {}, t.from || '—') : link(route('messages', t.id), t.from || '—'),
          muted(' · '), when(t.received_at)),
        t.snippet ? h('p', { class: 'snippet' }, L.truncate(t.snippet, 200)) : null);
    })));
}

async function pageMessage(ctx, id) {
  if (!L.isUUID(id)) return pageNotFound(ctx, 'That is not a message ID.');
  const m = await ctx.api('GET', `/messages/${encodeURIComponent(id)}`);
  const status = formStatus();
  const thread = await threadPanel(ctx, m);
  const hd = m.headers || {};
  ctx.mount(
    m.inbox_id ? backLink(route('inboxes', m.inbox_id), 'Inbox') : backLink('#/inboxes', 'Inboxes'),
    pageHead(m.subject || '(no subject)', h('span', {}, 'From ', mono(m.from || '—'), ' · ', when(m.received_at))),
    status,
    thread,
    panel('Message',
      kv([
        ['From', mono(m.from || '—')],
        ['To', mono(L.addressList(m.to).join(', '))],
        ['Cc', L.addressList(m.cc).length ? mono(L.addressList(m.cc).join(', ')) : null],
        ['Date', hd.date || null],
        ['Received', exact(m.received_at)],
        ['Message-ID', hd['message-id'] ? mono(hd['message-id']) : null],
        ['In-Reply-To', hd['in-reply-to'] ? mono(hd['in-reply-to']) : null],
      ]),
      messageBody(m)),
    attachmentList(ctx, m, status),
    panel('Reply', replyForm(ctx, m)));
}

function pageNotFound(ctx, detail) {
  ctx.mount(pageHead('Not found', detail || 'There is no page at this address.'),
    h('p', {}, link('#/emails', 'Go to emails')));
}

// ---- router ----

const PAGES = {
  emails: { title: 'Emails', what: 'emails', render: (ctx, id) => (id ? pageEmail(ctx, id) : pageEmails(ctx)) },
  domains: { title: 'Domains', what: 'domains', render: (ctx, id) => (id ? pageDomain(ctx, id) : pageDomains(ctx)) },
  webhooks: { title: 'Webhooks', what: 'webhooks', render: (ctx, id) => (id ? pageWebhook(ctx, id) : pageWebhooks(ctx)) },
  'api-keys': { title: 'API keys', what: 'API keys', render: (ctx, id) => (id ? pageNotFound(ctx) : pageAPIKeys(ctx)) },
  inboxes: { title: 'Inboxes', what: 'inboxes', render: (ctx, id) => (id ? pageInbox(ctx, id) : pageInboxes(ctx)) },
  messages: { title: 'Message', what: 'inboxes', nav: 'inboxes', render: (ctx, id) => (id ? pageMessage(ctx, id) : pageNotFound(ctx)) },
  notfound: { title: 'Not found', what: 'this', render: (ctx) => pageNotFound(ctx) },
};

let current = null;
let navigated = false;

async function render() {
  // A hash that is not a route (the skip link's "#main") keeps the current
  // page, or shows emails when nothing is shown yet.
  const r = L.parseRoute(location.hash) || (current ? null : { page: 'emails', id: '' });
  if (!r) return;
  if (!getKey()) {
    showSignIn();
    return;
  }
  showApp();
  if (current) current.abort();
  current = new AbortController();
  const ctx = makeContext(current.signal, navigated);
  navigated = true;
  const page = PAGES[r.page];
  for (const a of document.querySelectorAll('[data-nav]')) {
    if (a.dataset.nav === (page.nav || r.page)) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
  document.title = `${page.title} · OpenMailWire`;
  ctx.mount(loadingLine());
  try {
    await page.render(ctx, r.id);
  } catch (err) {
    if (!ctx.live || isAbort(err)) return;
    ctx.mount(pageHead(page.title), errorView(err, page.what));
  }
}

// ---- sign-in ----

function showApp() {
  $('#signin').hidden = true;
  $('#topbar').hidden = false;
  $('#main').hidden = false;
  $('#key-chip').textContent = L.keyLabel(getKey());
}

function showSignIn(message) {
  if (current) current.abort();
  current = null;
  $('#main').replaceChildren(); // drop anything shown, including one-time secrets
  $('#main').hidden = true;
  $('#topbar').hidden = true;
  $('#signin').hidden = false;
  const msg = $('#signin-message');
  msg.replaceChildren(message ? notice('calm', 'Signed out', message) : '');
  document.title = 'Sign in · OpenMailWire';
  $('#api-key').focus();
}

function signOut(message) {
  setKey('');
  navigated = false;
  showSignIn(message);
}

function setupSignIn() {
  const form = $('#signin-form');
  const keyInput = $('#api-key');
  const status = $('#signin-error');
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    status.replaceChildren();
    const key = keyInput.value.trim();
    const problem = L.checkKeyFormat(key);
    if (problem) {
      status.replaceChildren(h('p', { class: 'field-error' }, problem));
      keyInput.focus();
      return;
    }
    await withBusy(form, async () => {
      try {
        await request(key, 'GET', '/domains');
        keyInput.value = '';
        $('#signin-message').replaceChildren();
        setKey(key);
        navigated = true; // move focus to the page heading
        render();
      } catch (err) {
        const c = L.classifyError(err, 'domains');
        const text = c.kind === 'auth' ? 'This key is invalid or was revoked.' : `${c.title}. ${c.detail}`;
        status.replaceChildren(h('p', { class: 'field-error' }, text));
        keyInput.focus();
      }
    });
  });
  $('#sign-out').addEventListener('click', () => signOut('You signed out. The key was removed from this tab.'));
}

async function showVersion() {
  try {
    const res = await fetch(`${API}health`, { credentials: 'omit', cache: 'no-store' });
    const data = await res.json();
    if (data && data.version) $('#version').textContent = `Control plane ${data.version}`;
  } catch {
    // The footer simply shows no version.
  }
}

setupSignIn();
window.addEventListener('hashchange', render);
render();
showVersion();
