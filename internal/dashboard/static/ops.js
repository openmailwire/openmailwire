// OpenMailWire dashboard: the operator pages added in v0.6. Held emails,
// suppressions, pauses, project settings, insights and the audit log.
// Same rules as app.js: API data reaches the page as text only.

import * as L from './lib.js';
import {
  h, svg, put, nextID, announce, link, route, mono, muted, toneBadge, when, exact,
  copyable, panel, pageHead, emptyState, kv, notice, errorView, dataTable, pager,
  field, input, textarea, select, checkbox, formStatus, fieldError, withBusy, fill, download,
  confirmed, heldChanged, getKey, isAbort,
} from './ui.js';

const plural = (n, one, many) => `${L.formatCount(n)} ${n === 1 ? one : many}`;

// keyDirectory lists the project's keys when this session may (full
// access): their names for display, and the id of the key signed in.
export async function keyDirectory(ctx) {
  const out = { names: new Map(), self: null, list: [] };
  try {
    const res = await ctx.api('GET', '/api-keys');
    out.list = (res && res.data) || [];
    for (const k of out.list) {
      out.names.set(k.id, k.name || k.prefix);
      if (L.isCurrentKey(k.prefix, getKey())) out.self = k.id;
    }
  } catch (err) {
    if (isAbort(err)) throw err;
  }
  return out;
}

function keyName(keys, id) {
  if (!id) return muted('—');
  const name = keys.names.get(id);
  return name ? h('span', {}, h('strong', {}, name), keys.self === id ? muted(' (this session)') : null) : mono(`${id.slice(0, 8)}…`);
}

// ---- held emails ----

function decidedNotice(res) {
  const rs = (res && res.recipients) || [];
  const sup = (res && res.suppressed) || [];
  if (res && res.decision === 'rejected') {
    return notice('calm', 'Rejected', rs.length ? `Nothing will be sent to ${rs.join(', ')}.` : 'Nothing will be sent.');
  }
  return notice('ok', 'Approved', rs.length ? `Queued for ${rs.join(', ')}.` : 'Queued.',
    sup.length ? h('p', {}, `Not sent to ${sup.join(', ')}: suppressed since the email was held.`) : null,
    res && res.id ? h('p', {}, link(route('emails', res.id), 'Follow its delivery')) : null);
}

// decisionForm approves or rejects one held email, with an optional note.
export function decisionForm(ctx, email, { recipients, onDecided }) {
  const note = textarea({ name: 'note', rows: '2', maxlength: '1000', spellcheck: 'true' });
  const approve = h('button', { type: 'button', class: 'btn btn-primary' }, 'Approve and send');
  const reject = h('button', { type: 'button', class: 'btn btn-danger' }, 'Reject');
  const status = formStatus();
  const form = h('form', { class: 'form decision', novalidate: true, 'aria-label': 'Approve or reject' },
    field('Note (optional)', note, 'Kept with the decision and sent in the webhook. At most 1,000 characters.'),
    h('div', { class: 'form-actions' }, approve, reject),
    status);
  form.addEventListener('submit', (e) => e.preventDefault());
  const n = (recipients || []).length;
  const decide = async (verb) => {
    const q = verb === 'approve'
      ? `Approve this email and send it to ${plural(n, 'recipient', 'recipients')} now?`
      : `Reject this email? Nothing is ever sent to its ${plural(n, 'recipient', 'recipients')}.`;
    if (!await confirmed(q, { title: verb === 'approve' ? 'Approve and send?' : 'Reject this email?', confirm: verb === 'approve' ? 'Approve and send' : 'Reject', danger: verb !== 'approve' })) return;
    status.replaceChildren();
    await withBusy(form, async () => {
      try {
        const body = note.value.trim() ? { note: note.value.trim() } : {};
        const res = await ctx.api('POST', `/emails/${encodeURIComponent(email.id)}/${verb}`, body);
        if (!ctx.live) return;
        heldChanged();
        announce(res.decision === 'approved' ? 'Approved' : 'Rejected');
        onDecided({ ...res, id: email.id });
      } catch (err) {
        if (!ctx.live) return;
        const c = L.classifyError(err);
        status.replaceChildren(c.kind === 'forbidden'
          ? notice('calm', "This key can't approve held emails", 'Approving and rejecting need a full-access key, or a key whose policy has can_approve.')
          : errorView(err, 'held emails') || '');
        if (c.kind === 'conflict') heldChanged();
      }
    });
  };
  approve.addEventListener('click', () => decide('approve'));
  reject.addEventListener('click', () => decide('reject'));
  return form;
}

function reasonChips(reasons) {
  const list = Array.isArray(reasons) ? reasons : [];
  if (!list.length) return muted('—');
  return h('span', { class: 'chips' }, list.map((r) => toneBadge('warn', L.holdReasonLabel(r))));
}

function heldCard(ctx, e, keys) {
  const titleID = nextID('held');
  const card = h('article', { class: 'panel held-card', 'aria-labelledby': titleID });
  const own = keys.self && e.api_key_id === keys.self;
  const waiting = L.addressList(e.recipients);
  const summary = () => [
    h('div', { class: 'card-head' },
      h('h2', { class: 'panel-title', id: titleID }, e.subject || '(no subject)'),
      h('span', { class: 'chips' }, toneBadge('muted', L.STREAM_LABELS[e.stream] || L.statusLabel(e.stream)))),
    kv([
      ['Why held', reasonChips(e.hold_reasons)],
      ['From', mono(e.from)],
      ['Waiting', mono(waiting.join(', '))],
      ['To', mono(L.addressList(e.to).join(', '))],
      ['Cc', L.addressList(e.cc).length ? mono(L.addressList(e.cc).join(', ')) : null],
      ['Bcc', L.addressList(e.bcc).length ? mono(L.addressList(e.bcc).join(', ')) : null],
      ['Sent by', keyName(keys, e.api_key_id)],
      ['Held', when(e.created_at)],
      ['Expires', h('span', {}, exact(e.expires_at), muted(` · ${L.relativeTime(e.expires_at)}`))],
      ['Scheduled for', e.scheduled_at ? exact(e.scheduled_at) : null],
      ['Email', link(route('emails', e.id), 'Open its timeline')],
    ]),
  ];
  put(card,
    summary(),
    h('div', { class: 'preview' },
      h('p', { class: 'preview-label' }, 'Text, as the sending agent wrote it',
        e.text_truncated ? muted(' (the start only)') : null),
      e.text ? h('pre', { class: 'body' }, e.text) : emptyState('No text to preview.')),
    own ? notice('warn', "Sent with this session's key",
      'No key approves its own emails: approve it with another key that has can_approve, or with a full-access key. You can still reject it.') : null,
    decisionForm(ctx, e, {
      recipients: waiting,
      onDecided: (res) => {
        put(card, summary(), decidedNotice(res));
        card.classList.add('decided');
      },
    }));
  return card;
}

export async function pageHeld(ctx) {
  const fetchPage = (before) => ctx.api('GET', L.listPath('/emails/held', { limit: 20, before }));
  const first = await fetchPage();
  const keys = await keyDirectory(ctx);
  const rows = first.data || [];
  const list = h('div', { class: 'cards' });
  const sink = { add: (items) => { for (const e of items) list.append(heldCard(ctx, e, keys)); } };
  sink.add(rows);
  const refresh = h('button', { type: 'button', class: 'btn', onclick: () => { heldChanged(); ctx.reload(); } }, 'Refresh');
  ctx.mount(
    pageHead('Held emails',
      "Emails that matched a key's require_approval rules. Nothing is sent until someone approves; held mail nobody decides on expires.",
      refresh),
    rows.length
      ? [list, pager(ctx, sink, first, fetchPage, 'held emails')]
      : panel(null, emptyState('Nothing is waiting for approval.'),
        h('p', { class: 'hint' }, 'Emails from keys whose policy has require_approval wait here. Set the rules on ', link('#/api-keys', 'API keys'), '.')));
}

// ---- suppressions ----

const REASONS = ['hard_bounce', 'complaint', 'unsubscribe', 'manual', 'customer_request'];

function reasonList(id) {
  return h('datalist', { id }, REASONS.map((r) => h('option', { value: r })));
}

export async function pageSuppressions(ctx) {
  let filters = {};
  const list = h('section', { class: 'panel' });
  const listHead = h('h2', { class: 'panel-title' }, 'Suppressed addresses');
  const listStatus = formStatus();
  const fetchPage = (before) => ctx.api('GET', L.listPath('/suppressions', { limit: 50, before, ...filters }));

  const removeButton = (row) => {
    const btn = h('button', { type: 'button', class: 'btn btn-small btn-danger', 'aria-label': `Remove ${row.address} (${L.STREAM_LABELS[row.stream] || row.stream})` }, 'Remove');
    btn.addEventListener('click', async () => {
      if (!await confirmed(`Only do this when ${row.address} is known to work and to want the mail.`, { title: 'Send to this address again?', confirm: 'Remove suppression', danger: true })) return;
      btn.disabled = true;
      try {
        await ctx.api('DELETE', L.listPath(`/suppressions/${encodeURIComponent(row.address)}`, { stream: row.stream }));
        if (!ctx.live) return;
        announce(`${row.address} removed`);
        loadList();
      } catch (err) {
        if (ctx.live) listStatus.replaceChildren(errorView(err, 'suppressions') || '');
        btn.disabled = false;
      }
    });
    return btn;
  };
  const columns = [
    { label: 'Address', class: 'col-main col-addr', cell: (r) => mono(r.address) },
    { label: 'Stream', class: 'col-status', cell: (r) => toneBadge(r.stream === 'all' ? 'bad' : 'warn', L.STREAM_LABELS[r.stream] || r.stream) },
    { label: 'Reason', cell: (r) => L.statusLabel(r.reason) },
    { label: 'Detail', cell: (r) => (r.detail ? h('span', { class: 'mono small' }, r.detail) : muted('—')) },
    { label: 'Added', class: 'col-time', cell: (r) => when(r.created_at) },
    { label: 'Actions', class: 'col-actions', cell: (r) => removeButton(r) },
  ];
  const loadList = () => fill(ctx, list, listHead, 'suppressions', async () => {
    const first = await fetchPage();
    const rows = first.data || [];
    if (!rows.length) {
      return [listStatus, emptyState(Object.keys(filters).length ? 'No suppression matches these filters.' : 'No suppressed addresses.')];
    }
    const table = dataTable(columns, rows, 'Suppressed addresses');
    return [listStatus, table.el, pager(ctx, table, first, fetchPage, 'suppressions')];
  });

  // Filters.
  const fAddress = input({ name: 'address', inputmode: 'email', placeholder: 'jane@example.org' });
  const fDomain = input({ name: 'domain', placeholder: 'example.org' });
  const fReasonList = nextID('reasons');
  const fReason = input({ name: 'reason', list: fReasonList, placeholder: 'Any' });
  const fStream = select({ name: 'stream' }, [['', 'Any'], ['all', 'All mail'], ['broadcast', 'Broadcast only']]);
  const filterForm = h('form', { class: 'form filters', novalidate: true, 'aria-label': 'Filter suppressions' },
    h('div', { class: 'grid-4' },
      field('Address', fAddress), field('Domain', fDomain), field('Reason', fReason), field('Stream', fStream)),
    reasonList(fReasonList),
    h('div', { class: 'form-actions' },
      h('button', { type: 'submit', class: 'btn' }, 'Apply filters'),
      h('button', { type: 'button', class: 'btn btn-quiet', onclick: () => { filterForm.reset(); filters = {}; loadList(); } }, 'Clear')));
  filterForm.addEventListener('submit', (e) => {
    e.preventDefault();
    filters = L.suppressionQuery({ address: fAddress.value, domain: fDomain.value, reason: fReason.value, stream: fStream.value });
    loadList();
  });

  // Export.
  const exportStatus = formStatus();
  const exportBtn = h('button', { type: 'button', class: 'btn' }, 'Export CSV');
  exportBtn.addEventListener('click', async () => {
    exportBtn.disabled = true;
    exportStatus.replaceChildren();
    try {
      await download(ctx, L.listPath('/suppressions/export', filters), L.datedFilename('suppressions', 'csv'));
      announce('Export downloaded');
    } catch (err) {
      if (ctx.live) exportStatus.replaceChildren(errorView(err, 'suppressions') || '');
    } finally {
      exportBtn.disabled = false;
    }
  });

  // Add.
  const aAddress = input({ name: 'address', required: true, inputmode: 'email', placeholder: 'jane@example.org' });
  const aStream = select({ name: 'stream' }, [['all', 'All mail'], ['broadcast', 'Broadcast only']], 'all');
  const aReasonList = nextID('reasons');
  const aReason = input({ name: 'reason', list: aReasonList, placeholder: 'manual', maxlength: '32' });
  const aDetail = input({ name: 'detail', maxlength: '300', placeholder: 'Asked by phone', spellcheck: 'true' });
  const addStatus = formStatus();
  const addForm = h('form', { class: 'form', novalidate: true },
    h('div', { class: 'grid-2' },
      field('Address', aAddress),
      field('Stream', aStream, 'All mail stops everything; broadcast only stops newsletters and other bulk mail.'),
      field('Reason', aReason, 'A short name, like customer_request. Default: manual.'),
      field('Detail (optional)', aDetail, 'Free text kept with it, up to 300 characters.')),
    reasonList(aReasonList),
    h('div', { class: 'form-actions' }, h('button', { type: 'submit', class: 'btn btn-primary' }, 'Suppress address')),
    addStatus);
  addForm.addEventListener('submit', async (e) => {
    e.preventDefault();
    addStatus.replaceChildren();
    const address = aAddress.value.trim();
    if (!address) {
      addStatus.replaceChildren(fieldError('Enter an address.'));
      return;
    }
    const problem = L.checkSuppressionReason(aReason.value);
    if (problem) {
      addStatus.replaceChildren(fieldError(problem));
      return;
    }
    const body = { address, stream: aStream.value };
    if (aReason.value.trim()) body.reason = aReason.value.trim();
    if (aDetail.value.trim()) body.detail = aDetail.value.trim();
    await withBusy(addForm, async () => {
      try {
        const res = await ctx.api('POST', '/suppressions', body);
        if (!ctx.live) return;
        addForm.reset();
        addStatus.replaceChildren(res.created === false
          ? notice('calm', 'Already suppressed', `${res.address} was already suppressed for ${(L.STREAM_LABELS[res.stream] || res.stream).toLowerCase()} (${L.statusLabel(res.reason)}).`)
          : notice('ok', 'Suppressed', `Nothing more is sent to ${res.address}${res.stream === 'broadcast' ? ' as broadcast mail' : ''}.`));
        loadList();
      } catch (err) {
        if (ctx.live) addStatus.replaceChildren(errorView(err, 'suppressions') || '');
      }
    });
  });

  ctx.mount(
    pageHead('Suppressions',
      'Addresses this project does not send to. Bounces and complaints add them for every stream; one-click unsubscribes for broadcast mail only.',
      exportBtn),
    exportStatus,
    h('details', { class: 'panel collapsible' }, h('summary', {}, h('span', { class: 'panel-title' }, 'Suppress an address')), addForm),
    panel('Filter', filterForm),
    list);
  await loadList();
}

// ---- pauses ----

function rateCell(rate, limit) {
  const b = L.rateBar(rate, limit);
  return h('span', { class: 'rate-cell' },
    h('span', {}, h('strong', {}, L.formatRate(rate)), muted(` · limit ${L.formatRate(limit)}`)),
    svg('svg', { class: 'bar', viewBox: '0 0 100 8', preserveAspectRatio: 'none', 'aria-hidden': 'true', focusable: 'false' },
      svg('rect', { class: 'bar-track', x: 0, y: 0, width: 100, height: 8 }),
      svg('rect', { class: 'bar-bad', x: 0, y: 0, width: b.value, height: 8 }),
      svg('line', { class: 'bar-limit', x1: b.limit, x2: b.limit, y1: 0, y2: 8 })));
}

function scopeCell(p) {
  const label = L.PAUSE_SCOPES[p.scope] || L.statusLabel(p.scope);
  const name = p.scope_name || (p.scope_id ? `${String(p.scope_id).slice(0, 8)}…` : '');
  let target = null;
  if (p.scope === 'domain' && L.isUUID(p.scope_id)) target = route('domains', p.scope_id);
  if (p.scope === 'api_key' && L.isUUID(p.scope_id)) target = route('api-keys', p.scope_id);
  if (p.scope === 'project') target = '#/settings';
  return h('span', { class: 'scope-cell' }, toneBadge('muted', label), ' ', target ? link(target, name || label) : mono(name));
}

export async function pagePauses(ctx) {
  const box = h('div');
  const status = formStatus();
  const load = async () => {
    const [res, keys] = await Promise.all([ctx.api('GET', L.listPath('/pauses', { limit: 100 })), keyDirectory(ctx)]);
    const rows = (res && res.data) || [];
    const active = rows.filter((p) => p.active);
    const past = rows.filter((p) => !p.active);
    const resumeButton = (p) => {
      const what = `${(L.PAUSE_SCOPES[p.scope] || p.scope).toLowerCase()} ${p.scope_name || ''}`.trim();
      const btn = h('button', { type: 'button', class: 'btn btn-small', 'aria-label': `Resume ${what}` }, 'Resume');
      btn.addEventListener('click', async () => {
        if (!await confirmed(`Its counts start over, so the same bounces will not pause it again. Find out why recipients bounced or complained first.`, { title: `Resume sending for the ${what}?`, confirm: 'Resume' })) return;
        btn.disabled = true;
        status.replaceChildren();
        try {
          await ctx.api('POST', `/pauses/${encodeURIComponent(p.id)}/resume`);
          if (!ctx.live) return;
          announce('Sending resumed');
          await render(notice('ok', 'Resumed', `The ${what} can send again.`));
        } catch (err) {
          if (ctx.live) status.replaceChildren(errorView(err, 'pauses') || '');
          btn.disabled = false;
        }
      });
      return btn;
    };
    const common = [
      { label: 'Scope', class: 'col-main', cell: scopeCell },
      { label: 'Reason', cell: (p) => L.PAUSE_REASONS[p.reason] || L.statusLabel(p.reason) },
      { label: 'Rate (24 h)', class: 'col-rate', cell: (p) => rateCell(p.rate, p.threshold) },
      { label: 'Counted', cell: (p) => L.pauseCounts(p) },
    ];
    return [
      panel(`In force (${active.length})`, active.length
        ? dataTable([...common,
          { label: 'Since', class: 'col-time', cell: (p) => when(p.created_at) },
          { label: 'Actions', class: 'col-actions', cell: resumeButton },
        ], active, 'Pauses in force').el
        : notice('ok', 'Nothing is paused', 'Every key, domain and the project can send.')),
      past.length ? panel('Resumed', dataTable([...common,
        { label: 'Paused', class: 'col-time', cell: (p) => exact(p.created_at) },
        { label: 'Resumed', class: 'col-time', cell: (p) => exact(p.resumed_at) },
        { label: 'By', cell: (p) => keyName(keys, p.resumed_by) },
      ], past, 'Resumed pauses').el) : null,
    ];
  };
  const render = async (message) => {
    const nodes = await load();
    if (ctx.live) put(box, message || null, status, nodes);
  };
  await render();
  ctx.mount(
    pageHead('Pauses',
      "Circuit breakers. Sending pauses by itself for an API key, a domain or the whole project when its bounce or complaint rate over 24 hours goes over the project's limits; its sends are refused with 403 sending_paused until someone resumes it.",
      link('#/settings', 'Change the limits')),
    box);
}

// ---- project settings ----

export async function pageSettings(ctx) {
  let project = await ctx.api('GET', '/project');
  const cap = input({ name: 'max_per_recipient_per_day', type: 'number', min: '1', max: '10000', step: '1', inputmode: 'numeric', placeholder: 'No cap' });
  const enabled = checkbox('Pause sending when a rate goes over its limit', { name: 'enabled' });
  const bounce = input({ name: 'bounce_rate', type: 'number', min: '0.001', max: '100', step: 'any', inputmode: 'decimal' });
  const complaint = input({ name: 'complaint_rate', type: 'number', min: '0.001', max: '100', step: 'any', inputmode: 'decimal' });
  const minRcpt = input({ name: 'min_recipients', type: 'number', min: '1', step: '1', inputmode: 'numeric' });
  const status = formStatus();
  const updated = h('p', { class: 'hint' });
  const show = () => {
    const cb = project.circuit_breaker || {};
    cap.value = project.max_per_recipient_per_day != null ? String(project.max_per_recipient_per_day) : '';
    enabled.cb.checked = cb.enabled !== false;
    bounce.value = L.percentInput(cb.bounce_rate);
    complaint.value = L.percentInput(cb.complaint_rate);
    minRcpt.value = cb.min_recipients != null ? String(cb.min_recipients) : '';
    put(updated, project.updated_at ? ['Last changed ', when(project.updated_at), '.'] : 'Never changed: these are the defaults.');
  };
  const form = h('form', { class: 'form', novalidate: true },
    h('fieldset', { class: 'fieldset' },
      h('legend', {}, 'Per-recipient cap'),
      field('Emails per recipient per day', cap,
        'At most this many emails to one address in any 24 hours, across every key of the project, from 1 to 10,000. Empty: no cap.')),
    h('fieldset', { class: 'fieldset' },
      h('legend', {}, 'Circuit breaker'),
      enabled.el,
      h('div', { class: 'grid-3' },
        field('Bounce rate limit (%)', bounce, 'Default 5. Bounces over 24 hours, as a share of recipients.'),
        field('Complaint rate limit (%)', complaint, 'Default 0.1. Spam complaints, as a share of recipients.'),
        field('Minimum recipients', minRcpt, 'Default 50. Rates count only once a key, domain or the project has queued this many in 24 hours.'))),
    h('div', { class: 'form-actions' }, h('button', { type: 'submit', class: 'btn btn-primary' }, 'Save settings'), updated),
    status);
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    status.replaceChildren();
    let patch;
    try {
      patch = L.projectPatch(project, {
        maxPerRecipientPerDay: cap.value, enabled: enabled.cb.checked,
        bounceRate: bounce.value, complaintRate: complaint.value, minRecipients: minRcpt.value,
      });
    } catch (err) {
      status.replaceChildren(fieldError(err.message));
      return;
    }
    if (!Object.keys(patch).length) {
      status.replaceChildren(notice('calm', 'Nothing changed', null));
      return;
    }
    await withBusy(form, async () => {
      try {
        project = await ctx.api('PATCH', '/project', patch);
        if (!ctx.live) return;
        show();
        status.replaceChildren(notice('ok', 'Saved', 'The new settings apply to the next send.'));
        announce('Settings saved');
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'project settings') || '');
      }
    });
  });
  show();
  ctx.mount(
    pageHead('Project settings', h('span', {}, 'Sending rules for every key of ', h('strong', {}, project.name || 'this project'), '.'),
      link('#/pauses', 'See the pauses')),
    panel(null, form));
}

// ---- insights ----

function outcomeBar(stats) {
  const shares = L.outcomeShares(stats);
  let x = 0;
  return svg('svg', { class: 'bar bar-outcomes', viewBox: '0 0 100 8', preserveAspectRatio: 'none', 'aria-hidden': 'true', focusable: 'false' },
    svg('rect', { class: 'bar-track', x: 0, y: 0, width: 100, height: 8 }),
    shares.map((sh) => {
      const r = svg('rect', { class: `bar-${sh.key}`, x, y: 0, width: sh.percent, height: 8 });
      x += sh.percent;
      return r;
    }));
}

function legend() {
  return h('p', { class: 'legend', 'aria-hidden': 'true' }, L.OUTCOMES.map(([k, label]) => h('span', { class: 'legend-item' },
    svg('svg', { class: 'legend-swatch', viewBox: '0 0 10 10', 'aria-hidden': 'true', focusable: 'false' },
      svg('rect', { class: `bar-${k}`, x: 0, y: 0, width: 10, height: 10 })), label)));
}

const countRate = (count, rate) => h('span', { class: 'num' }, h('strong', {}, L.formatRate(rate)), muted(` ${L.formatCount(count ?? 0)}`));

function insightTable(groups, kind) {
  const rows = L.byAttempts(groups);
  if (!rows.length) return emptyState('No delivery attempts in this window.');
  const first = kind === 'provider'
    ? { label: 'Provider', class: 'col-main', cell: (g) => L.providerLabel(g.provider) }
    : { label: 'Sending IP', class: 'col-main', cell: (g) => mono(g.sending_ip || '—') };
  return dataTable([
    first,
    { label: 'Attempts', class: 'col-num', cell: (g) => L.formatCount(g.attempts ?? 0) },
    { label: 'Accepted', class: 'col-num', cell: (g) => countRate(g.accepted, g.acceptance_rate) },
    { label: 'Deferred', class: 'col-num', cell: (g) => countRate(g.deferred, g.deferral_rate) },
    { label: 'Bounced', class: 'col-num', cell: (g) => countRate(g.bounced, g.bounce_rate) },
    { label: 'Unknown', class: 'col-num', cell: (g) => L.formatCount(g.unknown ?? 0) },
    { label: 'Median', class: 'col-num', cell: (g) => L.formatSeconds(g.time_to_acceptance_p50_seconds) },
    { label: '95th pct.', class: 'col-num', cell: (g) => L.formatSeconds(g.time_to_acceptance_p95_seconds) },
    { label: 'Outcomes', class: 'col-bar', cell: (g) => outcomeBar(g) },
  ], rows, kind === 'provider' ? 'By mailbox provider' : 'By sending IP').el;
}

function statTiles(t) {
  const tile = (label, value, sub) => h('div', { class: 'stat' }, h('dt', {}, label), h('dd', {}, h('span', { class: 'stat-value' }, value), sub ? muted(sub) : null));
  return h('dl', { class: 'stats' },
    tile('Attempts', L.formatCount(t.attempts ?? 0)),
    tile('Accepted', L.formatRate(t.acceptance_rate), L.formatCount(t.accepted ?? 0)),
    tile('Deferred', L.formatRate(t.deferral_rate), L.formatCount(t.deferred ?? 0)),
    tile('Bounced', L.formatRate(t.bounce_rate), L.formatCount(t.bounced ?? 0)),
    tile('Time to acceptance', L.formatSeconds(t.time_to_acceptance_p50_seconds), 'median'),
    tile('95th percentile', L.formatSeconds(t.time_to_acceptance_p95_seconds)));
}

export async function pageInsights(ctx) {
  const data = await ctx.api('GET', '/insights');
  let current = '24h';
  const body = h('div');
  const name = nextID('window');
  const switcher = h('fieldset', { class: 'segmented' },
    h('legend', { class: 'sr-only' }, 'Window'),
    L.INSIGHT_WINDOWS.map(([w, label]) => {
      const r = h('input', { type: 'radio', name, value: w, id: nextID('w') });
      r.checked = w === current;
      r.addEventListener('change', () => {
        current = w;
        show();
        announce(`Showing the last ${label}`);
      });
      return h('span', { class: 'segment' }, r, h('label', { for: r.id }, `Last ${label}`));
    }));
  const show = () => {
    const w = L.insightWindow(data, current);
    const label = (L.INSIGHT_WINDOWS.find(([k]) => k === current) || [])[1] || current;
    put(body,
      panel(`Totals, last ${label}`, statTiles(w.total || {})),
      panel('By mailbox provider', h('p', { class: 'hint' }, 'Recognised by MX host. Rates are shares of attempts.'), legend(), insightTable(w.by_provider, 'provider')),
      panel('By sending IP', legend(), insightTable(w.by_sending_ip, 'ip')));
  };
  show();
  ctx.mount(
    pageHead('Insights',
      h('span', {}, 'Delivery outcomes from the attempts of emails sent in each window. Generated ', when(data.generated_at),
        ` from ${plural(Number(data.emails_considered) || 0, 'email', 'emails')}.`),
      switcher),
    data.truncated ? notice('calm', 'Busy project: newest 100,000 emails only', 'More than 100,000 emails were sent in 30 days, so the longer windows cover only the newest 100,000.') : null,
    body);
}

// ---- audit log ----

function auditDetails(e) {
  const pairs = L.eventDetails(e.data);
  if (!pairs.length) return muted('—');
  return h('dl', { class: 'facts' }, pairs.map(([k, v]) => h('div', {}, h('dt', {}, k), h('dd', {}, v))));
}

function auditHash(e) {
  return h('details', { class: 'hash' },
    h('summary', {}, h('span', { class: 'mono' }, L.shortHash(e.hash))),
    kv([
      ['Hash', mono(e.hash)],
      ['Previous', mono(e.prev_hash)],
      ['Entry', h('pre', { class: 'smtp' }, e.entry)],
    ]));
}

function verifyResult(v) {
  if (v.valid) {
    return notice('ok', 'The chain is intact', `${plural(Number(v.entries) || 0, 'entry', 'entries')} checked.`,
      v.head_hash ? h('p', {}, 'Head hash ', copyable(v.head_hash, 'head hash')) : null,
      h('p', { class: 'small' }, "Keep the head hash somewhere else: if a later check's chain no longer contains it, entries were removed from the end."));
  }
  return notice('error', 'The chain is broken',
    `Entry ${v.first_invalid_id ?? '?'} does not check out${v.problem ? `: ${v.problem}` : ''}. Entries were edited or removed; restore the database from a backup and compare.`,
    h('p', { class: 'small' }, `${plural(Number(v.entries) || 0, 'entry', 'entries')} read.`));
}

export async function pageAuditLog(ctx) {
  let action = '';
  const fetchPage = (before) => ctx.api('GET', L.listPath('/audit-log', { limit: 50, before, action }));
  const first = await fetchPage();
  const keys = await keyDirectory(ctx);
  const list = h('section', { class: 'panel' });
  const head = h('h2', { class: 'panel-title' }, 'Entries');
  const columns = [
    { label: 'When', class: 'col-time', cell: (e) => exact(e.created_at) },
    { label: 'Actor', cell: (e) => L.auditActor(e, keys.names) },
    { label: 'Action', class: 'col-main', cell: (e) => mono(e.action) },
    { label: 'Target', cell: (e) => (e.target_id ? h('span', {}, `${L.statusLabel(e.target_type)} `, mono(e.target_id)) : L.statusLabel(e.target_type)) },
    { label: 'Details', cell: auditDetails },
    { label: 'Hash', class: 'col-hash', cell: auditHash },
  ];
  const showList = (page) => {
    const rows = page.data || [];
    if (!rows.length) return emptyState(action ? 'No entries with this action.' : 'No entries yet.');
    const table = dataTable(columns, rows, 'Audit log');
    return [table.el, pager(ctx, table, page, fetchPage, 'the audit log')];
  };
  put(list, head, showList(first));

  const verifyBox = formStatus();
  const verify = h('button', { type: 'button', class: 'btn btn-primary' }, 'Verify the chain');
  verify.addEventListener('click', async () => {
    verify.disabled = true;
    verify.textContent = 'Verifying…';
    verifyBox.replaceChildren();
    try {
      const v = await ctx.api('GET', '/audit-log/verify');
      if (!ctx.live) return;
      verifyBox.replaceChildren(verifyResult(v));
      announce(v.valid ? 'The chain is intact' : 'The chain is broken');
    } catch (err) {
      if (ctx.live) verifyBox.replaceChildren(errorView(err, 'the audit log', 'see') || '');
    } finally {
      verify.disabled = false;
      verify.textContent = 'Verify the chain';
    }
  });

  const actionsID = nextID('actions');
  const fAction = input({ id: nextID('action'), name: 'action', list: actionsID, placeholder: 'Any action', class: 'input mono' });
  const filter = h('form', { class: 'inline-form', 'aria-label': 'Filter by action' },
    h('label', { class: 'sr-only', for: fAction.id }, 'Action'),
    fAction, h('datalist', { id: actionsID }, L.AUDIT_ACTIONS.map((a) => h('option', { value: a }))),
    h('button', { type: 'submit', class: 'btn' }, 'Filter'),
    h('button', { type: 'button', class: 'btn btn-quiet', onclick: () => { fAction.value = ''; filter.requestSubmit(); } }, 'Clear'));
  filter.addEventListener('submit', (e) => {
    e.preventDefault();
    action = fAction.value.trim();
    fill(ctx, list, head, 'the audit log', async () => showList(await fetchPage()));
  });

  ctx.mount(
    pageHead('Audit log',
      'Who changed what in this project, newest first. The log is append-only and hash-chained: each entry hashes the one before it.',
      verify),
    verifyBox,
    panel(null, filter),
    list);
}
