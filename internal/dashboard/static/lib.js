// Pure helpers for the dashboard: formatting, status mapping, routing and
// request building. Nothing here touches the DOM, so Node tests it directly
// (internal/dashboard/tests).

// ---- statuses ----

// Recipient statuses after which nothing more will happen.
export const FINAL_STATUSES = new Set([
  'delivered', 'bounced', 'expired', 'suppressed', 'unknown', 'canceled', 'complained',
]);

const TONES = {
  // good
  delivered: 'ok', late_report_delivered: 'ok', verified: 'ok', received: 'ok',
  // in progress
  queued: 'info', sending: 'info', scheduled: 'info', pending: 'info',
  // needs attention, may still resolve
  deferred: 'warn', unknown_retry: 'warn', lease_expired: 'warn', unknown: 'warn',
  late_report_deferred: 'warn', late_report_unknown: 'warn', throttled: 'warn', missing: 'warn',
  opportunistic: 'warn',
  // final and bad
  bounced: 'bad', late_report_bounced: 'bad', failed: 'bad', expired: 'bad',
  suppressed: 'bad', complained: 'bad',
  // neutral
  canceled: 'muted', throttle_relaxed: 'muted', none: 'muted',
};

// statusTone maps any status, outcome or event type to a colour family:
// ok, info, warn, bad or muted.
export function statusTone(status) {
  return TONES[String(status || '').toLowerCase()] || 'muted';
}

const LABELS = {
  late_report_delivered: 'Late report: delivered',
  late_report_deferred: 'Late report: deferred',
  late_report_bounced: 'Late report: bounced',
  late_report_unknown: 'Late report: unknown',
  unknown_retry: 'Unknown, will retry',
  lease_expired: 'Worker lost',
  full_access: 'Full access',
  sending_access: 'Sending only',
  throttle_relaxed: 'Throttle relaxed',
};

// statusLabel turns an API status ("unknown_retry", "email.bounced") into
// words for people.
export function statusLabel(status) {
  const s = String(status || '');
  if (!s) return '—';
  if (LABELS[s]) return LABELS[s];
  const words = s.replace(/^email\./, '').replace(/[_.]+/g, ' ').trim();
  return words.charAt(0).toUpperCase() + words.slice(1);
}

export function isFinal(status) {
  return FINAL_STATUSES.has(String(status || ''));
}

// isPending reports whether any recipient of an email may still change.
export function isPending(email) {
  const rs = (email && email.recipients) || [];
  return rs.some((r) => !isFinal(r.status));
}

// ---- time ----

// formatDateTime gives "2026-11-18 14:03:22" in the browser's zone, or in
// timeZone when given (tests pass "UTC").
export function formatDateTime(value, timeZone) {
  if (!value) return '—';
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return String(value);
  const parts = new Intl.DateTimeFormat('en-CA', {
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
    hourCycle: 'h23', timeZone,
  }).formatToParts(d);
  const get = (t) => (parts.find((p) => p.type === t) || {}).value || '';
  return `${get('year')}-${get('month')}-${get('day')} ${get('hour')}:${get('minute')}:${get('second')}`;
}

// relativeTime gives "3 min ago" or "in 2 h".
export function relativeTime(value, now = Date.now()) {
  if (!value) return '—';
  const t = new Date(value).getTime();
  if (Number.isNaN(t)) return String(value);
  const diff = Math.round((t - now) / 1000);
  const abs = Math.abs(diff);
  if (abs < 5) return diff <= 0 ? 'just now' : 'in a few seconds';
  let n;
  let unit;
  if (abs < 60) { n = abs; unit = 's'; }
  else if (abs < 3600) { n = Math.floor(abs / 60); unit = 'min'; }
  else if (abs < 86400) { n = Math.floor(abs / 3600); unit = 'h'; }
  else { n = Math.floor(abs / 86400); unit = 'd'; }
  return diff < 0 ? `${n} ${unit} ago` : `in ${n} ${unit}`;
}

// formatDuration gives "850 ms", "4.2 s" or "3 min 5 s".
export function formatDuration(ms) {
  if (typeof ms !== 'number' || !Number.isFinite(ms) || ms < 0) return '';
  if (ms < 1000) return `${Math.round(ms)} ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)} s`;
  const m = Math.floor(ms / 60000);
  const s = Math.round((ms % 60000) / 1000);
  return s ? `${m} min ${s} s` : `${m} min`;
}

// ---- values ----

export function formatBytes(n) {
  if (typeof n !== 'number' || !Number.isFinite(n) || n < 0) return '';
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

export function formatCount(n) {
  return Number(n).toLocaleString('en-US');
}

export function truncate(s, n) {
  const str = String(s ?? '');
  return str.length > n ? `${str.slice(0, Math.max(0, n - 1))}…` : str;
}

// addressList accepts a string, an array or nothing.
export function addressList(v) {
  if (v == null || v === '') return [];
  return (Array.isArray(v) ? v : [v]).map(String).filter(Boolean);
}

// summarizeAddresses gives "a@x.com +2".
export function summarizeAddresses(v, max = 1) {
  const list = addressList(v);
  if (!list.length) return '—';
  const shown = list.slice(0, max).join(', ');
  return list.length > max ? `${shown} +${list.length - max}` : shown;
}

// smtpReply joins an SMTP code, enhanced code and response without repeating
// a code the response already starts with.
export function smtpReply(code, enhanced, response) {
  const resp = String(response || '').trim();
  const parts = [];
  if (code != null && code !== '' && !resp.startsWith(String(code))) parts.push(String(code));
  if (enhanced && !resp.includes(enhanced)) parts.push(enhanced);
  if (resp) parts.push(resp);
  return parts.join(' ');
}

// eventDetails turns an event's data into [key, text] pairs. Fields already
// shown on the attempt the event came from are left out.
const ATTEMPT_FIELDS = new Set(['attempt_id', 'smtp_code', 'enhanced_code', 'response', 'mx_host']);

export function eventDetails(data, { fromAttempt = false } = {}) {
  if (!data || typeof data !== 'object' || Array.isArray(data)) return [];
  const out = [];
  for (const [k, v] of Object.entries(data)) {
    if (v == null || v === '') continue;
    if (fromAttempt && ATTEMPT_FIELDS.has(k)) continue;
    const text = typeof v === 'object' ? JSON.stringify(v) : String(v);
    out.push([k.replace(/_/g, ' '), truncate(text, 300)]);
  }
  return out;
}

// keyLabel shows the first 10 characters of a key: the same prefix the API
// lists for each key, so a person can match them.
export function keyLabel(key) {
  const k = String(key || '');
  return k.length > 10 ? `${k.slice(0, 10)}…` : k;
}

export function isCurrentKey(prefix, key) {
  return Boolean(prefix) && String(key || '').startsWith(prefix);
}

// checkKeyFormat returns why a pasted API key cannot be right, or "".
export function checkKeyFormat(key) {
  const k = String(key || '');
  if (!k) return 'Paste an API key.';
  if (k.startsWith('omwk_')) return 'That is a worker token (omwk_…). Sign in with an API key (omw_…).';
  if (!k.startsWith('omw_')) return 'API keys start with omw_.';
  if (!/^[\x21-\x7e]+$/.test(k)) return 'The key contains spaces or characters that cannot be part of a key.';
  return '';
}

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

export function isUUID(s) {
  return UUID_RE.test(String(s || '').trim().toLowerCase());
}

// safeFilename keeps a download name free of paths and control characters.
export function safeFilename(name) {
  const cleaned = String(name || '')
    .replace(/[\u0000-\u001f\u007f]/g, '')
    .replace(/[\\/]/g, '_')
    .replace(/^\.+/, '')
    .trim();
  return cleaned.slice(0, 200) || 'attachment';
}

// ---- policies ----

// parseList splits text on new lines and commas, trims, lowercases (unless
// lower is false) and drops empty entries and duplicates.
export function parseList(text, { lower = true } = {}) {
  const seen = new Set();
  const out = [];
  for (const raw of String(text || '').split(/[\n,]+/)) {
    const v = lower ? raw.trim().toLowerCase() : raw.trim();
    if (v && !seen.has(v)) {
      seen.add(v);
      out.push(v);
    }
  }
  return out;
}

function positiveInt(value, what) {
  const s = String(value ?? '').trim();
  if (s === '') return undefined;
  if (!/^\d+$/.test(s) || Number(s) < 1 || Number(s) > 1e9) {
    throw new Error(`${what} must be a whole number of 1 or more, or empty for no limit.`);
  }
  return Number(s);
}

function patterns(text, what) {
  const list = parseList(text);
  for (const p of list) {
    if (/\s/.test(p) || p.split('@').length !== 2 || p.startsWith('@') || p.endsWith('@')) {
      throw new Error(`${what}: "${p}" is not an address pattern. Use forms like ops@example.org or *@acme.com.`);
    }
  }
  return list;
}

// buildPolicy turns the API-key form into the policy JSON of the API. Empty
// fields mean no limit and are left out. It throws an Error whose message
// is ready to show.
export function buildPolicy({ maxPerHour, maxPerDay, allowedRecipients, allowedFrom } = {}) {
  const policy = {};
  const h = positiveInt(maxPerHour, 'Max per hour');
  const d = positiveInt(maxPerDay, 'Max per day');
  if (h !== undefined) policy.max_per_hour = h;
  if (d !== undefined) policy.max_per_day = d;
  const to = patterns(allowedRecipients, 'Allowed recipients');
  const from = patterns(allowedFrom, 'Allowed senders');
  if (to.length) policy.allowed_recipients = to;
  if (from.length) policy.allowed_from = from;
  return policy;
}

function listSummary(list) {
  return list.length > 2 ? `${list.slice(0, 2).join(', ')} +${list.length - 2}` : list.join(', ');
}

// policySummary describes a key's policy in short phrases.
export function policySummary(policy) {
  const p = policy && typeof policy === 'object' ? policy : {};
  const out = [];
  if (p.max_per_hour) out.push(`${formatCount(p.max_per_hour)} per hour`);
  if (p.max_per_day) out.push(`${formatCount(p.max_per_day)} per day`);
  if (Array.isArray(p.allowed_recipients) && p.allowed_recipients.length) out.push(`to ${listSummary(p.allowed_recipients)}`);
  if (Array.isArray(p.allowed_from) && p.allowed_from.length) out.push(`from ${listSummary(p.allowed_from)}`);
  return out.length ? out : ['No limits'];
}

// ---- timeline ----

function ts(v) {
  const t = new Date(v).getTime();
  return Number.isNaN(t) ? 0 : t;
}

// buildTimeline groups an email's attempts and events by recipient, oldest
// first. Attempts are numbered per recipient; an event recorded from an
// attempt's report carries that number. Events without a recipient belong
// to every recipient.
export function buildTimeline(email) {
  const recipients = (email && email.recipients) || [];
  const attempts = (email && email.attempts) || [];
  const events = (email && email.events) || [];
  const groups = new Map();
  const group = (addr) => {
    if (!groups.has(addr)) groups.set(addr, { recipient: addr, status: '', entries: [] });
    return groups.get(addr);
  };
  for (const r of recipients) group(r.email).status = r.status;

  const numbers = new Map();
  const sorted = [...attempts].sort((a, b) => ts(a.started_at) - ts(b.started_at));
  const counters = new Map();
  for (const a of sorted) {
    const n = (counters.get(a.recipient) || 0) + 1;
    counters.set(a.recipient, n);
    numbers.set(a.id, n);
    const started = ts(a.started_at);
    const finished = a.finished_at ? ts(a.finished_at) : 0;
    group(a.recipient).entries.push({
      kind: 'attempt', at: started, n, attempt: a,
      duration: finished && finished >= started ? finished - started : null,
    });
  }
  const shared = [];
  for (const e of events) {
    const attemptID = e.data && typeof e.data === 'object' ? e.data.attempt_id : undefined;
    const entry = { kind: 'event', at: ts(e.created_at), event: e, attemptNo: numbers.get(attemptID) || null };
    if (e.recipient) group(e.recipient).entries.push(entry);
    else shared.push(entry);
  }
  const out = [...groups.values()];
  for (const g of out) {
    g.entries.push(...shared);
    // Stable sort: at equal times an attempt comes before the event it caused.
    g.entries.sort((a, b) => a.at - b.at || (a.kind === b.kind ? 0 : a.kind === 'attempt' ? -1 : 1));
  }
  return out;
}

// ---- errors ----

// classifyError turns an API failure into something to show. err has
// status (0 for a network failure), code (the API's "name"), message and
// retryAfter (seconds). what names the thing the page manages, as in
// "This key can't manage API keys".
export function classifyError(err, what = 'this') {
  const status = Number(err && err.status) || 0;
  const code = (err && err.code) || '';
  const message = (err && err.message) || '';
  const base = { status, code, message };
  if (status === 0) {
    return { ...base, kind: 'network', title: "Can't reach the server", detail: 'Check your connection and that the control plane is running, then try again.' };
  }
  if (status === 401) {
    return { ...base, kind: 'auth', title: 'Signed out', detail: 'The server rejected this API key: it is invalid or was revoked. Sign in again.' };
  }
  if (status === 403 && code === 'insufficient_permission') {
    return { ...base, kind: 'forbidden', title: `This key can't manage ${what}`, detail: `It has sending access only. Sign in with a full-access key to manage ${what}.` };
  }
  // 405: the path exists for another method only (older servers). 404 from
  // the catch-all route, or without a JSON body: the endpoint is not there.
  if (status === 405 || (status === 404 && (!code || message === 'no such endpoint'))) {
    return { ...base, kind: 'unavailable', title: 'Not available on this server version', detail: `This control plane does not serve ${what} yet. Upgrade OpenMailWire to use this page.` };
  }
  if (status === 404) {
    return { ...base, kind: 'notfound', title: 'Not found', detail: message || 'It may have been deleted.' };
  }
  if (status === 429) {
    const wait = Number(err && err.retryAfter) > 0 ? ` Try again in ${err.retryAfter} s.` : '';
    const what = message || 'Too many requests.';
    return { ...base, kind: 'rate', title: 'Rate limit reached', detail: `${/[.!?]$/.test(what) ? what : `${what}.`}${wait}` };
  }
  if (status === 422 || status === 400) {
    return { ...base, kind: 'validation', title: 'Check the input', detail: message || 'The server refused the request.' };
  }
  if (status === 409) {
    return { ...base, kind: 'conflict', title: 'Conflict', detail: message || 'The request conflicts with the current state.' };
  }
  if (status === 403) {
    return { ...base, kind: 'refused', title: 'Refused', detail: message || 'The server refused the request.' };
  }
  if (status === 413) {
    return { ...base, kind: 'validation', title: 'Too large', detail: message || 'The request is too large.' };
  }
  if (status >= 500) {
    return { ...base, kind: 'server', title: 'The server had a problem', detail: message && message !== 'internal error' ? message : 'Try again in a moment. The control plane log has the details.' };
  }
  return { ...base, kind: 'other', title: `Request failed (${status})`, detail: message };
}

// ---- routing and requests ----

const PAGES = new Set(['emails', 'domains', 'webhooks', 'api-keys', 'inboxes', 'messages']);

// parseRoute reads "#/page/id". It returns null for hashes that are not
// routes (such as the skip link's "#main"), and page "notfound" for
// unknown routes.
export function parseRoute(hash) {
  const h = String(hash || '');
  if (h === '' || h === '#' || h === '#/') return { page: 'emails', id: '' };
  if (!h.startsWith('#/')) return null;
  const parts = h.slice(2).split('/').filter(Boolean);
  let decoded;
  try {
    decoded = parts.map((p) => decodeURIComponent(p));
  } catch {
    return { page: 'notfound', id: '' };
  }
  const [page = 'emails', id = '', ...rest] = decoded;
  if (!PAGES.has(page) || rest.length) return { page: 'notfound', id: '' };
  return { page, id };
}

// apiBase finds the API root from the dashboard's own path, so the
// dashboard also works behind a reverse proxy that adds a path prefix.
export function apiBase(pathname) {
  const p = String(pathname || '/');
  const i = p.lastIndexOf('/dashboard');
  return i >= 0 ? p.slice(0, i + 1) : '/';
}

// listPath builds "/emails?limit=25&before=…", leaving out empty values.
export function listPath(path, params = {}) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== '') q.set(k, String(v));
  }
  const s = q.toString();
  return s ? `${path}?${s}` : path;
}

// Webhook event types, including those added by newer servers.
export const WEBHOOK_EVENTS = [
  ['email.sent', 'Accepted and queued'],
  ['email.delivered', 'The receiving server accepted it'],
  ['email.delivery_delayed', 'A temporary failure; will retry'],
  ['email.bounced', 'A permanent failure'],
  ['email.failed', 'Expired before delivery'],
  ['email.suppressed', 'On the suppression list; not sent'],
  ['email.delivery_unknown', 'Outcome unknown; not retried'],
  ['email.canceled', 'Canceled before sending (v0.4+)'],
  ['email.complained', 'Marked as spam by the recipient (v0.5+)'],
  ['email.received', 'Mail arrived in an inbox (v0.5+)'],
];
