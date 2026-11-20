// Pure helpers for the dashboard: formatting, status mapping, routing and
// request building. Nothing here touches the DOM, so Node tests it directly
// (internal/dashboard/tests).

// dict makes a lookup table without a prototype, so API values such as
// "__proto__" or "constructor" find nothing.
const dict = (o) => Object.freeze(Object.assign(Object.create(null), o));

// ---- statuses ----

// Recipient statuses after which nothing more will happen.
export const FINAL_STATUSES = new Set([
  'delivered', 'bounced', 'expired', 'suppressed', 'unknown', 'canceled', 'complained', 'rejected',
]);

const TONES = dict({
  // good
  delivered: 'ok', late_report_delivered: 'ok', verified: 'ok', received: 'ok',
  approved: 'ok', authenticated: 'ok', active: 'ok', released: 'ok',
  // in progress
  queued: 'info', sending: 'info', scheduled: 'info', pending: 'info',
  // needs attention, may still resolve
  deferred: 'warn', unknown_retry: 'warn', lease_expired: 'warn', unknown: 'warn',
  late_report_deferred: 'warn', late_report_unknown: 'warn', throttled: 'warn', missing: 'warn',
  opportunistic: 'warn', held: 'warn', unauthenticated: 'warn',
  // final and bad
  bounced: 'bad', late_report_bounced: 'bad', failed: 'bad', expired: 'bad',
  suppressed: 'bad', complained: 'bad', rejected: 'bad',
  // neutral
  canceled: 'muted', throttle_relaxed: 'muted', none: 'muted', retiring: 'muted',
});

// statusTone maps any status, outcome or event type to a colour family:
// ok, info, warn, bad or muted.
export function statusTone(status) {
  return TONES[String(status || '').toLowerCase()] || 'muted';
}

const LABELS = dict({
  late_report_delivered: 'Late report: delivered',
  late_report_deferred: 'Late report: deferred',
  late_report_bounced: 'Late report: bounced',
  late_report_unknown: 'Late report: unknown',
  unknown_retry: 'Unknown, will retry',
  lease_expired: 'Worker lost',
  full_access: 'Full access',
  sending_access: 'Sending only',
  throttle_relaxed: 'Throttle relaxed',
  held: 'Held for approval',
  'email.held': 'Held for approval',
});

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

// isMoving reports whether an email may change by itself: a recipient is
// neither final nor held (held mail waits for a person).
export function isMoving(email) {
  const rs = (email && email.recipients) || [];
  return rs.some((r) => !isFinal(r.status) && r.status !== 'held');
}

// heldRecipients lists the recipients of an email that wait for approval.
export function heldRecipients(email) {
  return ((email && email.recipients) || []).filter((r) => r.status === 'held').map((r) => r.email);
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

// middle shortens a long value in the middle: "v=DKIM1; p=MIIB…IDAQAB".
export function middle(s, n = 64) {
  const str = String(s ?? '');
  if (str.length <= n) return str;
  const head = Math.ceil((n - 1) * 0.6);
  return `${str.slice(0, head)}…${str.slice(str.length - (n - 1 - head))}`;
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

function wholeNumber(value, what, min, max, hint) {
  const s = String(value ?? '').trim();
  if (s === '') return undefined;
  if (!/^\d+$/.test(s) || Number(s) < min || Number(s) > max) {
    throw new Error(`${what} must be a whole number from ${formatCount(min)} to ${formatCount(max)}${hint ? `, ${hint}` : ''}.`);
  }
  return Number(s);
}

// APPROVAL_RULES are the require_approval rules, in the order the API
// documents them, with words for people.
export const APPROVAL_RULES = [
  ['always', 'Always', 'Every email'],
  ['first_contact', 'First contact', 'A recipient this project has never sent to'],
  ['has_attachments', 'Has attachments', 'Any attachment, inline ones included'],
  ['has_links', 'Has links', 'A web link in the text or HTML'],
  ['recipients_over', 'Many recipients', 'More than a number of recipients'],
];

// holdReasonLabel names a hold reason ("first_contact" is "First contact").
export function holdReasonLabel(reason) {
  const r = APPROVAL_RULES.find(([k]) => k === reason);
  return r ? r[1] : statusLabel(reason);
}

// buildPolicy turns the API-key form into the policy JSON of the API. Empty
// fields mean no limit and are left out. It throws an Error whose message
// is ready to show. requireApproval holds the rule checkboxes and the
// recipientsOver number; canApprove and inboxes are v0.6 fields.
export function buildPolicy({
  maxPerHour, maxPerDay, allowedRecipients, allowedFrom,
  requireApproval, canApprove, inboxes,
} = {}) {
  const policy = {};
  const h = positiveInt(maxPerHour, 'Max per hour');
  const d = positiveInt(maxPerDay, 'Max per day');
  if (h !== undefined) policy.max_per_hour = h;
  if (d !== undefined) policy.max_per_day = d;
  const to = patterns(allowedRecipients, 'Allowed recipients');
  const from = patterns(allowedFrom, 'Allowed senders');
  if (to.length) policy.allowed_recipients = to;
  if (from.length) policy.allowed_from = from;
  const ra = requireApproval || {};
  const rules = {};
  if (ra.always) rules.always = true;
  if (ra.firstContact) rules.first_contact = true;
  if (ra.hasAttachments) rules.has_attachments = true;
  if (ra.hasLinks) rules.has_links = true;
  const over = wholeNumber(ra.recipientsOver, 'Hold when recipients are over', 0, 1000, 'or empty');
  if (over !== undefined) rules.recipients_over = over;
  if (Object.keys(rules).length) policy.require_approval = rules;
  if (canApprove) policy.can_approve = true;
  const boxes = patterns(inboxes, 'Inboxes');
  if (boxes.length) policy.inboxes = boxes;
  return policy;
}

// policyForm is the inverse of buildPolicy: a key's policy as form values,
// to edit it.
export function policyForm(policy) {
  const p = policy && typeof policy === 'object' ? policy : {};
  const ra = p.require_approval && typeof p.require_approval === 'object' ? p.require_approval : {};
  const lines = (v) => (Array.isArray(v) ? v.join('\n') : '');
  return {
    maxPerHour: p.max_per_hour ? String(p.max_per_hour) : '',
    maxPerDay: p.max_per_day ? String(p.max_per_day) : '',
    allowedRecipients: lines(p.allowed_recipients),
    allowedFrom: lines(p.allowed_from),
    requireApproval: {
      always: Boolean(ra.always),
      firstContact: Boolean(ra.first_contact),
      hasAttachments: Boolean(ra.has_attachments),
      hasLinks: Boolean(ra.has_links),
      recipientsOver: ra.recipients_over != null ? String(ra.recipients_over) : '',
    },
    canApprove: Boolean(p.can_approve),
    inboxes: lines(p.inboxes),
  };
}

function listSummary(list) {
  return list.length > 2 ? `${list.slice(0, 2).join(', ')} +${list.length - 2}` : list.join(', ');
}

// approvalSummary describes require_approval: "holds: first contact, links".
export function approvalSummary(rules) {
  if (!rules || typeof rules !== 'object') return '';
  const out = [];
  if (rules.always) out.push('every email');
  if (rules.first_contact) out.push('first contact');
  if (rules.has_attachments) out.push('attachments');
  if (rules.has_links) out.push('links');
  if (rules.recipients_over != null) out.push(`over ${formatCount(rules.recipients_over)} recipients`);
  return out.length ? `holds: ${out.join(', ')}` : '';
}

// policySummary describes a key's policy in short phrases.
export function policySummary(policy) {
  const p = policy && typeof policy === 'object' ? policy : {};
  const out = [];
  if (p.max_per_hour) out.push(`${formatCount(p.max_per_hour)} per hour`);
  if (p.max_per_day) out.push(`${formatCount(p.max_per_day)} per day`);
  if (Array.isArray(p.allowed_recipients) && p.allowed_recipients.length) out.push(`to ${listSummary(p.allowed_recipients)}`);
  if (Array.isArray(p.allowed_from) && p.allowed_from.length) out.push(`from ${listSummary(p.allowed_from)}`);
  const holds = approvalSummary(p.require_approval);
  if (holds) out.push(holds);
  if (p.can_approve) out.push('can approve');
  if (Array.isArray(p.inboxes) && p.inboxes.length) out.push(`inboxes ${listSummary(p.inboxes)}`);
  return out.length ? out : ['No limits'];
}

// ---- key access: allowed IPs and expiry ----

const IPV4_RE = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/;

function validIPv6(s) {
  if (!/^[0-9a-f:.]+$/i.test(s) || !s.includes(':')) return false;
  const halves = s.split('::');
  if (halves.length > 2) return false;
  const groups = halves.flatMap((x) => (x ? x.split(':') : []));
  let count = 0;
  for (const [i, g] of groups.entries()) {
    if (i === groups.length - 1 && g.includes('.')) {
      if (!IPV4_RE.test(g)) return false;
      count += 2;
    } else if (/^[0-9a-f]{1,4}$/i.test(g)) {
      count += 1;
    } else {
      return false;
    }
  }
  return halves.length === 2 ? count < 8 : count === 8;
}

// parseAllowedIPs reads one address or CIDR prefix per line (or comma) and
// returns the list, or null for none (every address allowed). It throws an
// Error whose message is ready to show.
export function parseAllowedIPs(text) {
  const list = parseList(text);
  if (!list.length) return null;
  if (list.length > 100) throw new Error('Allowed IPs: at most 100 entries.');
  for (const entry of list) {
    const [addr, bits, extra] = entry.split('/');
    const v4 = IPV4_RE.test(addr);
    const v6 = !v4 && validIPv6(addr);
    const max = v4 ? 32 : 128;
    const okBits = bits === undefined || (/^\d{1,3}$/.test(bits) && Number(bits) <= max);
    if ((!v4 && !v6) || !okBits || extra !== undefined) {
      throw new Error(`Allowed IPs: "${entry}" is not an address or CIDR prefix. Use forms like 203.0.113.7, 10.0.0.0/8 or 2001:db8::/32.`);
    }
  }
  return list;
}

const pad2 = (n) => String(n).padStart(2, '0');

// localInputValue turns an RFC 3339 time into the value of a
// datetime-local input, in the browser's zone ("2026-12-01T10:00").
export function localInputValue(value) {
  if (!value) return '';
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return '';
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

// expiryFromInput turns a datetime-local value into RFC 3339, or null when
// empty. The time must be in the future.
export function expiryFromInput(value, now = Date.now()) {
  const s = String(value || '').trim();
  if (!s) return null;
  const d = new Date(s);
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/.test(s) || Number.isNaN(d.getTime())) {
    throw new Error('Expires: enter a date and time.');
  }
  if (d.getTime() <= now) throw new Error('Expires: the time must be in the future.');
  return d.toISOString();
}

// keyAccessSummary describes a key's allowed IPs and expiry.
export function keyAccessSummary(key, now = Date.now()) {
  const k = key || {};
  const out = [];
  if (Array.isArray(k.allowed_ips) && k.allowed_ips.length) out.push(`from ${listSummary(k.allowed_ips)}`);
  if (k.expires_at) {
    const t = new Date(k.expires_at).getTime();
    out.push(t <= now ? `expired ${relativeTime(k.expires_at, now)}` : `expires ${relativeTime(k.expires_at, now)}`);
  }
  return out;
}

export function isExpired(key, now = Date.now()) {
  return Boolean(key && key.expires_at && new Date(key.expires_at).getTime() <= now);
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
export function classifyError(err, what = 'this', verb = 'manage') {
  const status = Number(err && err.status) || 0;
  const code = (err && err.code) || '';
  const message = (err && err.message) || '';
  const base = { status, code, message };
  if (status === 0) {
    return { ...base, kind: 'network', title: "Can't reach the server", detail: 'Check your connection and that the control plane is running, then try again.' };
  }
  if (status === 401) {
    const detail = code === 'api_key_expired'
      ? 'This API key has expired. Sign in with another key.'
      : 'The server rejected this API key: it is invalid or was revoked. Sign in again.';
    return { ...base, kind: 'auth', title: 'Signed out', detail };
  }
  if (status === 403 && code === 'insufficient_permission') {
    return { ...base, kind: 'forbidden', title: `This key can't ${verb} ${what}`, detail: `It has sending access only. Sign in with a full-access key to ${verb} ${what}.` };
  }
  if (status === 403 && code === 'cannot_approve_own_email') {
    return {
      ...base, kind: 'own', title: "You can't approve your own email",
      detail: 'This session signed in with the key that sent it, and no key approves its own emails. Approve it with another key that has can_approve, or with a full-access key.',
    };
  }
  if (status === 403 && code === 'sending_paused') {
    return { ...base, kind: 'paused', title: 'Sending is paused', detail: message || 'A circuit breaker paused sending. See Pauses.' };
  }
  if (status === 403 && code === 'ip_not_allowed') {
    return { ...base, kind: 'refused', title: 'Not allowed from this address', detail: message || "This key's allowed_ips do not include this address." };
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
  const dns = dict({
    dns_provider_auth_failed: 'Cloudflare refused the token',
    dns_zone_not_found: 'No matching zone in Cloudflare',
    dns_provider_unavailable: 'Cloudflare is not reachable',
  });
  if (dns[code]) {
    return { ...base, kind: status === 503 ? 'server' : 'validation', title: dns[code], detail: message || 'Check the token and its zone, then try again.' };
  }
  if (status === 422 || status === 400) {
    return { ...base, kind: 'validation', title: 'Check the input', detail: message || 'The server refused the request.' };
  }
  if (status === 409) {
    const titles = dict({
      not_held: 'Already decided',
      not_paused: 'Already resumed',
      dkim_record_not_found: 'The DNS record is not published yet',
    });
    return { ...base, kind: 'conflict', title: titles[code] || 'Conflict', detail: message || 'The request conflicts with the current state.' };
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

const PAGES = new Set([
  'overview', 'compose', 'mail',
  'emails', 'held', 'suppressions', 'pauses',
  'domains', 'inboxes', 'messages', 'webhooks', 'api-keys', 'settings',
  'insights', 'audit-log',
]);

// parseRoute reads "#/page/id". It returns null for hashes that are not
// routes (such as the skip link's "#main"), and page "notfound" for
// unknown routes.
export function parseRoute(hash) {
  const h = String(hash || '');
  if (h === '' || h === '#' || h === '#/') return { page: 'overview', id: '' };
  if (!h.startsWith('#/')) return null;
  const parts = h.slice(2).split('/').filter(Boolean);
  let decoded;
  try {
    decoded = parts.map((p) => decodeURIComponent(p));
  } catch {
    return { page: 'notfound', id: '' };
  }
  const [page = 'overview', id = '', ...rest] = decoded;
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
  ['email.held', 'Held for approval (v0.6+)'],
  ['email.approved', 'Approved and queued (v0.6+)'],
  ['email.rejected', 'Rejected by an approver (v0.6+)'],
  ['sending.paused', 'A circuit breaker paused sending (v0.6+)'],
];

// ---- rates and durations ----

function trimZeros(s) {
  return s.includes('.') ? s.replace(/\.?0+$/, '') : s;
}

// formatRate shows a fraction as a percentage: 0.966 is "96.6%", 0.05 is
// "5%", 0.0007 is "0.07%". null (no data) is "—".
export function formatRate(fraction) {
  if (fraction == null || fraction === '' || !Number.isFinite(Number(fraction))) return '—';
  const p = Number(fraction) * 100;
  if (p === 0) return '0%';
  const s = Math.abs(p) >= 1 ? p.toFixed(1) : p.toPrecision(2);
  return `${trimZeros(s)}%`;
}

// formatSeconds shows a duration in seconds: "350 ms", "2.1 s", "1 min 5 s".
export function formatSeconds(seconds) {
  if (seconds == null || seconds === '' || !Number.isFinite(Number(seconds))) return '—';
  return formatDuration(Number(seconds) * 1000) || '—';
}

// percentInput turns a fraction into the text of a percentage input:
// 0.001 is "0.1".
export function percentInput(fraction) {
  if (fraction == null || !Number.isFinite(Number(fraction))) return '';
  return String(Number((Number(fraction) * 100).toPrecision(8)));
}

// parsePercent reads a percentage typed by a person ("5", "0.1 %") and
// returns the fraction. It must be above 0 and at most 100.
export function parsePercent(text, what) {
  const s = String(text ?? '').trim().replace(/\s*%$/, '');
  if (!/^\d+(\.\d+)?$/.test(s) || Number(s) <= 0 || Number(s) > 100) {
    throw new Error(`${what} must be a percentage above 0 and at most 100, like 5 or 0.1.`);
  }
  return Number((Number(s) / 100).toPrecision(8));
}

// ---- project settings ----

// projectPatch compares the settings form with the current project and
// returns the PATCH /project body: only the fields that changed. It throws
// an Error whose message is ready to show.
export function projectPatch(current, values) {
  const cur = current || {};
  const cb = cur.circuit_breaker || {};
  const v = values || {};
  const patch = {};
  const cap = wholeNumber(v.maxPerRecipientPerDay, 'Emails per recipient per day', 1, 10000, 'or empty for no cap');
  const capValue = cap === undefined ? null : cap;
  if (capValue !== (cur.max_per_recipient_per_day ?? null)) patch.max_per_recipient_per_day = capValue;
  const breaker = {};
  const enabled = Boolean(v.enabled);
  if (enabled !== (cb.enabled ?? true)) breaker.enabled = enabled;
  const bounce = parsePercent(v.bounceRate, 'Bounce rate');
  if (bounce !== cb.bounce_rate) breaker.bounce_rate = bounce;
  const complaint = parsePercent(v.complaintRate, 'Complaint rate');
  if (complaint !== cb.complaint_rate) breaker.complaint_rate = complaint;
  const min = wholeNumber(v.minRecipients, 'Minimum recipients', 1, 1e9);
  if (min === undefined) throw new Error('Minimum recipients must be a whole number of 1 or more.');
  if (min !== cb.min_recipients) breaker.min_recipients = min;
  if (Object.keys(breaker).length) patch.circuit_breaker = breaker;
  return patch;
}

// ---- inbound trust ----

const TRUST = dict({
  authenticated: {
    tone: 'ok', label: 'Authenticated',
    detail: 'The From domain is proven by an aligned DKIM signature or SPF pass.',
  },
  unauthenticated: {
    tone: 'warn', label: 'Unauthenticated',
    detail: 'Nothing proves who sent it: the From address may be forged.',
  },
  failed: {
    tone: 'bad', label: 'Failed authentication',
    detail: "The From domain's DMARC policy (or SPF for an aligned envelope sender) says it did not send this.",
  },
});

// trustBadge maps a received message's trust verdict to a tone, a label and
// an explanation. Messages received before v0.6 have no verdict.
export function trustBadge(trust) {
  return TRUST[String(trust || '')] || { tone: 'muted', label: 'Not checked', detail: 'This message has no sender verdict (received before v0.6).' };
}

export const SUSPICIOUS_RULES = dict({
  override_instructions: 'Tries to override instructions',
  role_marker: 'Contains chat role markers (system:, assistant:)',
  addresses_agent: 'Addresses an AI agent directly',
  tool_instruction: 'Asks for tool calls or commands',
  conceal_from_user: 'Asks to hide something from the user',
  claims_approval: 'Claims approval or authority',
});

export function suspiciousLabel(rule) {
  return SUSPICIOUS_RULES[rule] || statusLabel(rule);
}

const yesNo = (b) => (b ? 'aligned' : 'not aligned');

// authRows turns a message's authentication_results into rows to show:
// [check, result, details]. Domains in it come from the message, so they
// are shown as text only.
export function authRows(results) {
  const r = results && typeof results === 'object' && !Array.isArray(results) ? results : {};
  const rows = [];
  const spf = (name, x) => {
    if (!x || typeof x !== 'object' || !x.result) return;
    rows.push([name, String(x.result), [x.domain, x.reason].filter(Boolean).join(' · ')]);
  };
  spf('SPF (envelope sender)', r.spf);
  spf('SPF (HELO)', r.spf_helo);
  const sigs = Array.isArray(r.dkim) ? r.dkim : [];
  for (const [i, d] of sigs.entries()) {
    if (!d || typeof d !== 'object') continue;
    const details = [d.domain && `d=${d.domain}`, d.selector && `s=${d.selector}`, d.domain ? yesNo(d.aligned) : '', d.reason]
      .filter(Boolean).join(' · ');
    rows.push([sigs.length > 1 ? `DKIM signature ${i + 1}` : 'DKIM', String(d.result || 'none'), details]);
  }
  if (!sigs.length && (r.spf || r.dmarc)) rows.push(['DKIM', 'none', 'no signature']);
  if (r.dmarc && typeof r.dmarc === 'object' && r.dmarc.result) {
    const m = r.dmarc;
    const details = [
      m.from_domain && `From domain ${m.from_domain}`,
      m.policy && `policy p=${m.policy}`,
      `SPF ${yesNo(m.spf_aligned)}`, `DKIM ${yesNo(m.dkim_aligned)}`,
    ].filter(Boolean).join(' · ');
    rows.push(['DMARC', String(m.result), details]);
  }
  return rows;
}

// authTone colours one SPF, DKIM or DMARC result.
export function authTone(result) {
  const r = String(result || '').toLowerCase();
  if (r === 'pass') return 'ok';
  if (r === 'fail' || r === 'permerror' || r === 'hardfail') return 'bad';
  if (r === 'softfail' || r === 'temperror') return 'warn';
  return 'muted';
}

// ---- sending ----

// buildSendBody turns the test-email form into the body of POST /emails
// and POST /emails/check.
export function buildSendBody({ from, to, subject, text, stream } = {}) {
  const body = {
    from: String(from || '').trim(),
    to: parseList(to, { lower: false }),
    subject: String(subject ?? ''),
    text: String(text ?? ''),
  };
  if (stream && stream !== 'transactional') body.stream = String(stream);
  return body;
}

// checkVerdict summarizes a POST /emails/check result.
export function checkVerdict(result) {
  const r = result || {};
  const warnings = Array.isArray(r.warnings) ? r.warnings.length : 0;
  if (r.would === 'refuse') return { tone: 'bad', title: 'Would be refused' };
  if (r.would === 'hold') return { tone: 'warn', title: 'Would be held for approval' };
  if (warnings) return { tone: 'warn', title: `Would send, with ${warnings === 1 ? 'a warning' : `${warnings} warnings`}` };
  return { tone: 'ok', title: 'Ready to send' };
}

// ---- insights ----

export const OUTCOMES = [
  ['accepted', 'Accepted'],
  ['deferred', 'Deferred'],
  ['bounced', 'Bounced'],
  ['unknown', 'Unknown'],
];

// outcomeShares splits a group's attempts into whole-number percentages
// that add up to 100 (largest remainder), for a stacked bar. Empty groups
// give an empty list.
export function outcomeShares(stats) {
  const s = stats || {};
  const counts = OUTCOMES.map(([k]) => Math.max(0, Number(s[k]) || 0));
  const total = counts.reduce((a, b) => a + b, 0);
  if (!total) return [];
  const raw = counts.map((c) => (c * 100) / total);
  const floor = raw.map(Math.floor);
  let left = 100 - floor.reduce((a, b) => a + b, 0);
  const order = raw.map((r, i) => [r - floor[i], i]).sort((a, b) => b[0] - a[0] || a[1] - b[1]);
  for (const [, i] of order) {
    if (left <= 0) break;
    floor[i] += 1;
    left -= 1;
  }
  return OUTCOMES.map(([key, label], i) => ({ key, label, count: counts[i], percent: floor[i] }))
    .filter((x) => x.count > 0);
}

// providerLabel names a mailbox provider ("google" is "Google").
export function providerLabel(p) {
  const names = dict({ google: 'Google', microsoft: 'Microsoft', yahoo: 'Yahoo', apple: 'Apple', zoho: 'Zoho', proton: 'Proton', fastmail: 'Fastmail' });
  const s = String(p || '');
  return names[s] || statusLabel(s || 'unknown');
}

export const INSIGHT_WINDOWS = [['24h', '24 hours'], ['7d', '7 days'], ['30d', '30 days']];

// insightWindow returns the window with that name, or an empty one.
export function insightWindow(insights, name) {
  const ws = (insights && Array.isArray(insights.windows)) ? insights.windows : [];
  return ws.find((w) => w && w.window === name)
    || { window: name, total: { attempts: 0, accepted: 0, deferred: 0, bounced: 0, unknown: 0 }, by_provider: [], by_sending_ip: [] };
}

// byAttempts sorts insight groups by attempts, most first.
export function byAttempts(groups) {
  return [...(Array.isArray(groups) ? groups : [])].sort((a, b) => (Number(b.attempts) || 0) - (Number(a.attempts) || 0));
}

// ---- pauses ----

export const PAUSE_SCOPES = dict({ api_key: 'API key', domain: 'Domain', project: 'Project' });
export const PAUSE_REASONS = dict({ bounce_rate: 'Bounce rate', complaint_rate: 'Complaint rate' });

// rateBar places a rate and its limit on one scale for a small bar: both
// as percentages of the bar's width. The scale leaves room past the larger.
export function rateBar(rate, limit) {
  const r = Math.max(0, Number(rate) || 0);
  const l = Math.max(0, Number(limit) || 0);
  const top = Math.max(r, l) * 1.25;
  if (!top) return { value: 0, limit: 0 };
  const pct = (x) => Math.round((x / top) * 1000) / 10;
  return { value: pct(r), limit: pct(l) };
}

// pauseCounts describes what tripped a pause: "45 bounces of 600 recipients".
export function pauseCounts(p) {
  const x = p || {};
  const n = (v) => formatCount(Number(v) || 0);
  const plural = (v, one, many) => (Number(v) === 1 ? one : many);
  const what = x.reason === 'complaint_rate'
    ? `${n(x.complaints)} ${plural(x.complaints, 'complaint', 'complaints')}`
    : `${n(x.bounces)} ${plural(x.bounces, 'bounce', 'bounces')}`;
  return x.recipients != null ? `${what} of ${n(x.recipients)} ${plural(x.recipients, 'recipient', 'recipients')}` : what;
}

// ---- DKIM ----

// dkimState reads GET /domains/{id}/dkim: whether a rotation waits for its
// record (step 2 next), and the Ed25519 key's state: off, pending or active.
export function dkimState(keys) {
  const list = Array.isArray(keys) ? keys : [];
  const next = list.find((k) => k && k.algorithm === 'rsa' && k.state === 'pending');
  const ed = list.find((k) => k && k.algorithm === 'ed25519');
  const retiring = list.filter((k) => k && k.state === 'retiring');
  return {
    rotation: next ? 'pending' : 'idle',
    next: next || null,
    ed25519: ed ? (ed.state === 'pending' ? 'pending' : 'active') : 'off',
    edKey: ed || null,
    retiring,
  };
}

// ---- audit log ----

export const AUDIT_ACTIONS = [
  'api_key.created', 'api_key.updated', 'api_key.revoked',
  'domain.created', 'domain.verified', 'domain.unverified', 'domain.deleted', 'domain.expired',
  'webhook.created', 'webhook.deleted', 'webhook.secret_rotated',
  'inbox.created', 'inbox.updated', 'inbox.deleted', 'message.released',
  'email.approve', 'email.reject', 'pause.resume', 'project.update',
  'suppression.add', 'suppression.delete',
];

// auditActor names who acted: a key's name when known.
export function auditActor(entry, keyNames) {
  const e = entry || {};
  if (e.actor === 'cli') return 'Operator (CLI)';
  if (e.actor === 'system') return 'System';
  const id = e.actor_key_id;
  if (!id) return 'API key';
  const name = keyNames && (keyNames instanceof Map ? keyNames.get(id) : keyNames[id]);
  return name ? `Key ${name}` : `Key ${String(id).slice(0, 8)}…`;
}

export function shortHash(h) {
  const s = String(h || '');
  return s.length > 12 ? `${s.slice(0, 12)}…` : s;
}

// ---- suppressions ----

// suppressionQuery keeps the filters that are set, trimmed: the query of
// GET /suppressions and its CSV export.
export function suppressionQuery({ address, domain, reason, stream } = {}) {
  const q = {};
  const a = String(address || '').trim().toLowerCase();
  const d = String(domain || '').trim().toLowerCase().replace(/^@/, '');
  const r = String(reason || '').trim().toLowerCase();
  const s = String(stream || '').trim();
  if (a) q.address = a;
  if (d) q.domain = d;
  if (r) q.reason = r;
  if (s === 'all' || s === 'broadcast') q.stream = s;
  return q;
}

// checkSuppressionReason returns why a reason name is refused, or "".
export function checkSuppressionReason(reason) {
  const r = String(reason || '').trim();
  if (!r) return '';
  return /^[a-z0-9_]{1,32}$/.test(r) ? '' : 'A reason is a short name of lowercase letters, digits and _, like customer_request.';
}

export const STREAM_LABELS = dict({ all: 'All mail', broadcast: 'Broadcast only', transactional: 'Transactional' });

// datedFilename gives "suppressions-2026-11-20.csv".
export function datedFilename(base, ext, now = new Date()) {
  const d = new Date(now);
  return `${base}-${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}.${ext}`;
}

// ---- webhooks ----

export const ROTATE_OVERLAPS = [
  [86400, '24 hours (default)'],
  [3600, '1 hour'],
  [21600, '6 hours'],
  [259200, '3 days'],
  [604800, '7 days'],
  [0, 'None: stop the old secret now (after a leak)'],
];

// webhookHealth describes a webhook endpoint's circuit breaker.
export function webhookHealth(w, now = Date.now()) {
  const x = w || {};
  if (x.paused_until && new Date(x.paused_until).getTime() > now) {
    return { tone: 'bad', label: 'Breaker open', detail: `No requests until ${formatDateTime(x.paused_until)}, then one at a time until one succeeds.` };
  }
  const f = Number(x.consecutive_failures) || 0;
  if (f > 0) return { tone: 'warn', label: `${f} failed in a row`, detail: 'At 5 failures in a row the circuit breaker opens.' };
  return { tone: 'ok', label: 'Healthy', detail: '' };
}

// ---- keyboard shortcuts ----

// SHORTCUTS: "g" then a key goes to a page.
export const SHORTCUTS = [
  ['o', 'overview', 'Overview'],
  ['c', 'compose', 'Compose'],
  ['m', 'mail', 'Mail'],
  ['e', 'emails', 'Emails'],
  ['h', 'held', 'Held emails'],
  ['d', 'domains', 'Domains'],
  ['b', 'inboxes', 'Inboxes'],
  ['w', 'webhooks', 'Webhooks'],
  ['k', 'api-keys', 'API keys'],
  ['s', 'suppressions', 'Suppressions'],
  ['p', 'pauses', 'Pauses'],
  ['i', 'insights', 'Insights'],
  ['a', 'audit-log', 'Audit log'],
  ['t', 'settings', 'Project settings'],
];

// shortcutRoute returns the route for "g" followed by key, or "".
export function shortcutRoute(prev, key) {
  if (prev !== 'g') return '';
  const hit = SHORTCUTS.find(([k]) => k === String(key || '').toLowerCase());
  return hit ? `#/${hit[1]}` : '';
}

// isTyping reports whether a keydown happened in a field, where shortcuts
// must not fire.
export function isTyping(target) {
  const t = target || {};
  const tag = String(t.tagName || '').toLowerCase();
  return tag === 'input' || tag === 'textarea' || tag === 'select' || Boolean(t.isContentEditable);
}

// ---- email list ----

// emailStatus is the status an email shows in lists.
export function emailStatus(e, now = Date.now()) {
  const x = e || {};
  if (x.scheduled_at && new Date(x.scheduled_at).getTime() > now && (!x.last_event || x.last_event === 'queued' || x.last_event === 'scheduled')) {
    return 'scheduled';
  }
  return x.last_event || 'queued';
}

function dayStart(ymd) {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(String(ymd || ''));
  return m ? new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3])).getTime() : NaN;
}

const domainOf = (addr) => {
  const s = String(addr || '').toLowerCase();
  const inner = s.includes('<') ? s.slice(s.lastIndexOf('<') + 1).replace(/>.*$/, '') : s;
  return inner.includes('@') ? inner.slice(inner.lastIndexOf('@') + 1).trim() : '';
};

// filterEmails filters loaded emails by text (subject, addresses, id),
// status, recipient domain, and a date range of local days.
export function filterEmails(rows, { q, status, domain, since, until } = {}, now = Date.now()) {
  const text = String(q || '').trim().toLowerCase();
  const dom = String(domain || '').trim().toLowerCase().replace(/^@/, '');
  const from = dayStart(since);
  const to = dayStart(until);
  return (Array.isArray(rows) ? rows : []).filter((e) => {
    if (status && emailStatus(e, now) !== status) return false;
    if (dom && !addressList(e.to).some((a) => {
      const d = domainOf(a);
      return d === dom || d.endsWith(`.${dom}`);
    })) return false;
    const t = new Date(e.created_at).getTime();
    if (!Number.isNaN(from) && !(t >= from)) return false;
    if (!Number.isNaN(to) && !(t < to + 86400000)) return false;
    if (text) {
      const hay = [e.subject, e.from, e.id, ...addressList(e.to)].join(' ').toLowerCase();
      if (!hay.includes(text)) return false;
    }
    return true;
  });
}

// ---- email detail ----

// recipientSteps summarizes one recipient's timeline group as steps from
// queued to the final status: [{label, tone, at, kind}].
export function recipientSteps(group, now = Date.now()) {
  const g = group || {};
  const steps = [];
  const entries = Array.isArray(g.entries) ? g.entries : [];
  const queued = entries.find((e) => e.kind === 'event' && ['queued', 'scheduled', 'held'].includes(e.event.type));
  steps.push({ kind: 'start', label: queued && queued.event.type === 'held' ? 'Held' : 'Queued', tone: 'info', at: queued ? queued.event.created_at : '' });
  for (const e of entries) {
    if (e.kind === 'attempt') {
      const a = e.attempt;
      const outcome = a.outcome || (a.finished_at ? 'unknown' : 'sending');
      steps.push({ kind: 'attempt', label: `Attempt ${e.n}: ${statusLabel(outcome).toLowerCase()}`, tone: statusTone(outcome), at: a.started_at });
    } else if (['approved', 'rejected'].includes(e.event.type)) {
      steps.push({ kind: 'event', label: statusLabel(e.event.type), tone: statusTone(e.event.type), at: e.event.created_at });
    }
  }
  const st = g.status || '';
  if (isFinal(st)) {
    steps.push({ kind: 'final', label: statusLabel(st), tone: statusTone(st), at: '' });
  } else if (st === 'held') {
    steps.push({ kind: 'wait', label: 'Waiting for approval', tone: 'warn', at: '' });
  } else {
    steps.push({ kind: 'wait', label: st === 'sending' ? 'Sending now' : 'Not final yet', tone: 'muted', at: '' });
  }
  void now;
  return steps;
}

// ---- overview ----

export const QUEUE_STATES = [
  ['queued', 'Queued, due now', 'info'],
  ['scheduled', 'Scheduled', 'info'],
  ['sending', 'Sending', 'info'],
  ['deferred', 'Deferred, will retry', 'warn'],
  ['held', 'Held for approval', 'warn'],
];

// formatAge gives "45 s", "12 min", "3 h 5 min" or "2 d 4 h".
export function formatAge(seconds) {
  const s = Number(seconds);
  if (seconds == null || !Number.isFinite(s) || s < 0) return '—';
  if (s < 60) return `${Math.round(s)} s`;
  if (s < 3600) return `${Math.floor(s / 60)} min`;
  if (s < 86400) {
    const m = Math.floor((s % 3600) / 60);
    return m ? `${Math.floor(s / 3600)} h ${m} min` : `${Math.floor(s / 3600)} h`;
  }
  const hrs = Math.floor((s % 86400) / 3600);
  return hrs ? `${Math.floor(s / 86400)} d ${hrs} h` : `${Math.floor(s / 86400)} d`;
}

const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];

// dailyBars turns overview.sending.daily into bars on one scale: each
// with heights in percent of the tallest day.
export function dailyBars(daily) {
  const days = Array.isArray(daily) ? daily : [];
  const max = Math.max(0, ...days.map((d) => Number(d.recipients) || 0));
  return days.map((d) => {
    const total = Number(d.recipients) || 0;
    const t = new Date(`${d.day}T00:00:00Z`);
    const pct = (n) => (max ? Math.round(((Number(n) || 0) / max) * 1000) / 10 : 0);
    return {
      day: d.day,
      label: Number.isNaN(t.getTime()) ? String(d.day || '') : WEEKDAYS[t.getUTCDay()],
      total,
      delivered: Number(d.delivered) || 0,
      bounced: Number(d.bounced) || 0,
      complained: Number(d.complained) || 0,
      height: pct(total),
      deliveredHeight: pct(Math.min(Number(d.delivered) || 0, total)),
      bouncedHeight: pct(Math.min(Number(d.bounced) || 0, total)),
    };
  });
}

// workerState describes a worker: online, draining or offline.
export function workerState(w) {
  const x = w || {};
  if (x.draining) return { tone: 'warn', label: 'Draining' };
  if (x.online) return { tone: 'ok', label: 'Online' };
  if (!x.last_seen_at) return { tone: 'muted', label: 'Never seen' };
  return { tone: 'bad', label: 'Offline' };
}

// warmupText describes an IP's warm-up.
export function warmupText(warmup) {
  const w = warmup;
  if (!w) return '—';
  if (w.enabled === false) return 'Off';
  if (w.warm) return 'Warm';
  if (!w.started) return 'Not started';
  const left = w.remaining != null && w.cap != null ? `, ${formatCount(w.remaining)} of ${formatCount(w.cap)} left today` : '';
  return `Day ${w.day}${left}`;
}

// warmupShare is the share of today's warm-up capacity used, 0 to 1, or
// null when no cap applies.
export function warmupShare(warmup) {
  const w = warmup;
  if (!w || w.warm || !w.cap) return null;
  const used = Math.max(0, Number(w.cap) - Math.max(0, Number(w.remaining) || 0));
  return Math.min(1, used / Number(w.cap));
}

// domainsNeedingAttention keeps the overview's domains with problems.
export function domainsNeedingAttention(domains) {
  return (Array.isArray(domains) ? domains : []).filter((d) => Array.isArray(d.problems) && d.problems.length);
}

// ---- setup checklist ----

// setupSteps works out the guided setup, for sending and for receiving,
// from what the project has. s: {domains (with records), keys (list or
// null when unknown), emailCount, receiving (overview.receiving or null),
// inboxes, messageCount}.
export function setupSteps(s) {
  const st = s || {};
  const domains = Array.isArray(st.domains) ? st.domains : [];
  const verified = domains.find((d) => d.status === 'verified');
  const target = verified || domains[0] || null;
  const required = target ? (target.records || []).filter((r) => r.required) : [];
  const recordsDone = Boolean(target) && required.length > 0 && required.every((r) => r.status === 'verified');
  const keys = Array.isArray(st.keys) ? st.keys : null;
  const agentKey = keys ? keys.length > 1 || keys.some((k) => k.permission === 'sending_access') : null;
  const rcv = st.receiving || null;
  const inbound = target ? (target.records || []).find((r) => r.record === 'INBOUND') : null;
  const inboxes = Array.isArray(st.inboxes) ? st.inboxes : [];
  const send = [
    { id: 'domain', title: 'Add a sending domain', done: domains.length > 0, route: '#/domains', domain: target },
    { id: 'dns', title: 'Publish its DNS records', done: recordsDone || Boolean(verified), route: target ? `#/domains/${encodeURIComponent(target.id)}` : '#/domains', domain: target },
    { id: 'verify', title: 'Verify the domain', done: Boolean(verified), route: target ? `#/domains/${encodeURIComponent(target.id)}` : '#/domains' },
    { id: 'key', title: 'Create a key for your agent', done: agentKey === true, unknown: agentKey === null, route: '#/api-keys' },
    { id: 'send', title: 'Send a test email and watch its timeline', done: (Number(st.emailCount) || 0) > 0, route: '#/compose' },
  ];
  const receiveOn = rcv ? Boolean(rcv.enabled) : Boolean(inbound);
  const receive = [
    {
      id: 'mx', title: 'Publish the inbound MX record',
      done: Boolean(inbound && inbound.status === 'verified'),
      blocked: !receiveOn,
      route: target ? `#/domains/${encodeURIComponent(target.id)}` : '#/domains',
      record: inbound || null,
    },
    { id: 'inbox', title: 'Create an inbox', done: inboxes.length > 0, route: '#/inboxes' },
    { id: 'receive', title: 'Receive a message and read it in Mail', done: (Number(st.messageCount) || 0) > 0, route: '#/mail', inbox: inboxes[0] || null },
  ];
  const all = [...send, ...receive.filter((x) => !x.blocked)];
  const left = all.filter((x) => !x.done && !x.unknown);
  return { send, receive, receiveOn, done: left.length === 0, next: left[0] ? left[0].id : '', total: all.length, finished: all.length - left.length };
}

// ---- compose ----

const LOCAL_RE = /^[A-Za-z0-9!#$%&'*+/=?^_`{|}~.-]+$/;
const ADDRESS_RE = /^[^\s@<>(),;:"[\]\\]+@[^\s@<>(),;:"[\]\\]+\.[^\s@<>(),;:"[\]\\.]+$/;

// isAddress is a plain shape check for one address (not "Name <a@b>").
export function isAddress(s) {
  return ADDRESS_RE.test(String(s || '').trim());
}

// splitAddresses splits what someone typed or pasted into addresses.
export function splitAddresses(text) {
  return String(text || '').split(/[\s,;]+/).map((x) => x.trim().replace(/^<|>$/g, '')).filter(Boolean);
}

// formatFrom builds a From header value: "Name" <local@domain>, quoting
// the name when needed. It throws an Error ready to show.
export function formatFrom({ name, local, domain } = {}) {
  const l = String(local || '').trim();
  const d = String(domain || '').trim();
  if (!d) throw new Error('Choose a verified domain to send from.');
  if (!l || !LOCAL_RE.test(l) || l.startsWith('.') || l.endsWith('.') || l.includes('..') || l.length > 64) {
    throw new Error('The part before @ may use letters, digits and . _ - + only, like hello or support.');
  }
  const n = String(name || '').replace(/[\r\n]+/g, ' ').trim();
  if (!n) return `${l}@${d}`;
  const plain = /^[A-Za-z0-9 !#$%&'*+/=?^_`{|}~.-]+$/.test(n);
  return `${plain ? n : `"${n.replace(/(["\\])/g, '\\$1')}"`} <${l}@${d}>`;
}

export const ATTACHMENT_LIMITS = { files: 20, bytes: 10 * 1024 * 1024 };

// attachmentBudget checks attachments against the API's limits.
export function attachmentBudget(files) {
  const list = Array.isArray(files) ? files : [];
  const bytes = list.reduce((a, f) => a + (Number(f.size) || 0), 0);
  let problem = '';
  if (list.length > ATTACHMENT_LIMITS.files) problem = `At most ${ATTACHMENT_LIMITS.files} files per email.`;
  else if (bytes > ATTACHMENT_LIMITS.bytes) problem = `At most ${formatBytes(ATTACHMENT_LIMITS.bytes)} in total per email.`;
  return { count: list.length, bytes, problem };
}

// base64FromDataURL keeps the base64 part of a data: URL.
export function base64FromDataURL(url) {
  const s = String(url || '');
  const i = s.indexOf(';base64,');
  return i >= 0 ? s.slice(i + 8) : '';
}

// buildCompose turns the compose form into the body of POST /emails and
// POST /emails/check. It throws an Error ready to show.
export function buildCompose(v, now = Date.now()) {
  const x = v || {};
  const body = { from: formatFrom(x.from || {}) };
  for (const [field, label] of [['to', 'To'], ['cc', 'Cc'], ['bcc', 'Bcc'], ['reply_to', 'Reply-To']]) {
    const list = (Array.isArray(x[field]) ? x[field] : splitAddresses(x[field])).map((a) => String(a).trim()).filter(Boolean);
    const bad = list.find((a) => !isAddress(a));
    if (bad) throw new Error(`${label}: "${bad}" is not an email address.`);
    if (list.length) body[field] = list;
  }
  if (!body.to) throw new Error('Add at least one recipient under To.');
  const n = new Set([...(body.to || []), ...(body.cc || []), ...(body.bcc || [])].map((a) => a.toLowerCase())).size;
  if (n > 50) throw new Error('To, Cc and Bcc together hold at most 50 recipients.');
  const subject = String(x.subject ?? '');
  if (!subject.trim()) throw new Error('Write a subject.');
  body.subject = subject;
  const text = String(x.text ?? '');
  const html = String(x.html ?? '');
  if (!text.trim() && !html.trim()) throw new Error('Write a text or HTML body.');
  if (text.trim()) body.text = text;
  if (html.trim()) body.html = html;
  if (x.stream && x.stream !== 'transactional') body.stream = String(x.stream);
  if (x.stream === 'broadcast' && n !== 1) throw new Error('Broadcast mail goes to exactly one recipient.');
  const when = (value, label) => {
    const s = String(value || '').trim();
    if (!s) return '';
    const d = new Date(s);
    if (Number.isNaN(d.getTime())) throw new Error(`${label}: enter a date and time.`);
    if (d.getTime() <= now) throw new Error(`${label}: the time must be in the future.`);
    return d.toISOString();
  };
  const sched = when(x.scheduledAt, 'Send at');
  if (sched) {
    if (new Date(sched).getTime() > now + 30 * 86400000) throw new Error('Send at: at most 30 days ahead.');
    body.scheduled_at = sched;
  }
  const exp = when(x.expiresAt, 'Give up at');
  if (exp) {
    const start = sched ? new Date(sched).getTime() : now;
    if (new Date(exp).getTime() <= start) throw new Error('Give up at: must be after the send time.');
    if (new Date(exp).getTime() > start + 7 * 86400000) throw new Error('Give up at: at most 7 days after the send time.');
    body.expires_at = exp;
  }
  if (x.retryUncertain === false) body.retry_uncertain = false;
  const atts = Array.isArray(x.attachments) ? x.attachments : [];
  const budget = attachmentBudget(atts);
  if (budget.problem) throw new Error(`Attachments: ${budget.problem}`);
  if (atts.length) {
    body.attachments = atts.map((a) => {
      const out = { filename: safeFilename(a.filename), content: String(a.content || '') };
      if (a.content_type) out.content_type = String(a.content_type);
      return out;
    });
  }
  return body;
}

// ---- mail (all inboxes) ----

// mergeMessages merges pages from several inboxes, newest first, and
// tags each with its inbox.
export function mergeMessages(pages) {
  const out = [];
  const seen = new Set();
  for (const p of Array.isArray(pages) ? pages : []) {
    for (const m of (p && Array.isArray(p.data)) ? p.data : []) {
      if (!m || seen.has(m.id)) continue;
      seen.add(m.id);
      out.push({ ...m, inbox_id: m.inbox_id || p.inbox_id, inbox: m.inbox || p.inbox || '' });
    }
  }
  return out.sort((a, b) => new Date(b.received_at) - new Date(a.received_at));
}

// filterMessages filters merged messages by inbox, trust, held,
// suspicious and text (from, subject, snippet).
export function filterMessages(rows, { inbox, trust, held, suspicious, q } = {}) {
  const text = String(q || '').trim().toLowerCase();
  return (Array.isArray(rows) ? rows : []).filter((m) => {
    if (inbox && m.inbox_id !== inbox) return false;
    if (trust && (m.trust || '') !== trust) return false;
    if (held && !m.held) return false;
    if (suspicious && !m.suspicious_instructions) return false;
    if (text && ![m.from, m.subject, m.snippet].join(' ').toLowerCase().includes(text)) return false;
    return true;
  });
}

// countNew counts messages received after a time (ms), or none when there
// was no earlier visit.
export function countNew(rows, since) {
  const t = Number(since);
  if (!t) return 0;
  return (Array.isArray(rows) ? rows : []).filter((m) => new Date(m.received_at).getTime() > t).length;
}

// threadItems merges a thread's received messages and our sent emails,
// oldest first: [{kind: 'received'|'sent', at, item}].
export function threadItems(messages, emails) {
  const out = [];
  for (const m of Array.isArray(messages) ? messages : []) out.push({ kind: 'received', at: m.received_at, item: m });
  for (const e of Array.isArray(emails) ? emails : []) out.push({ kind: 'sent', at: e.created_at, item: e });
  return out.sort((a, b) => new Date(a.at) - new Date(b.at) || (a.kind === b.kind ? 0 : a.kind === 'sent' ? 1 : -1));
}

// ---- DNS automation ----

export const DNS_ACTIONS = dict({
  create: ['ok', 'Create'],
  update: ['warn', 'Update'],
  unchanged: ['muted', 'Unchanged'],
  skip: ['muted', 'Skip'],
});

// dnsActionBadge gives [tone, label] for a planned DNS change.
export function dnsActionBadge(action) {
  return DNS_ACTIONS[String(action || '')] || ['muted', statusLabel(action)];
}

// dnsSummary counts planned DNS changes: "2 to create, 1 to update".
export function dnsSummary(changes) {
  const list = Array.isArray(changes) ? changes : [];
  const n = (a) => list.filter((c) => c && c.action === a).length;
  const parts = [];
  if (n('create')) parts.push(`${n('create')} to create`);
  if (n('update')) parts.push(`${n('update')} to update`);
  if (n('unchanged')) parts.push(`${n('unchanged')} already right`);
  if (n('skip')) parts.push(`${n('skip')} skipped`);
  return parts.length ? parts.join(', ') : 'No changes';
}
