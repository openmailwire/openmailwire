// OpenMailWire dashboard: plain ES modules, no build step, no dependencies.
//
// Every value that comes from the API can be attacker-controlled: subjects,
// addresses, SMTP replies, webhook URLs, inbound mail. Such values reach
// the page only as text (text nodes, textContent, plain attributes set by
// h()). Nothing from the API is ever parsed as markup, and links are only
// built to in-app routes. tests/safety.test.mjs fails the build if an HTML
// sink appears in any script.

import * as L from './lib.js';
import {
  API, PAGE_SIZE, getKey, setKey, request, onUnauthorized, isAbort, randomID, sleep,
  $, h, put, nextID, announce, link, route, mono, muted, badge, toneBadge, senderTag, untrusted,
  when, exact, copyable, panel, pageHead, backLink, loadingLine, emptyState, kv, notice, errorView,
  dataTable, pager, field, input, textarea, select, checkbox, formStatus, fieldError, withBusy,
  secretReveal, download, makeContext, fill, confirmed, onHeldChange, heldChanged, toast, showDialog,
} from './ui.js';
import {
  pageHeld, pageSuppressions, pagePauses, pageSettings, pageInsights, pageAuditLog, decisionForm,
} from './ops.js';
import { reportAddressesPanel, dkimPanel, reportsPanel, cloudflarePanel } from './domains.js';
import { pageOverview } from './home.js';
import { pageCompose } from './compose.js';
import { pageMail, receivingHelp, fetchNewCount } from './mail.js';

const POLL_MS = 5000;
const POLL_FOR_MS = 10 * 60 * 1000;

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
      status.replaceChildren(fieldError('An email ID looks like ed01a077-…, 36 characters.'));
      return;
    }
    location.hash = route('emails', v);
  });
  return form;
}

function checkResult(r) {
  const v = L.checkVerdict(r);
  const kind = { ok: 'ok', warn: 'warn', bad: 'error' }[v.tone];
  const refusals = Array.isArray(r.refusals) ? r.refusals : [];
  const warnings = Array.isArray(r.warnings) ? r.warnings : [];
  const issue = (code, message) => h('li', {}, h('code', {}, code || '—'), ' ', message || '');
  return h('div', { class: `notice notice-${kind}`, role: 'status' },
    h('p', { class: 'notice-title' }, v.title),
    kv([
      ['Would', L.statusLabel(r.would)],
      ['Stream', L.STREAM_LABELS[r.stream] || r.stream],
      ['Size', `${L.formatBytes(r.size)}, DKIM signature included (estimated)`],
      ['Hold reasons', (r.hold_reasons || []).length ? h('span', { class: 'chips' }, r.hold_reasons.map((x) => toneBadge('warn', L.holdReasonLabel(x)))) : null],
    ]),
    refusals.length ? [h('p', { class: 'list-title' }, 'Refusals'), h('ul', { class: 'issues' }, refusals.map((x) => issue(x.name, x.message)))] : null,
    warnings.length ? [h('p', { class: 'list-title' }, 'Warnings'), h('ul', { class: 'issues' }, warnings.map((x) => issue(x.code, x.message)))] : null);
}

function sendTestPanel(ctx) {
  const from = input({ name: 'from', required: true, inputmode: 'email', placeholder: 'test@your-domain.com' });
  const to = input({ name: 'to', required: true, inputmode: 'email', placeholder: 'you@example.org' });
  const subject = input({ name: 'subject', required: true, value: 'OpenMailWire test' });
  const text = textarea({ name: 'text', required: true, rows: '4', spellcheck: 'true' },
    'This is a test email sent from the OpenMailWire dashboard.');
  const stream = select({ name: 'stream' }, [['transactional', 'Transactional'], ['broadcast', 'Broadcast']], 'transactional');
  const status = formStatus();
  const check = h('button', { type: 'button', class: 'btn' }, 'Check');
  let idem = randomID();
  const form = h('form', { class: 'form', novalidate: true },
    h('div', { class: 'grid-2' },
      field('From', from, 'An address on a verified domain.'),
      field('To', to, 'Separate several addresses with commas.')),
    h('div', { class: 'grid-2' },
      field('Subject', subject),
      field('Stream', stream, 'Transactional: mail the recipient expects. Broadcast: newsletters and bulk mail, to one recipient, with a one-click unsubscribe (needs OMW_PUBLIC_URL).')),
    field('Text', text),
    h('div', { class: 'form-actions' },
      h('button', { type: 'submit', class: 'btn btn-primary' }, 'Send test email'),
      check,
      h('p', { class: 'hint' }, 'Check sends nothing: it reports what a send would do. Send sends real email.')),
    status);
  const body = () => L.buildSendBody({ from: from.value, to: to.value, subject: subject.value, text: text.value, stream: stream.value });
  form.addEventListener('input', () => { idem = randomID(); });
  check.addEventListener('click', async () => {
    status.replaceChildren();
    await withBusy(form, async () => {
      try {
        const res = await ctx.api('POST', '/emails/check', body());
        if (ctx.live) status.replaceChildren(checkResult(res));
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'emails') || '');
      }
    });
  });
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    status.replaceChildren();
    const b = body();
    if (!b.from || !b.to.length || !b.subject.trim() || !b.text.trim()) {
      status.replaceChildren(fieldError('Fill in from, to, subject and text.'));
      return;
    }
    await withBusy(form, async () => {
      try {
        const res = await ctx.api('POST', '/emails', b, { idempotencyKey: idem });
        if (res.held) heldChanged();
        location.hash = route('emails', res.id);
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'emails') || '');
      }
    });
  });
  const details = h('details', { class: 'panel collapsible' },
    h('summary', {}, h('span', { class: 'panel-title' }, 'Send or check a test email')), form);
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
  { label: 'Sent', class: 'col-time', cell: (e) => (e.scheduled_at ? h('span', { class: 'status-cell' }, when(e.created_at), h('span', { class: 'small muted' }, 'for ', exact(e.scheduled_at))) : when(e.created_at)) },
];

const EMAIL_STATUSES = [
  ['', 'Any status'], ['queued', 'Queued'], ['scheduled', 'Scheduled'], ['held', 'Held'], ['sending', 'Sending'],
  ['deferred', 'Deferred'], ['delivered', 'Delivered'], ['bounced', 'Bounced'], ['complained', 'Complained'],
  ['suppressed', 'Suppressed'], ['expired', 'Expired'], ['rejected', 'Rejected'], ['canceled', 'Canceled'],
];

async function pageEmails(ctx) {
  const list = h('section', { class: 'panel' });
  let rows = [];
  let next = null;
  let more = false;
  const fetchPage = (before) => ctx.api('GET', L.listPath('/emails', { limit: 50, before }));

  const q = input({ name: 'q', type: 'search', placeholder: 'Search subject, address or ID', 'aria-label': 'Search emails', 'data-search': 'true' });
  const status = select({ name: 'status', 'aria-label': 'Status' }, EMAIL_STATUSES, '');
  const domain = input({ name: 'domain', placeholder: 'Recipient domain', 'aria-label': 'Recipient domain', class: 'input mono' });
  const since = input({ name: 'since', type: 'date', 'aria-label': 'Sent on or after' });
  const until = input({ name: 'until', type: 'date', 'aria-label': 'Sent on or before' });
  const filterBar = h('div', { class: 'filter-bar', role: 'search' }, q, status, domain,
    h('span', { class: 'date-range' }, since, muted(' to '), until));
  const summary = h('p', { class: 'mail-summary', 'aria-live': 'polite' });
  const body = h('div');
  const moreBox = h('div', { class: 'pager' });
  const listStatus = formStatus();
  const moreBtn = h('button', { type: 'button', class: 'btn' }, 'Load more');
  const filters = () => ({ q: q.value, status: status.value, domain: domain.value, since: since.value, until: until.value });
  const filtered = () => Object.values(filters()).some(Boolean);
  const draw = () => {
    const shown = L.filterEmails(rows, filters());
    put(summary, filtered() ? `${L.formatCount(shown.length)} of ${L.formatCount(rows.length)} loaded emails match` : `${L.formatCount(rows.length)} loaded`,
      more && filtered() ? ' · load more to search further' : null);
    if (!rows.length) {
      put(body, emptyState('No emails yet.', link('#/compose', 'Compose one'), ' or call POST /emails.'));
    } else if (!shown.length) {
      put(body, emptyState('No loaded email matches these filters.'));
    } else {
      put(body, dataTable(emailColumns, shown, 'Emails').el);
    }
    put(moreBox, more ? moreBtn : null, listStatus);
  };
  moreBtn.addEventListener('click', async () => {
    moreBtn.disabled = true;
    moreBtn.textContent = 'Loading…';
    listStatus.replaceChildren();
    try {
      const page = await fetchPage(next);
      if (!ctx.live) return;
      rows = rows.concat(page.data || []);
      next = page.next_before;
      more = Boolean(page.has_more && next);
      draw();
      announce(`${(page.data || []).length} more loaded`);
    } catch (err) {
      if (ctx.live) listStatus.replaceChildren(errorView(err, 'emails') || '');
    } finally {
      moreBtn.disabled = false;
      moreBtn.textContent = 'Load more';
    }
  });
  for (const el of [q, status, domain, since, until]) el.addEventListener('input', draw);

  ctx.mount(
    pageHead('Emails', 'Messages sent with this project, newest first.', link('#/compose', 'Compose'), lookupForm()),
    sendTestPanel(ctx),
    list);
  const head = h('h2', { class: 'panel-title' }, 'Recent emails');
  await fill(ctx, list, head, 'an email list', async () => {
    let first;
    try {
      first = await fetchPage();
    } catch (err) {
      if (L.classifyError(err).kind !== 'unavailable') throw err;
      return [errorView(err, 'an email list'), h('p', { class: 'hint' }, 'You can still open an email by its ID, above.')];
    }
    rows = first.data || [];
    next = first.next_before;
    more = Boolean(first.has_more && next);
    draw();
    return [filterBar, summary, body, moreBox];
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

function heldPanel(ctx, email, onRefresh) {
  const waiting = L.heldRecipients(email);
  if (!waiting.length) return null;
  const box = h('section', { class: 'panel panel-warn' },
    h('h2', { class: 'panel-title' }, 'Waiting for approval'),
    h('p', {}, `${waiting.join(', ')} ${waiting.length === 1 ? 'waits' : 'wait'} for a person to approve this email. `,
      'It expires unsent at ', exact(email.expires_at), '. ', link('#/held', 'All held emails')));
  box.append(decisionForm(ctx, email, {
    recipients: waiting,
    onDecided: () => onRefresh(),
  }));
  return box;
}

function stepsStrip(group) {
  const steps = L.recipientSteps(group);
  return h('ol', { class: 'stepper', 'aria-label': `Progress for ${group.recipient}` }, steps.map((st) => h('li', { class: `stp tone-${st.tone} stp-${st.kind}` },
    h('span', { class: 'stp-dot', 'aria-hidden': 'true' }),
    h('span', { class: 'stp-label' }, st.label),
    st.at ? h('span', { class: 'stp-at' }, when(st.at)) : null)));
}

function renderEmail(ctx, box, email, { pending, onRefresh }) {
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
    heldPanel(ctx, email, onRefresh),
    panel('Message', kv([
      ['ID', copyable(email.id, 'email ID')],
      ['From', mono(email.from)],
      ['To', mono(L.addressList(email.to).join(', '))],
      ['Cc', L.addressList(email.cc).length ? mono(L.addressList(email.cc).join(', ')) : null],
      ['Bcc', L.addressList(email.bcc).length ? mono(L.addressList(email.bcc).join(', ')) : null],
      ['Stream', email.stream ? L.STREAM_LABELS[email.stream] || email.stream : null],
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
        stepsStrip(g),
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
    renderEmail(ctx, box, email, { pending: polling && L.isMoving(email), onRefresh: refresh });
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
  // Held mail waits for a person, so it is not polled.
  while (ctx.live && L.isMoving(email) && Date.now() - started < POLL_FOR_MS) {
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

function claimNote(d) {
  if (!d.claim_expires_at || d.status === 'verified') return null;
  return h('span', { class: 'small muted' }, 'removed ', L.relativeTime(d.claim_expires_at), ' unless verified');
}

async function pageDomains(ctx) {
  const name = input({ name: 'name', required: true, placeholder: 'example.com', inputmode: 'url' });
  const status = formStatus();
  const form = h('form', { class: 'form', novalidate: true },
    h('div', { class: 'row' }, field('Domain name', name, 'The domain in your From addresses. It must be verified within 7 days, or it is removed.'),
      h('div', { class: 'row-end' }, h('button', { type: 'submit', class: 'btn btn-primary' }, 'Add domain'))),
    status);
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    status.replaceChildren();
    if (!name.value.trim()) {
      status.replaceChildren(fieldError('Enter a domain name.'));
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
    pageHead('Domains', 'Domains this project sends from. Each needs its DKIM, SPF, return-path MX and DMARC records before it can send.'),
    panel('Add a domain', form),
    list);
  await fill(ctx, list, h('h2', { class: 'panel-title' }, 'Your domains'), 'domains', async () => {
    const res = await ctx.api('GET', '/domains');
    const rows = res.data || [];
    if (!rows.length) return emptyState('No domains yet. Add one above.');
    return dataTable([
      { label: 'Domain', class: 'col-main', cell: (d) => link(route('domains', d.id), d.name) },
      { label: 'Status', class: 'col-status', cell: (d) => h('span', { class: 'status-cell' }, badge(d.status), claimNote(d)) },
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
    { label: 'Value', class: 'col-value', cell: (r) => copyable(r.value, `${r.record} record value`, 72) },
    {
      label: 'Status',
      class: 'col-status',
      cell: (r) => h('span', { class: 'status-cell' }, badge(r.status), r.detail ? h('span', { class: 'small muted' }, r.detail) : null),
    },
  ], records, 'DNS records');
}

function dmarcNote(records) {
  const r = (records || []).find((x) => x.record === 'DMARC');
  if (!r || r.status === 'verified') return null;
  return notice('warn', 'DMARC record needed',
    'Since v0.6 a domain needs a DMARC record (p=none is enough to start) to send: Gmail, Yahoo and Microsoft require one from bulk senders. A verified domain without one loses its status at its next check.');
}

async function pageDomain(ctx, id) {
  if (!L.isUUID(id)) return pageNotFound(ctx, 'That is not a domain ID.');
  const path = `/domains/${encodeURIComponent(id)}`;
  const box = h('div');
  const addresses = h('div');
  let cf = null;
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
        toast(nd.status === 'verified' ? `${nd.name} is verified` : `${nd.name} is not verified yet`, nd.status === 'verified' ? 'ok' : 'warn');
      } catch (err) {
        if (!ctx.live) return;
        status.replaceChildren(errorView(err, 'domains') || '');
        verify.disabled = false;
        verify.textContent = 'Verify DNS now';
      }
    });
    del.addEventListener('click', async () => {
      if (!await confirmed(`Mail from ${d.name} is refused until it is added and verified again.`, { title: `Delete ${d.name}?`, confirm: 'Delete domain', danger: true })) return;
      del.disabled = true;
      try {
        await ctx.api('DELETE', path);
        toast(`${d.name} deleted`);
        location.hash = '#/domains';
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'domains') || '');
        del.disabled = false;
      }
    });
    const claim = d.claim_expires_at && d.status !== 'verified'
      ? notice('calm', 'Verify it within 7 days',
        h('span', {}, 'An unverified domain is removed at ', exact(d.claim_expires_at), ` (${L.relativeTime(d.claim_expires_at)}). Any project may add a name; only one that proves control of its DNS can verify it.`))
      : null;
    if (!cf) {
      cf = cloudflarePanel(ctx, d, (nd) => render(nd, nd.status === 'verified'
        ? notice('ok', 'Verified', 'This domain can send.')
        : notice('calm', 'Not verified yet', 'The records are written; DNS can take a few minutes. Verify again shortly.')));
    }
    put(box,
      pageHead(d.name, h('span', {}, badge(d.status), ' Last checked ', when(d.checked_at, 'never')), verify, del),
      status,
      claim,
      cf,
      panel('DNS records',
        h('p', { class: 'hint' }, 'Publish these records at your DNS provider, then verify. The domain can send once every required record is found.'),
        dmarcNote(d.records),
        recordsTable(d.records || []).el),
      panel('Details', kv([
        ['ID', copyable(d.id, 'domain ID')],
        ['Added', exact(d.created_at)],
        ['Verified', exact(d.verified_at)],
        ['Last check', exact(d.checked_at)],
        ['Claim expires', d.claim_expires_at ? exact(d.claim_expires_at) : null],
      ])));
    put(addresses, reportAddressesPanel(d));
    if (message) announce(message.textContent);
  };
  const d = await ctx.api('GET', path);
  render(d);
  ctx.mount(backLink('#/domains', 'All domains'), box, addresses, dkimPanel(ctx, d), reportsPanel(ctx, d));
}

// ---- webhooks ----

function eventChips(events) {
  if (!events || !events.length) return muted('All events');
  return h('span', { class: 'chips' }, events.map((e) => h('span', { class: 'chip' }, L.statusLabel(e))));
}

function healthBadge(w) {
  const x = L.webhookHealth(w);
  return toneBadge(x.tone, x.label, x.detail || null);
}

function signingLabel(w) {
  return w.asymmetric_signing ? 'HMAC + Ed25519' : 'HMAC';
}

async function pageWebhooks(ctx) {
  const endpoint = input({ name: 'endpoint', type: 'url', required: true, inputmode: 'url', placeholder: 'https://example.com/webhooks/openmailwire' });
  const boxes = L.WEBHOOK_EVENTS.map(([type, help]) => {
    const cb = h('input', { type: 'checkbox', name: 'events', value: type, id: nextID('ev') });
    return { type, cb, el: h('div', { class: 'check' }, cb, h('label', { for: cb.id }, h('span', { class: 'mono' }, type), muted(` ${help}`))) };
  });
  const ed = checkbox('Also sign with Ed25519 (Standard Webhooks v1a)', {
    name: 'asymmetric_signing',
    hint: 'Receivers verify with a public key and hold no secret that could sign.',
  });
  const status = formStatus();
  const reveal = h('div');
  const form = h('form', { class: 'form', novalidate: true },
    field('Endpoint URL', endpoint, 'Must use https and resolve to a public address. It must answer within 10 seconds.'),
    h('fieldset', { class: 'fieldset' },
      h('legend', {}, 'Events'),
      h('p', { class: 'hint' }, 'Leave all unchecked to receive every event type.'),
      h('div', { class: 'checks' }, boxes.map((b) => b.el))),
    h('fieldset', { class: 'fieldset' }, h('legend', {}, 'Signing'), ed.el),
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
      { label: 'Signing', cell: signingLabel },
      { label: 'State', class: 'col-status', cell: healthBadge },
      { label: 'Created', class: 'col-time', cell: (w) => when(w.created_at) },
      { label: 'Actions', class: 'col-actions', cell: (w) => link(route('webhooks', w.id), 'Deliveries') },
    ], rows, 'Webhooks').el;
  });
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    status.replaceChildren();
    if (!endpoint.value.trim()) {
      status.replaceChildren(fieldError('Enter the endpoint URL.'));
      return;
    }
    const events = boxes.filter((b) => b.cb.checked).map((b) => b.type);
    const body = { endpoint: endpoint.value.trim(), events };
    if (ed.cb.checked) body.asymmetric_signing = true;
    await withBusy(form, async () => {
      try {
        const res = await ctx.api('POST', '/webhooks', body);
        if (!ctx.live) return;
        form.reset();
        reveal.replaceChildren(secretReveal('Signing secret', res.signing_secret, 'signing secret',
          'Copy it now. Your handler needs it to verify the svix-signature or webhook-signature header.',
          res.public_key ? h('p', {}, 'Ed25519 public key (not secret, also shown on the webhook): ', copyable(res.public_key, 'public key')) : null));
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

function rotatePanel(ctx, w, onRotated) {
  const overlap = select({ name: 'overlap_seconds' }, L.ROTATE_OVERLAPS, 86400);
  const status = formStatus();
  const form = h('form', { class: 'form', novalidate: true },
    h('p', { class: 'hint' }, 'A new signing secret', w.asymmetric_signing ? ' and Ed25519 key' : '',
      ' replace the current ones. During the overlap every request carries both signatures, so you can update your handler at any moment in between.'),
    h('div', { class: 'row' }, field('Overlap', overlap, 'How long the previous secret keeps signing next to the new one.'),
      h('div', { class: 'row-end' }, h('button', { type: 'submit', class: 'btn' }, 'Rotate the secret'))),
    status);
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    status.replaceChildren();
    const secs = Number(overlap.value);
    const q = secs === 0
      ? 'Rotate now with no overlap? The old secret stops signing at once, so your handler fails until it has the new one.'
      : 'Rotate the signing secret? The new one is shown once.';
    if (!await confirmed(q, { title: 'Rotate the signing secret?', confirm: 'Rotate', danger: secs === 0 })) return;
    await withBusy(form, async () => {
      try {
        const res = await ctx.api('POST', `/webhooks/${encodeURIComponent(w.id)}/rotate-secret`, { overlap_seconds: secs });
        if (!ctx.live) return;
        onRotated(res);
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'webhooks') || '');
      }
    });
  });
  return h('details', { class: 'panel collapsible' }, h('summary', {}, h('span', { class: 'panel-title' }, 'Rotate the signing secret')), form);
}

async function pageWebhook(ctx, id) {
  if (!L.isUUID(id)) return pageNotFound(ctx, 'That is not a webhook ID.');
  const path = `/webhooks/${encodeURIComponent(id)}`;
  const box = h('div');
  const reveal = h('div');
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
      if (!await confirmed('Pending deliveries to it stop, and it receives no more events.', { title: 'Delete this webhook?', confirm: 'Delete', danger: true })) return;
      del.disabled = true;
      try {
        await ctx.api('DELETE', path);
        location.hash = '#/webhooks';
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'webhooks') || '');
        del.disabled = false;
      }
    });
    const health = L.webhookHealth(w);
    const deliveries = w.recent_deliveries || [];
    put(box,
      pageHead('Webhook', h('span', { class: 'mono' }, w.endpoint), refresh, del),
      status,
      health.tone !== 'ok' ? notice(health.tone === 'bad' ? 'error' : 'warn', health.label, health.detail) : null,
      panel('Details', kv([
        ['ID', copyable(w.id, 'webhook ID')],
        ['Endpoint', copyable(w.endpoint, 'endpoint URL')],
        ['Events', eventChips(w.events)],
        ['State', healthBadge(w)],
        ['Signing', signingLabel(w)],
        ['Public key', w.public_key ? copyable(w.public_key, 'public key') : null],
        ['Previous public key', w.previous_public_key ? copyable(w.previous_public_key, 'previous public key') : null],
        ['Previous secret signs until', w.previous_secret_expires_at ? exact(w.previous_secret_expires_at) : null],
        ['Secret rotated', w.secret_rotated_at ? exact(w.secret_rotated_at) : null],
        ['Failed in a row', w.consecutive_failures ? String(w.consecutive_failures) : null],
        ['Paused until', w.paused_until ? exact(w.paused_until) : null],
        ['Created', exact(w.created_at)],
      ])),
      rotatePanel(ctx, w, (res) => {
        reveal.replaceChildren(secretReveal('New signing secret', res.signing_secret, 'signing secret',
          res.previous_secret_expires_at
            ? `Copy it now. The previous secret keeps signing until ${L.formatDateTime(res.previous_secret_expires_at)}.`
            : 'Copy it now. The previous secret has stopped signing.',
          res.public_key ? h('p', {}, 'New Ed25519 public key: ', copyable(res.public_key, 'public key')) : null));
        render({ ...w, ...res, recent_deliveries: w.recent_deliveries });
        announce('Secret rotated');
      }),
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
  ctx.mount(backLink('#/webhooks', 'All webhooks'), reveal, box);
}

// ---- API keys ----

// keyFields builds the policy and access fields shared by the create and
// edit forms. read() returns {policy, allowed_ips, expires_at} or throws an
// Error whose message is ready to show.
function keyFields(key) {
  const k = key || {};
  const v = L.policyForm(k.policy);
  const perHour = input({ name: 'max_per_hour', type: 'number', min: '1', step: '1', inputmode: 'numeric', placeholder: 'No limit', value: v.maxPerHour });
  const perDay = input({ name: 'max_per_day', type: 'number', min: '1', step: '1', inputmode: 'numeric', placeholder: 'No limit', value: v.maxPerDay });
  const allowTo = textarea({ name: 'allowed_recipients', placeholder: '*@acme.com\nops@example.org' }, v.allowedRecipients);
  const allowFrom = textarea({ name: 'allowed_from', placeholder: 'agent@acme.com' }, v.allowedFrom);
  const inboxes = textarea({ name: 'inboxes', placeholder: 'support@acme.com\n*@agents.acme.com' }, v.inboxes);
  const ips = textarea({ name: 'allowed_ips', placeholder: '203.0.113.7\n10.0.0.0/8' }, (k.allowed_ips || []).join('\n'));
  const expires = input({ name: 'expires_at', type: 'datetime-local', value: L.localInputValue(k.expires_at) });
  const rules = Object.fromEntries(L.APPROVAL_RULES.filter(([r]) => r !== 'recipients_over').map(([r, label, help]) => [r, checkbox(label, {
    name: `require_${r}`, checked: v.requireApproval[{ always: 'always', first_contact: 'firstContact', has_attachments: 'hasAttachments', has_links: 'hasLinks' }[r]], hint: help,
  })]));
  const over = input({ name: 'recipients_over', type: 'number', min: '0', max: '1000', step: '1', inputmode: 'numeric', placeholder: 'Off', value: v.requireApproval.recipientsOver });
  const canApprove = checkbox('Can approve held emails', {
    name: 'can_approve', checked: v.canApprove,
    hint: "Approve and reject other keys' held emails. Full-access keys always can; no key approves its own.",
  });
  const nodes = [
    h('fieldset', { class: 'fieldset' },
      h('legend', {}, 'Sending limits'),
      h('p', { class: 'hint' }, 'Optional. Enforced by the server; empty means no limit. Patterns match lowercase addresses, and * matches any characters in the local part or the domain.'),
      h('div', { class: 'grid-2' },
        field('Max recipients per hour', perHour),
        field('Max recipients per day', perDay),
        field('Allowed recipients', allowTo, 'One pattern per line.'),
        field('Allowed senders', allowFrom, 'One pattern per line.'))),
    h('fieldset', { class: 'fieldset' },
      h('legend', {}, 'Approval'),
      h('p', { class: 'hint' }, "Hold this key's emails for a person to approve when any rule matches. Held emails appear under Held."),
      h('div', { class: 'checks' }, Object.values(rules).map((r) => r.el)),
      h('div', { class: 'grid-2' }, field('Hold when recipients are over', over, 'To, cc and bcc together, 0 to 1,000. Empty: off.')),
      canApprove.el),
    h('fieldset', { class: 'fieldset' },
      h('legend', {}, 'Access'),
      h('div', { class: 'grid-2' },
        field('Inboxes it may read', inboxes, 'Sending-only keys: inbox address patterns, one per line. Without any, the key sees no inbox. Full-access keys see every inbox.'),
        field('Allowed IPs', ips, 'Addresses or CIDR prefixes, one per line. Empty: any address.'),
        field('Expires', expires, 'Empty: never. In your time zone.'))),
  ];
  const read = () => ({
    policy: L.buildPolicy({
      maxPerHour: perHour.value, maxPerDay: perDay.value, allowedRecipients: allowTo.value, allowedFrom: allowFrom.value,
      requireApproval: {
        always: rules.always.cb.checked, firstContact: rules.first_contact.cb.checked,
        hasAttachments: rules.has_attachments.cb.checked, hasLinks: rules.has_links.cb.checked, recipientsOver: over.value,
      },
      canApprove: canApprove.cb.checked, inboxes: inboxes.value,
    }),
    allowed_ips: L.parseAllowedIPs(ips.value),
    expires: expires.value,
  });
  return { nodes, read, expiresInitial: expires.value };
}

function keyForm(ctx, onCreated) {
  const name = input({ name: 'name', required: true, placeholder: 'support-agent', maxlength: '100' });
  const full = h('input', { type: 'radio', name: 'permission', value: 'full_access', id: nextID('perm') });
  const sending = h('input', { type: 'radio', name: 'permission', value: 'sending_access', id: nextID('perm'), checked: true });
  const fields = keyFields(null);
  const status = formStatus();
  const form = h('form', { class: 'form', novalidate: true },
    field('Name', name, 'Who or what uses this key, e.g. the agent\'s name.'),
    h('fieldset', { class: 'fieldset' },
      h('legend', {}, 'Permission'),
      h('div', { class: 'check' }, sending, h('label', { for: sending.id }, h('strong', {}, 'Sending only'),
        muted(' Send and read its own emails, list domains, read and reply in the inboxes its policy lists.'))),
      h('div', { class: 'check' }, full, h('label', { for: full.id }, h('strong', {}, 'Full access'),
        muted(' Everything, including domains, webhooks, API keys and approvals.')))),
    fields.nodes,
    h('div', { class: 'form-actions' }, h('button', { type: 'submit', class: 'btn btn-primary' }, 'Create key')),
    status);
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    status.replaceChildren();
    if (!name.value.trim()) {
      status.replaceChildren(fieldError('Give the key a name.'));
      return;
    }
    let body;
    try {
      const v = fields.read();
      body = { name: name.value.trim(), permission: full.checked ? 'full_access' : 'sending_access', policy: v.policy };
      if (v.allowed_ips) body.allowed_ips = v.allowed_ips;
      const exp = L.expiryFromInput(v.expires);
      if (exp) body.expires_at = exp;
    } catch (err) {
      status.replaceChildren(fieldError(err.message));
      return;
    }
    await withBusy(form, async () => {
      try {
        const res = await ctx.api('POST', '/api-keys', body);
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

function accessCell(k) {
  const bits = L.keyAccessSummary(k);
  return h('span', { class: 'status-cell' },
    h('span', { class: `badge tone-${k.permission === 'sending_access' ? 'info' : 'warn'}` }, L.statusLabel(k.permission || 'full_access')),
    L.isExpired(k) ? toneBadge('bad', 'Expired') : null,
    bits.length ? h('span', { class: 'small muted' }, bits.join(' · ')) : null);
}

function revokeButton(ctx, k, after) {
  const current = L.isCurrentKey(k.prefix, getKey());
  const btn = h('button', { type: 'button', class: 'btn btn-small btn-danger', 'aria-label': `Revoke ${k.name || k.prefix}` }, 'Revoke');
  btn.addEventListener('click', async () => {
    const q = current
      ? `Revoke ${k.name}? This session uses it, so you will be signed out.`
      : `Revoke ${k.name}? Anything using it stops working at once.`;
    if (!await confirmed(q, { title: `Revoke ${k.name || 'this key'}?`, confirm: 'Revoke', danger: true })) return;
    btn.disabled = true;
    try {
      await ctx.api('DELETE', `/api-keys/${encodeURIComponent(k.id)}`);
      if (current) {
        signOut('You revoked the key this session used.');
        return;
      }
      announce(`${k.name} revoked`);
      await after(null);
    } catch (err) {
      btn.disabled = false;
      await after(err);
    }
  });
  return btn;
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
        { label: 'Name', class: 'col-main', cell: (k) => link(route('api-keys', k.id), k.name || '(unnamed)') },
        {
          label: 'Key',
          cell: (k) => h('span', {}, mono(`${k.prefix || ''}…`), L.isCurrentKey(k.prefix, key) ? h('span', { class: 'badge tone-info' }, 'This session') : null),
        },
        { label: 'Access', cell: accessCell },
        { label: 'Policy', cell: (k) => L.policySummary(k.policy).join(' · ') },
        { label: 'Created', class: 'col-time', cell: (k) => when(k.created_at) },
        { label: 'Last used', class: 'col-time', cell: (k) => when(k.last_used_at, 'never') },
        {
          label: 'Actions',
          class: 'col-actions',
          cell: (k) => h('span', { class: 'row-actions' }, link(route('api-keys', k.id), 'Edit'),
            revokeButton(ctx, k, async (err) => {
              if (err) {
                if (ctx.live) status.replaceChildren(errorView(err, 'API keys') || '');
                return;
              }
              await reload();
            })),
        },
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

async function pageAPIKey(ctx, id) {
  if (!L.isUUID(id)) return pageNotFound(ctx, 'That is not an API key ID.');
  const path = `/api-keys/${encodeURIComponent(id)}`;
  let k = await ctx.api('GET', path);
  const box = h('div');
  const render = (message) => {
    const name = input({ name: 'name', required: true, maxlength: '100', value: k.name || '' });
    const fields = keyFields(k);
    const status = formStatus();
    if (message) status.append(message);
    const form = h('form', { class: 'form', novalidate: true },
      field('Name', name),
      fields.nodes,
      h('div', { class: 'form-actions' },
        h('button', { type: 'submit', class: 'btn btn-primary' }, 'Save changes'),
        h('p', { class: 'hint' }, 'The permission cannot change: create a new key instead.')),
      status);
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      status.replaceChildren();
      if (!name.value.trim()) {
        status.replaceChildren(fieldError('Give the key a name.'));
        return;
      }
      let body;
      try {
        const v = fields.read();
        body = { name: name.value.trim(), policy: v.policy, allowed_ips: v.allowed_ips };
        // An unchanged expiry is left alone: a past one could not be sent again.
        if (v.expires !== fields.expiresInitial) body.expires_at = L.expiryFromInput(v.expires);
      } catch (err) {
        status.replaceChildren(fieldError(err.message));
        return;
      }
      await withBusy(form, async () => {
        try {
          k = await ctx.api('PATCH', path, body);
          if (!ctx.live) return;
          render(notice('ok', 'Saved', 'The new rules apply to the next request with this key.'));
          announce('Saved');
        } catch (err) {
          if (ctx.live) status.replaceChildren(errorView(err, 'API keys') || '');
        }
      });
    });
    const current = L.isCurrentKey(k.prefix, getKey());
    const headStatus = formStatus();
    put(box,
      pageHead(k.name || '(unnamed)', h('span', {}, mono(`${k.prefix || ''}…`), ' ', accessCell(k), current ? toneBadge('info', 'This session') : null),
        revokeButton(ctx, k, async (err) => {
          if (err) {
            if (ctx.live) headStatus.replaceChildren(errorView(err, 'API keys') || '');
            return;
          }
          location.hash = '#/api-keys';
        })),
      headStatus,
      panel('Details', kv([
        ['ID', copyable(k.id, 'key ID')],
        ['Permission', L.statusLabel(k.permission)],
        ['Policy', L.policySummary(k.policy).join(' · ')],
        ['Allowed IPs', (k.allowed_ips || []).length ? mono(k.allowed_ips.join(', ')) : 'Any address'],
        ['Expires', k.expires_at ? h('span', {}, exact(k.expires_at), muted(` · ${L.relativeTime(k.expires_at)}`)) : 'Never'],
        ['Created', exact(k.created_at)],
        ['Last used', exact(k.last_used_at)],
      ])),
      panel('Edit', form));
  };
  render();
  ctx.mount(backLink('#/api-keys', 'All keys'), box);
}

// ---- inboxes ----

const inboxAddress = (i) => i.address || i.email || i.id;

function holdLabel(i) {
  if (i.hold_unauthenticated === undefined) return muted('—');
  return i.hold_unauthenticated ? toneBadge('info', 'Held until released') : toneBadge('warn', 'Shown to agents');
}

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
    if (!rows.length) return receivingHelp('');
    return [listStatus, dataTable([
      { label: 'Address', class: 'col-main', cell: (i) => link(route('inboxes', i.id), h('span', { class: 'mono' }, inboxAddress(i))) },
      { label: 'Unauthenticated mail', cell: holdLabel },
      { label: 'Created', class: 'col-time', cell: (i) => when(i.created_at) },
      { label: 'Actions', class: 'col-actions', cell: (i) => deleteInbox(i) },
    ], rows, 'Inboxes').el];
  });
  const deleteInbox = (i) => {
    const btn = h('button', { type: 'button', class: 'btn btn-small btn-danger', 'aria-label': `Delete ${inboxAddress(i)}` }, 'Delete');
    btn.addEventListener('click', async () => {
      if (!await confirmed(`Mail to ${inboxAddress(i)} will be refused.`, { title: 'Delete this inbox?', confirm: 'Delete', danger: true })) return;
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
      status.replaceChildren(fieldError('Enter an address.'));
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

function trustCell(trust) {
  const t = L.trustBadge(trust);
  return toneBadge(t.tone, t.label, t.detail);
}

function messageFlags(m) {
  return [
    m.held ? toneBadge('info', 'Held') : null,
    m.suspicious_instructions ? toneBadge('bad', 'Suspicious instructions', 'The text matches a prompt-injection rule.') : null,
  ].filter(Boolean);
}

function releaseButton(ctx, m, onReleased, small = true) {
  const btn = h('button', { type: 'button', class: `btn ${small ? 'btn-small' : 'btn-primary'}`, 'aria-label': `Release the message from ${m.from || 'unknown sender'}` }, 'Release');
  btn.addEventListener('click', async () => {
    if (!await confirmed('Agent keys that may read this inbox will see it, including any instructions written in it.', { title: 'Release this message?', confirm: 'Release' })) return;
    btn.disabled = true;
    try {
      const res = await ctx.api('POST', `/messages/${encodeURIComponent(m.id)}/release`);
      if (!ctx.live) return;
      announce('Message released');
      onReleased(res, null);
    } catch (err) {
      btn.disabled = false;
      if (ctx.live) onReleased(null, err);
    }
  });
  return btn;
}

function holdSetting(ctx, inbox) {
  if (inbox.hold_unauthenticated === undefined) return null;
  const toggle = checkbox('Hold mail from senders that are not authenticated', {
    name: 'hold_unauthenticated', checked: inbox.hold_unauthenticated,
    hint: 'Held mail is hidden from sending-only (agent) keys until a full-access key releases it. Recommended.',
  });
  const status = formStatus();
  const form = h('form', { class: 'form', novalidate: true },
    toggle.el,
    h('div', { class: 'form-actions' }, h('button', { type: 'submit', class: 'btn' }, 'Save')),
    status);
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    status.replaceChildren();
    if (!toggle.cb.checked && !await confirmed('Anyone can forge a From address, and agents may act on what such mail says.', { title: 'Show unauthenticated mail to agents at once?', confirm: 'Show it to agents', danger: true })) {
      toggle.cb.checked = true;
      return;
    }
    await withBusy(form, async () => {
      try {
        const res = await ctx.api('PATCH', `/inboxes/${encodeURIComponent(inbox.id)}`, { hold_unauthenticated: toggle.cb.checked });
        if (!ctx.live) return;
        toggle.cb.checked = Boolean(res.hold_unauthenticated);
        status.replaceChildren(notice('ok', 'Saved', res.hold_unauthenticated
          ? 'Mail from unauthenticated senders is held until released.'
          : 'Mail from unauthenticated senders reaches agents at once.'));
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'inboxes') || '');
      }
    });
  });
  return h('details', { class: 'panel collapsible' }, h('summary', {}, h('span', { class: 'panel-title' }, 'Inbox settings')), form);
}

async function pageInbox(ctx, id) {
  if (!L.isUUID(id)) return pageNotFound(ctx, 'That is not an inbox ID.');
  const fetchPage = (before) => ctx.api('GET', L.listPath(`/inboxes/${encodeURIComponent(id)}/messages`, { limit: PAGE_SIZE, before }));
  const first = await fetchPage();
  let inbox = null;
  try {
    inbox = await ctx.api('GET', `/inboxes/${encodeURIComponent(id)}`);
  } catch (err) {
    if (isAbort(err)) throw err;
    try {
      const all = await ctx.api('GET', '/inboxes');
      inbox = ((all && all.data) || []).find((i) => i.id === id) || null;
    } catch (err2) {
      if (isAbort(err2)) throw err2;
    }
  }
  const title = inbox ? inboxAddress(inbox) : 'Inbox';
  const rows = first.data || [];
  const listStatus = formStatus();
  const refresh = h('button', { type: 'button', class: 'btn', onclick: () => ctx.reload() }, 'Refresh');
  let content = receivingHelp(inbox ? inboxAddress(inbox) : '');
  if (rows.length) {
    const table = dataTable([
      { label: 'Sender', class: 'col-status', cell: (m) => trustCell(m.trust) },
      { label: 'From', class: 'col-addr', cell: (m) => untrusted(mono(m.from || '—')) },
      {
        label: 'Subject',
        class: 'col-main',
        cell: (m) => h('span', { class: 'subject-cell' },
          h('span', { class: 'subject-line' }, untrusted(link(route('messages', m.id), m.subject || '(no subject)')), ' ', messageFlags(m)),
          m.snippet ? h('span', { class: 'snippet' }, untrusted(L.truncate(m.snippet, 160))) : null),
      },
      { label: 'Thread', cell: (m) => (m.in_reply_to_email_id ? badge('received', 'Reply to your email') : muted('New')) },
      { label: 'Received', class: 'col-time', cell: (m) => when(m.received_at) },
      {
        label: 'Actions',
        class: 'col-actions',
        cell: (m) => (m.held ? releaseButton(ctx, m, (res, err) => {
          if (err) listStatus.replaceChildren(errorView(err, 'inboxes') || '');
          else ctx.reload();
        }) : null),
      },
    ], rows, 'Messages');
    content = [
      h('p', { class: 'legend-note' }, senderTag(), ' From, subject and snippet are written by the sender and shown as text only. ',
        'The sender badge is what SPF, DKIM and DMARC prove.'),
      listStatus, table.el, pager(ctx, table, first, fetchPage, 'inbox messages'),
    ];
  }
  ctx.mount(
    backLink('#/inboxes', 'All inboxes'),
    pageHead(title, 'Received messages, newest first.', refresh),
    inbox ? holdSetting(ctx, inbox) : null,
    panel('Messages', content));
}

function messageBody(m) {
  const parts = [];
  if (m.text) parts.push(h('pre', { class: 'body untrusted-box' }, m.text));
  if (m.html) {
    if (!m.text) parts.push(notice('calm', 'HTML-only message', 'It is shown as source below. The dashboard never renders mail as HTML.'));
    parts.push(h('details', { class: 'source', open: !m.text }, h('summary', {}, 'HTML source'), h('pre', { class: 'body untrusted-box' }, m.html)));
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
        // Always a download, never rendered: the content type from the
        // sender is ignored, so an HTML attachment cannot run here.
        await download(ctx, `/messages/${encodeURIComponent(m.id)}/attachments/${encodeURIComponent(a.index)}`, a.filename);
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'attachments') || '');
      } finally {
        btn.disabled = false;
      }
    });
    return h('li', {}, untrusted(h('span', { class: 'mono' }, a.filename || `attachment ${a.index}`)),
      muted(` ${a.content_type || ''} ${L.formatBytes(a.size)}`), btn);
  })));
}

function replyForm(ctx, m) {
  const text = textarea({ name: 'text', required: true, rows: '5', spellcheck: 'true' });
  const to = input({ name: 'to', inputmode: 'email', placeholder: 'Default: the first Reply-To, or From' });
  const status = formStatus();
  const form = h('form', { class: 'form', novalidate: true },
    field('Your reply', text, 'Sent from this inbox as "Re: …", in the same thread.'),
    field('To (optional)', to, 'Separate several addresses with commas. Empty: only the first Reply-To address, or From.'),
    h('div', { class: 'form-actions' }, h('button', { type: 'submit', class: 'btn btn-primary' }, 'Send reply')),
    status);
  let idem = randomID();
  form.addEventListener('input', () => { idem = randomID(); });
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    status.replaceChildren();
    if (!text.value.trim()) {
      status.replaceChildren(fieldError('Write a reply first.'));
      return;
    }
    const body = { text: text.value };
    const toList = L.parseList(to.value, { lower: false });
    if (toList.length) body.to = toList;
    await withBusy(form, async () => {
      try {
        const res = await ctx.api('POST', `/messages/${encodeURIComponent(m.id)}/reply`, body, { idempotencyKey: idem });
        if (!ctx.live) return;
        form.reset();
        idem = randomID();
        const warnings = Array.isArray(res.warnings) ? res.warnings : [];
        if (res.held) heldChanged();
        toast(res.held ? 'Reply held for approval' : 'Reply queued', res.held ? 'warn' : 'ok');
        status.replaceChildren(
          res.held
            ? notice('warn', 'Reply held for approval', `Its key's require_approval rules matched (${(res.hold_reasons || []).map(L.holdReasonLabel).join(', ') || 'a rule'}).`,
              h('p', {}, link('#/held', 'See held emails'), ' · ', link(route('emails', res.id), 'Its timeline')))
            : notice('ok', 'Reply queued', (res.to || []).length ? `To ${res.to.join(', ')}.` : null,
              h('p', {}, link(route('emails', res.id), 'Track its delivery'))),
          warnings.length ? notice('warn', 'Check who got it', null, h('ul', { class: 'issues' }, warnings.map((w) => h('li', {}, w)))) : null);
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
  let sent = [];
  try {
    const [res, ours] = await Promise.all([
      ctx.api('GET', L.listPath(`/inboxes/${encodeURIComponent(m.inbox_id)}/messages`, { thread_id: m.thread_id, limit: 50 })),
      ctx.api('GET', L.listPath('/emails', { thread_id: m.thread_id, limit: 50 })).catch((err) => { if (isAbort(err)) throw err; return null; }),
    ]);
    items = (res && res.data) || [];
    // Only a server that applied the filter echoes thread_id; an older one
    // would list every email.
    if (ours && ours.thread_id === m.thread_id) sent = ours.data || [];
  } catch (err) {
    if (isAbort(err)) throw err;
    return null;
  }
  const all = L.threadItems(items, sent);
  if (all.length < 2 && !m.in_reply_to_email_id) return null;
  return panel(`Thread (${all.length})`,
    m.in_reply_to_email_id && !sent.some((e) => e.id === m.in_reply_to_email_id)
      ? h('p', {}, 'Answers ', link(route('emails', m.in_reply_to_email_id), 'an email you sent'), '.')
      : null,
    h('ol', { class: 'thread' }, all.map((x) => {
      if (x.kind === 'sent') {
        const e = x.item;
        return h('li', { class: 'thread-item sent' },
          h('div', { class: 'thread-head' }, toneBadge('info', 'You sent'), ' ', link(route('emails', e.id), e.subject || '(no subject)'),
            muted(' · '), badge(L.emailStatus(e)), muted(' · '), when(e.created_at)),
          h('p', { class: 'snippet' }, 'To ', mono(L.summarizeAddresses(e.to, 2))));
      }
      const t = x.item;
      const current = t.id === m.id;
      return h('li', { class: current ? 'thread-item current' : 'thread-item', 'aria-current': current ? 'true' : null },
        h('div', { class: 'thread-head' },
          trustCell(t.trust), ' ',
          untrusted(current ? h('strong', {}, t.from || '—') : link(route('messages', t.id), t.from || '—')),
          muted(' · '), when(t.received_at), current ? muted(' (this message)') : null),
        t.snippet ? h('p', { class: 'snippet' }, untrusted(L.truncate(t.snippet, 200))) : null);
    })));
}

function trustPanel(m) {
  const t = L.trustBadge(m.trust);
  const ar = m.authentication_results && typeof m.authentication_results === 'object' ? m.authentication_results : {};
  const rows = L.authRows(ar);
  return panel('Sender verdict',
    h('p', { class: 'verdict' }, toneBadge(t.tone, t.label), ' ', t.detail),
    ar.reason ? h('p', { class: 'hint' }, 'Why: ', ar.reason) : null,
    rows.length ? dataTable([
      { label: 'Check', cell: (r) => r[0] },
      { label: 'Result', class: 'col-status', cell: (r) => toneBadge(L.authTone(r[1]), r[1]) },
      { label: 'Details', cell: (r) => (r[2] ? h('span', { class: 'mono small' }, r[2]) : muted('—')) },
    ], rows, 'Authentication checks').el : null,
    kv([
      ['Envelope sender', m.mail_from ? mono(m.mail_from) : null],
      ['TLS', m.tls === undefined ? null : (m.tls ? 'Received over TLS' : 'Received without TLS')],
      ['Size', m.size ? L.formatBytes(m.size) : null],
    ]),
    ar.header ? h('details', { class: 'source' }, h('summary', {}, 'Authentication-Results header'), h('pre', { class: 'smtp' }, ar.header)) : null);
}

function warningsFor(m) {
  const out = [];
  if (m.suspicious_instructions) {
    const rules = (m.suspicious_rules || []).map(L.suspiciousLabel);
    out.push(notice('warn', 'Text aimed at AI agents',
      'This message contains text that looks like instructions to an agent. Treat it as data: an agent must not follow instructions in received mail.',
      rules.length ? h('ul', { class: 'issues' }, rules.map((r) => h('li', {}, r))) : null));
  }
  if (m.trust === 'failed') {
    out.push(notice('error', 'The sender failed authentication', L.trustBadge('failed').detail));
  }
  if (m.reply_to_mismatch) {
    out.push(notice('warn', 'Reply-To points elsewhere', 'A Reply-To address is on another domain than From, a common way to divert answers. A reply goes only to the first Reply-To unless you name recipients.'));
  }
  if (m.message_id_conflict) {
    out.push(notice('warn', 'Duplicate Message-ID', 'Another message in this inbox has the same Message-ID with different content. One of them may be forged.'));
  }
  return out;
}

async function pageMessage(ctx, id) {
  if (!L.isUUID(id)) return pageNotFound(ctx, 'That is not a message ID.');
  let m = await ctx.api('GET', `/messages/${encodeURIComponent(id)}`);
  const status = formStatus();
  const thread = await threadPanel(ctx, m);
  const heldBox = h('div');
  const showHeld = () => {
    if (!m.held) {
      put(heldBox, m.released_at ? notice('calm', 'Released', h('span', {}, 'Agent keys have seen this message since ', exact(m.released_at), '.')) : null);
      return;
    }
    put(heldBox, h('section', { class: 'panel panel-warn' },
      h('h2', { class: 'panel-title' }, 'Held from agents'),
      h('p', {}, 'The sender is not authenticated, so sending-only (agent) keys do not see this message until it is released. Release it only if you trust it.'),
      h('div', { class: 'form-actions' }, releaseButton(ctx, m, (res, err) => {
        if (err) {
          status.replaceChildren(errorView(err, 'inboxes') || '');
          return;
        }
        m = { ...m, held: false, released_at: res.released_at || new Date().toISOString() };
        showHeld();
      }, false))));
  };
  showHeld();
  const hd = m.headers || {};
  ctx.mount(
    backLink('#/mail', 'Mail'),
    pageHead(untrusted(m.subject || '(no subject)'),
      h('span', {}, trustCell(m.trust), ' From ', untrusted(mono(m.from || '—')), ' · ', when(m.received_at))),
    status,
    warningsFor(m),
    heldBox,
    thread,
    panel(null,
      h('div', { class: 'panel-head' }, h('h2', { class: 'panel-title' }, 'Message'), senderTag()),
      h('p', { class: 'hint' }, 'Everything below except the received time was written by the sender. It is shown as text only, and proves nothing on its own.'),
      kv([
        ['From', untrusted(mono(m.from || '—'))],
        ['Reply-To', L.addressList(m.reply_to).length ? untrusted(mono(L.addressList(m.reply_to).join(', '))) : null],
        ['To', mono(L.addressList(m.to).join(', '))],
        ['Cc', L.addressList(m.cc).length ? mono(L.addressList(m.cc).join(', ')) : null],
        ['Date', hd.date ? untrusted(hd.date) : null],
        ['Received', exact(m.received_at)],
        ['Message-ID', hd['message-id'] ? untrusted(mono(hd['message-id'])) : null],
        ['In-Reply-To', hd['in-reply-to'] ? untrusted(mono(hd['in-reply-to'])) : null],
      ]),
      messageBody(m)),
    trustPanel(m),
    attachmentList(ctx, m, status),
    panel('Reply', replyForm(ctx, m)));
}

function pageNotFound(ctx, detail) {
  ctx.mount(pageHead('Not found', detail || 'There is no page at this address.'),
    h('p', {}, link('#/overview', 'Go to the overview')));
}

// ---- router ----

const PAGES = {
  overview: { title: 'Overview', what: 'the overview', verb: 'see', render: (ctx, id) => (id ? pageNotFound(ctx) : pageOverview(ctx)) },
  compose: { title: 'Compose', what: 'emails', render: (ctx, id) => (id ? pageNotFound(ctx) : pageCompose(ctx)) },
  mail: { title: 'Mail', what: 'inboxes', render: (ctx, id) => (id ? pageNotFound(ctx) : pageMail(ctx, { onSeen: () => showMailCount('') })) },
  emails: { title: 'Emails', what: 'emails', render: (ctx, id) => (id ? pageEmail(ctx, id) : pageEmails(ctx)) },
  held: { title: 'Held emails', what: 'held emails', verb: 'see', render: (ctx, id) => (id ? pageNotFound(ctx) : pageHeld(ctx)) },
  suppressions: { title: 'Suppressions', what: 'suppressions', render: (ctx, id) => (id ? pageNotFound(ctx) : pageSuppressions(ctx)) },
  pauses: { title: 'Pauses', what: 'pauses', render: (ctx, id) => (id ? pageNotFound(ctx) : pagePauses(ctx)) },
  domains: { title: 'Domains', what: 'domains', render: (ctx, id) => (id ? pageDomain(ctx, id) : pageDomains(ctx)) },
  inboxes: { title: 'Inboxes', what: 'inboxes', render: (ctx, id) => (id ? pageInbox(ctx, id) : pageInboxes(ctx)) },
  messages: { title: 'Message', what: 'inboxes', nav: 'mail', render: (ctx, id) => (id ? pageMessage(ctx, id) : pageNotFound(ctx)) },
  webhooks: { title: 'Webhooks', what: 'webhooks', render: (ctx, id) => (id ? pageWebhook(ctx, id) : pageWebhooks(ctx)) },
  'api-keys': { title: 'API keys', what: 'API keys', render: (ctx, id) => (id ? pageAPIKey(ctx, id) : pageAPIKeys(ctx)) },
  settings: { title: 'Project settings', what: 'project settings', render: (ctx, id) => (id ? pageNotFound(ctx) : pageSettings(ctx)) },
  insights: { title: 'Insights', what: 'insights', verb: 'see', render: (ctx, id) => (id ? pageNotFound(ctx) : pageInsights(ctx)) },
  'audit-log': { title: 'Audit log', what: 'the audit log', verb: 'see', render: (ctx, id) => (id ? pageNotFound(ctx) : pageAuditLog(ctx)) },
  notfound: { title: 'Not found', what: 'this', render: (ctx) => pageNotFound(ctx) },
};

let current = null;
let navigated = false;

async function render() {
  // A hash that is not a route (the skip link's "#main") keeps the current
  // page, or shows the overview when nothing is shown yet.
  const r = L.parseRoute(location.hash) || (current ? null : { page: 'overview', id: '' });
  if (!r) return;
  if (!getKey()) {
    showSignIn();
    return;
  }
  showApp();
  setMenu(false);
  if (current) current.abort();
  current = new AbortController();
  const ctx = makeContext(current.signal, navigated);
  ctx.reload = () => render();
  navigated = true;
  const page = PAGES[r.page];
  for (const a of document.querySelectorAll('[data-nav]')) {
    if (a.dataset.nav === (page.nav || r.page)) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
  document.title = `${page.title} · OpenMailWire`;
  refreshHeld(false);
  if (r.page !== 'mail') refreshMail(false);
  ctx.mount(loadingLine());
  try {
    await page.render(ctx, r.id);
  } catch (err) {
    if (!ctx.live || isAbort(err)) return;
    ctx.mount(pageHead(page.title), errorView(err, page.what, page.verb));
  }
}

// ---- navigation: the menu on narrow screens, and the held count ----

function setMenu(open) {
  const btn = $('#menu-btn');
  btn.setAttribute('aria-expanded', String(open));
  $('#nav').classList.toggle('open', open);
}

function showShortcuts() {
  const row = (keys, what) => h('div', { class: 'kbd-row' }, h('dt', {}, keys.map((k, i) => [i ? muted(' then ') : null, h('kbd', {}, k)])), h('dd', {}, what));
  showDialog('Keyboard shortcuts',
    h('dl', { class: 'kbd-list' },
      row(['/'], 'Search on this page (emails, mail)'),
      row(['?'], 'Show these shortcuts'),
      row(['Esc'], 'Close a dialog or the menu'),
      L.SHORTCUTS.map(([k, , label]) => row(['g', k], `Go to ${label}`))));
}

function setupNav() {
  const btn = $('#menu-btn');
  btn.addEventListener('click', () => setMenu(btn.getAttribute('aria-expanded') !== 'true'));
  $('#shortcuts-btn').addEventListener('click', showShortcuts);
  let prev = '';
  let prevAt = 0;
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && btn.getAttribute('aria-expanded') === 'true') {
      setMenu(false);
      btn.focus();
      return;
    }
    if (!getKey() || e.ctrlKey || e.metaKey || e.altKey || L.isTyping(e.target) || document.querySelector('dialog[open]')) return;
    const target = Date.now() - prevAt < 1500 ? L.shortcutRoute(prev, e.key) : '';
    if (target) {
      e.preventDefault();
      prev = '';
      location.hash = target;
      return;
    }
    if (e.key === '/') {
      const search = document.querySelector('#main [data-search]');
      if (search) {
        e.preventDefault();
        search.focus();
        search.select();
      } else if (location.hash !== '#/emails') {
        e.preventDefault();
        location.hash = '#/emails';
      }
    } else if (e.key === '?') {
      e.preventDefault();
      showShortcuts();
    }
    prev = e.key.toLowerCase();
    prevAt = Date.now();
  });
}

const HELD_EVERY_MS = 60 * 1000;
let heldAt = 0;
let heldBusy = false;

function showCount(selector, text, suffix) {
  for (const el of document.querySelectorAll(selector)) {
    el.hidden = !text;
    put(el, text, text ? h('span', { class: 'sr-only' }, suffix) : null);
  }
}

const showHeldCount = (text) => showCount('[data-held-count]', text, ' waiting for approval');
const showMailCount = (text) => showCount('[data-mail-count]', text, ' new messages');

let mailAt = 0;
let mailBusy = false;

// refreshMail updates the count of messages received since the last visit
// to Mail, at most every minute.
async function refreshMail(force) {
  if (!getKey() || mailBusy || (!force && Date.now() - mailAt < 60000)) return;
  mailBusy = true;
  mailAt = Date.now();
  try {
    const n = await fetchNewCount((method, path) => request(getKey(), method, path));
    showMailCount(n ? String(n > 99 ? '99+' : n) : '');
  } catch {
    showMailCount('');
  } finally {
    mailBusy = false;
  }
}

// refreshHeld updates the held count in the nav, at most every 15 s unless
// forced (after a decision or a send).
async function refreshHeld(force) {
  if (!getKey() || heldBusy || (!force && Date.now() - heldAt < 15000)) return;
  heldBusy = true;
  heldAt = Date.now();
  try {
    const res = await request(getKey(), 'GET', L.listPath('/emails/held', { limit: 50 }));
    const n = ((res && res.data) || []).length;
    showHeldCount(n ? `${n}${res.has_more ? '+' : ''}` : '');
  } catch {
    showHeldCount('');
  } finally {
    heldBusy = false;
  }
}

// ---- sign-in ----

function showApp() {
  $('#signin').hidden = true;
  $('#topbar').hidden = false;
  $('#shell').hidden = false;
  $('#key-chip').textContent = L.keyLabel(getKey());
}

function showSignIn(message) {
  if (current) current.abort();
  current = null;
  $('#main').replaceChildren(); // drop anything shown, including one-time secrets
  $('#shell').hidden = true;
  $('#topbar').hidden = true;
  $('#signin').hidden = false;
  showHeldCount('');
  showMailCount('');
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
      status.replaceChildren(fieldError(problem));
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
        heldAt = 0;
        mailAt = 0;
        render();
      } catch (err) {
        const c = L.classifyError(err, 'domains');
        const text = c.kind === 'auth' ? (err.code === 'api_key_expired' ? 'This key has expired.' : 'This key is invalid or was revoked.') : `${c.title}. ${c.detail}`;
        status.replaceChildren(fieldError(text));
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

onUnauthorized((detail) => signOut(detail));
onHeldChange(() => refreshHeld(true));
setupSignIn();
setupNav();
window.addEventListener('hashchange', render);
setInterval(() => {
  if (document.hidden) return;
  refreshHeld(false);
  if (!location.hash.startsWith('#/mail')) refreshMail(false);
}, HELD_EVERY_MS);
render();
showVersion();
