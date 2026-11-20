// OpenMailWire dashboard: the overview, the default page. The state of
// the whole system at a glance (GET /overview), the setup checklist for a
// new project, and recent emails. It refreshes itself while visible.

import * as L from './lib.js';
import {
  h, svg, put, link, route, mono, muted, badge, toneBadge, when, copyable, pageHead, emptyState,
  errorView, dataTable, loadingLine, getPref, setPref, toast, isAbort, sleep,
} from './ui.js';

const REFRESH_MS = 20000;

function tile(title, target, linkLabel, content, cls = '') {
  return h('section', { class: `tile ${cls}`.trim() },
    h('div', { class: 'tile-head' }, h('h2', { class: 'tile-title' }, title),
      target ? link(target, linkLabel, h('span', { 'aria-hidden': 'true' }, ' →')) : null),
    content);
}

function bigNumber(n, label, tone) {
  return h('p', { class: `big tone-${tone || 'muted'}` }, h('span', { class: 'big-n' }, L.formatCount(n ?? 0)), ' ', h('span', { class: 'big-label' }, label));
}

// ---- sending ----

function trendChart(daily) {
  const bars = L.dailyBars(daily);
  if (!bars.length) return null;
  const w = 10;
  return h('figure', { class: 'trend' },
    svg('svg', { class: 'trend-svg', viewBox: `0 0 ${bars.length * w} 40`, preserveAspectRatio: 'none', 'aria-hidden': 'true', focusable: 'false' },
      bars.map((b, i) => {
        const x = i * w + 1.5;
        const y = (v) => 40 - (v / 100) * 38;
        const rest = Math.max(0, b.height - b.deliveredHeight - b.bouncedHeight);
        return [
          svg('rect', { class: 'trend-track', x, y: 0, width: w - 3, height: 40 }),
          rest ? svg('rect', { class: 'bar-unknown', x, y: y(b.height), width: w - 3, height: (rest / 100) * 38 }) : null,
          b.bouncedHeight ? svg('rect', { class: 'bar-bounced', x, y: y(b.deliveredHeight + b.bouncedHeight), width: w - 3, height: (b.bouncedHeight / 100) * 38 }) : null,
          b.deliveredHeight ? svg('rect', { class: 'bar-accepted', x, y: y(b.deliveredHeight), width: w - 3, height: (b.deliveredHeight / 100) * 38 }) : null,
        ];
      })),
    h('ol', { class: 'trend-days', 'aria-hidden': 'true' }, bars.map((b) => h('li', {}, b.label))),
    h('figcaption', { class: 'sr-only' }, 'Recipients per day, last 7 days: ',
      bars.map((b) => `${b.day}: ${b.total} recipients, ${b.delivered} delivered, ${b.bounced} bounced`).join('; '), '.'));
}

function rateRow(label, w24, w7, key, countKey, tone) {
  return h('div', { class: 'rate-row' },
    h('dt', {}, label),
    h('dd', {}, h('span', { class: `rate-main tone-text-${tone}` }, L.formatRate(w24[key])), muted(` ${L.formatCount(w24[countKey] ?? 0)}`)),
    h('dd', { class: 'rate-7d' }, muted('7 d '), L.formatRate(w7[key])));
}

function sendingTile(o) {
  const s = o.sending || {};
  const ws = Array.isArray(s.windows) ? s.windows : [];
  const w24 = ws.find((w) => w.window === '24h') || {};
  const w7 = ws.find((w) => w.window === '7d') || {};
  return tile('Sending, last 24 hours', '#/insights', 'Insights', [
    h('div', { class: 'sending' },
      h('div', {},
        bigNumber(w24.recipients, (w24.recipients === 1 ? 'recipient' : 'recipients'), 'info'),
        h('p', { class: 'small muted' }, `${L.formatCount(w24.emails ?? 0)} emails · 7 days: ${L.formatCount(w7.recipients ?? 0)} recipients`),
        h('dl', { class: 'rates' },
          rateRow('Delivered', w24, w7, 'delivery_rate', 'delivered', 'ok'),
          rateRow('Bounced', w24, w7, 'bounce_rate', 'bounced', 'bad'),
          rateRow('Complaints', w24, w7, 'complaint_rate', 'complained', 'bad'))),
      trendChart(s.daily)),
    s.truncated ? h('p', { class: 'hint' }, 'Counted over the newest 100,000 emails.') : null,
  ], 'tile-wide');
}

function queueTile(o) {
  const q = o.queue || {};
  const total = L.QUEUE_STATES.reduce((a, [k]) => a + (Number(q[k]) || 0), 0);
  return tile('Queue', '#/emails', 'Emails', [
    bigNumber(total, total === 1 ? 'recipient in flight' : 'recipients in flight', total ? 'info' : 'muted'),
    h('dl', { class: 'counts' }, L.QUEUE_STATES.map(([k, label, tone]) => h('div', { class: Number(q[k]) ? '' : 'zero' },
      h('dt', {}, h('span', { class: `dot tone-${tone}`, 'aria-hidden': 'true' }), label), h('dd', {}, L.formatCount(q[k] ?? 0))))),
    h('p', { class: 'small' }, 'Oldest waiting: ', q.oldest_age_seconds != null
      ? h('strong', { title: q.oldest_queued_at ? L.formatDateTime(q.oldest_queued_at) : null }, L.formatAge(q.oldest_age_seconds))
      : muted('nothing waiting')),
  ]);
}

function heldTile(o) {
  const n = Number(o.held_emails) || 0;
  return tile('Waiting for approval', '#/held', n ? 'Review' : 'Held', [
    bigNumber(n, n === 1 ? 'held email' : 'held emails', n ? 'warn' : 'muted'),
    h('p', { class: 'small muted' }, n ? 'Nothing is sent until a person approves.' : 'Nothing is waiting.'),
  ]);
}

function pausesTile(o) {
  const n = Number(o.active_pauses) || 0;
  return tile('Pauses', '#/pauses', 'Pauses', [
    bigNumber(n, n === 1 ? 'active pause' : 'active pauses', n ? 'bad' : 'muted'),
    h('p', { class: 'small muted' }, n ? 'Sends from a paused key, domain or project are refused.' : 'Every key and domain can send.'),
  ]);
}

function receivingTile(o) {
  const r = o.receiving || {};
  if (!r.enabled) {
    return tile('Receiving', '#/mail', 'Mail', [
      h('p', {}, toneBadge('muted', 'Off')),
      h('p', { class: 'small muted' }, 'This server does not receive mail yet. Set OMW_INBOUND_MX and run an inbound node (README: Receiving mail).'),
    ]);
  }
  return tile('Receiving', '#/mail', 'Mail', [
    bigNumber(r.messages_24h, 'received in 24 h', r.messages_24h ? 'info' : 'muted'),
    h('dl', { class: 'counts' },
      h('div', {}, h('dt', {}, 'Inboxes'), h('dd', {}, L.formatCount(r.inboxes ?? 0))),
      h('div', { class: r.held_messages ? '' : 'zero' }, h('dt', {}, h('span', { class: 'dot tone-warn', 'aria-hidden': 'true' }), 'Held from agents'), h('dd', {}, L.formatCount(r.held_messages ?? 0)))),
    r.inbound_mx ? h('p', { class: 'small muted' }, 'MX ', mono(r.inbound_mx)) : null,
  ]);
}

function domainsTile(o) {
  const all = Array.isArray(o.domains) ? o.domains : [];
  const bad = L.domainsNeedingAttention(all);
  let content;
  if (!all.length) content = emptyState('No domains yet.', link('#/domains', 'Add a domain'));
  else if (!bad.length) content = [bigNumber(all.length, all.length === 1 ? 'domain, healthy' : 'domains, all healthy', 'ok')];
  else {
    content = h('ul', { class: 'attention' }, bad.map((d) => h('li', {},
      h('p', {}, link(route('domains', d.id), d.name), ' ', badge(d.status)),
      h('ul', { class: 'problems' }, d.problems.map((p) => h('li', {}, p))))));
  }
  return tile(bad.length ? `Domains needing attention (${bad.length})` : 'Domains', '#/domains', 'Domains', content);
}

function workersTile(o) {
  const ws = Array.isArray(o.workers) ? o.workers : [];
  if (!ws.length) {
    return tile('Workers', null, '', emptyState('No workers registered. Delivery needs one: openmailwire workers add <name>, then run openmailwire worker.'), 'tile-wide');
  }
  return tile(`Workers (${ws.length})`, null, '', dataTable([
    { label: 'Worker', class: 'col-main', cell: (w) => h('span', { class: 'status-cell' }, h('strong', { class: 'mono' }, w.name), muted([w.role, w.kind].filter(Boolean).join(' · '))) },
    { label: 'IP', cell: (w) => (w.ip ? mono(w.ip) : muted('—')) },
    { label: 'State', class: 'col-status', cell: (w) => { const st = L.workerState(w); return h('span', { class: 'status-cell' }, toneBadge(st.tone, st.label), w.last_seen_at ? h('span', { class: 'small muted' }, 'seen ', when(w.last_seen_at)) : null); } },
    {
      label: 'Self-check',
      cell: (w) => {
        const c = w.self_check;
        if (!c) return muted('—');
        if (c.ok) return toneBadge('ok', c.skipped ? 'Skipped' : 'Passed');
        return h('span', { class: 'status-cell' }, toneBadge('bad', 'Problems'), h('ul', { class: 'problems small' }, (c.problems || []).map((p) => h('li', {}, p))));
      },
    },
    {
      label: 'Warm-up',
      cell: (w) => {
        const share = L.warmupShare(w.warmup);
        return h('span', { class: 'status-cell' }, L.warmupText(w.warmup),
          share == null ? null : svg('svg', { class: 'bar', viewBox: '0 0 100 8', preserveAspectRatio: 'none', 'aria-hidden': 'true', focusable: 'false' },
            svg('rect', { class: 'bar-track', x: 0, y: 0, width: 100, height: 8 }),
            svg('rect', { class: 'bar-deferred', x: 0, y: 0, width: Math.round(share * 100), height: 8 })));
      },
    },
    { label: 'Version', cell: (w) => (w.version ? mono(w.version) : muted('—')) },
  ], ws, 'Workers').el, 'tile-wide');
}

function recentTile(emails) {
  const rows = Array.isArray(emails) ? emails : [];
  return tile('Recent emails', '#/emails', 'All emails', rows.length
    ? dataTable([
      { label: 'Status', class: 'col-status', cell: (e) => badge(L.emailStatus(e)) },
      { label: 'Subject', class: 'col-main', cell: (e) => link(route('emails', e.id), e.subject || '(no subject)') },
      { label: 'To', class: 'col-addr', cell: (e) => mono(L.summarizeAddresses(e.to)) },
      { label: 'Sent', class: 'col-time', cell: (e) => when(e.created_at) },
    ], rows, 'Recent emails').el
    : emptyState('No emails yet.', link('#/compose', 'Compose one'), ' or call POST /emails.'), 'tile-wide');
}

// ---- setup checklist ----

function recordLines(records) {
  const list = (records || []).filter((r) => r.required || r.record === 'DMARC');
  return h('ul', { class: 'mini-records' }, list.map((r) => h('li', { class: r.status === 'verified' ? 'done' : '' },
    h('span', { class: 'mini-head' }, h('strong', {}, r.record), ' ', badge(r.status)),
    h('span', { class: 'mini-row' }, muted(`${r.type} `), copyable(r.name, `${r.record} record name`)),
    h('span', { class: 'mini-row' }, muted('value '), copyable(r.value, `${r.record} record value`)))));
}

function stepDetail(step, st) {
  const domainRoute = step.domain ? route('domains', step.domain.id) : '#/domains';
  switch (step.id) {
    case 'domain':
      return step.done ? muted((st.domains[0] || {}).name || '') : link('#/domains', 'Add a domain');
    case 'dns':
      if (step.done || !step.domain) return step.done ? muted('Every required record is published.') : muted('Add a domain first.');
      return [
        h('p', { class: 'step-actions' }, link(domainRoute, 'Set up automatically with Cloudflare'), muted(' or publish these at your DNS provider:')),
        recordLines(step.domain.records)];
    case 'verify':
      return step.done ? muted('Verified.') : link(domainRoute, 'Verify DNS on the domain page');
    case 'key':
      if (step.unknown) return muted('A full-access key can create keys.');
      return step.done ? muted('An agent has its own key.') : link('#/api-keys', 'Create a sending-only key');
    case 'send':
      return step.done
        ? h('span', {}, muted('Sent. '), st.latest ? link(route('emails', st.latest.id), 'See its timeline') : null)
        : link('#/compose', 'Compose a test email');
    case 'mx':
      if (step.blocked) {
        return muted('This server does not receive mail yet: set OMW_INBOUND_MX and run an inbound node (openmailwire inbound). See "Receiving mail" in the README.');
      }
      if (step.done) return muted('Mail for the domain reaches the inbound node.');
      return step.record
        ? [muted('Publish this MX record on the domain:'), recordLines([{ ...step.record, required: true }])]
        : link(domainRoute, 'See the INBOUND record on the domain page');
    case 'inbox':
      return step.done ? muted(L.addressList((st.inboxes || []).map((i) => i.address)).join(', ')) : link('#/inboxes', 'Create an inbox');
    case 'receive': {
      const to = step.inbox ? step.inbox.address : '<inbox>';
      return step.done ? link('#/mail', 'Open Mail') : h('span', {},
        muted('Send yourself a message from Gmail, or run '), h('code', {}, `openmailwire dev-deliver --to ${to}`),
        muted(' locally, and watch it arrive in '), link('#/mail', 'Mail'), muted('.'));
    }
    default:
      return null;
  }
}

function stepList(steps, st, next, offset) {
  return h('ol', { class: 'checklist-steps' }, steps.map((s, i) => h('li', {
    class: `cl-step${s.done ? ' done' : ''}${s.id === next ? ' current' : ''}${s.blocked ? ' blocked' : ''}`,
    'aria-current': s.id === next ? 'step' : null,
  },
  h('span', { class: 'cl-mark', 'aria-hidden': 'true' }, s.done ? '✓' : String(offset + i + 1)),
  h('div', { class: 'cl-text' },
    h('p', { class: 'cl-title' }, s.title, s.done ? h('span', { class: 'sr-only' }, ' (done)') : null),
    h('div', { class: 'cl-detail' }, stepDetail(s, st))))));
}

function checklist(st, onDismiss) {
  const steps = L.setupSteps(st);
  if (steps.done) return null;
  const pct = steps.total ? Math.round((steps.finished / steps.total) * 100) : 0;
  const dismiss = h('button', { type: 'button', class: 'btn btn-quiet btn-small' }, 'Hide checklist');
  dismiss.addEventListener('click', onDismiss);
  return h('section', { class: 'panel checklist', 'aria-labelledby': 'checklist-title' },
    h('div', { class: 'checklist-head' },
      h('div', {}, h('h2', { class: 'panel-title', id: 'checklist-title' }, 'Get started'),
        h('p', { class: 'small muted' }, `${steps.finished} of ${steps.total} done: send from your domain, then receive mail for your agents.`)),
      dismiss),
    svg('svg', { class: 'bar progress', viewBox: '0 0 100 6', preserveAspectRatio: 'none', 'aria-hidden': 'true', focusable: 'false' },
      svg('rect', { class: 'bar-track', x: 0, y: 0, width: 100, height: 6 }),
      svg('rect', { class: 'bar-accepted', x: 0, y: 0, width: pct, height: 6 })),
    h('div', { class: 'checklist-cols' },
      h('div', {}, h('h3', { class: 'cl-group' }, 'Send'), stepList(steps.send, st, steps.next, 0)),
      h('div', {}, h('h3', { class: 'cl-group' }, 'Receive'), stepList(steps.receive, st, steps.next, steps.send.length))));
}

// checklistState gathers what the checklist needs. Failures count as
// unknown rather than as errors: the checklist is a guide.
async function checklistState(ctx, overview) {
  const quiet = (p) => p.catch((err) => { if (isAbort(err)) throw err; return null; });
  const [domains, keys, emails, inboxes] = await Promise.all([
    quiet(ctx.api('GET', '/domains')),
    quiet(ctx.api('GET', '/api-keys')),
    quiet(ctx.api('GET', L.listPath('/emails', { limit: 1 }))),
    quiet(ctx.api('GET', '/inboxes')),
  ]);
  const boxes = (inboxes && inboxes.data) || [];
  let messageCount = overview && overview.receiving ? Number(overview.receiving.messages_24h) || 0 : 0;
  if (!messageCount && boxes.length) {
    const first = await quiet(ctx.api('GET', L.listPath(`/inboxes/${encodeURIComponent(boxes[0].id)}/messages`, { limit: 1 })));
    messageCount = first && first.data ? first.data.length : 0;
  }
  const emailRows = (emails && emails.data) || [];
  return {
    domains: (domains && domains.data) || [],
    keys: keys ? keys.data || [] : null,
    emailCount: emailRows.length,
    latest: emailRows[0] || null,
    receiving: overview ? overview.receiving : null,
    inboxes: boxes,
    messageCount,
  };
}

// ---- page ----

export async function pageOverview(ctx) {
  const body = h('div', { class: 'overview' });
  const top = h('div');
  const updated = h('span', { class: 'live-note' }, h('span', { class: 'pulse', 'aria-hidden': 'true' }), 'Live');
  const refresh = h('button', { type: 'button', class: 'btn' }, 'Refresh');
  let lastAt = 0;

  const load = async () => {
    let overview = null;
    let failure = null;
    try {
      overview = await ctx.api('GET', '/overview');
    } catch (err) {
      if (isAbort(err)) throw err;
      failure = err;
    }
    const recent = await ctx.api('GET', L.listPath('/emails', { limit: 8 })).catch((err) => { if (isAbort(err)) throw err; return null; });
    const dismissed = getPref('setup.dismissed') === '1';
    const st = dismissed ? null : await checklistState(ctx, overview);
    if (!ctx.live) return;
    lastAt = Date.now();
    const showAgain = h('button', { type: 'button', class: 'btn btn-quiet btn-small' }, 'Show the setup checklist');
    showAgain.addEventListener('click', () => { setPref('setup.dismissed', ''); load(); });
    put(top,
      st ? checklist(st, () => { setPref('setup.dismissed', '1'); toast('Checklist hidden. Show it again at the bottom of this page.'); load(); }) : null);
    const tiles = overview
      ? [sendingTile(overview), queueTile(overview), heldTile(overview), pausesTile(overview),
        domainsTile(overview), receivingTile(overview), workersTile(overview), recentTile(recent && recent.data)]
      : [errorView(failure, 'the overview', 'see'), recentTile(recent && recent.data)];
    put(body, h('div', { class: 'tiles' }, tiles), dismissed ? h('p', { class: 'center' }, showAgain) : null);
  };

  refresh.addEventListener('click', async () => {
    refresh.disabled = true;
    try {
      await load();
      toast('Overview refreshed');
    } catch (err) {
      if (!isAbort(err)) toast('Could not refresh the overview', 'bad');
    } finally {
      refresh.disabled = false;
    }
  });
  put(body, loadingLine('Loading the overview…', 6));
  ctx.mount(pageHead('Overview', h('span', {}, 'The whole system at a glance. ', updated), refresh), top, body);
  await load();
  // Live: refresh while the page is open and visible.
  while (ctx.live) {
    await sleep(2000, ctx.signal);
    if (!ctx.live) return;
    if (document.hidden || Date.now() - lastAt < REFRESH_MS) continue;
    // Never pull the page from under someone using it.
    if (top.contains(document.activeElement) || body.contains(document.activeElement)) continue;
    try {
      await load();
    } catch (err) {
      if (isAbort(err)) return;
      lastAt = Date.now();
    }
  }
}
