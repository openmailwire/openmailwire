// OpenMailWire dashboard: Mail, every inbox this key may read in one
// list, newest first. Messages open in the thread view (app.js
// pageMessage). Everything a sender wrote is shown as text and marked.

import * as L from './lib.js';
import {
  h, put, announce, link, route, mono, muted, toneBadge, when, panel, pageHead,
  emptyState, errorView, dataTable, input, select, checkbox, formStatus, confirmed, toast,
  senderTag, untrusted, getPref, setPref, sleep, isAbort, loadingLine,
} from './ui.js';

const PER_INBOX = 25;
const REFRESH_MS = 20000;
const SEEN_PREF = 'mail.seen';

// receivingHelp explains how to start receiving mail.
export function receivingHelp(inbox) {
  return h('div', { class: 'empty empty-rich' },
    h('p', {}, inbox
      ? `No mail in ${inbox} yet.`
      : 'No inboxes yet. Receiving takes three things:'),
    h('ol', { class: 'how' },
      h('li', {}, 'An inbound node: run ', h('code', {}, 'openmailwire inbound'), ' on a host that accepts port 25, with ', h('code', {}, 'OMW_INBOUND_MX'), ' set on the control plane.'),
      h('li', {}, 'The domain\'s INBOUND MX record, shown on its ', link('#/domains', 'domain page'), ', pointing at that node.'),
      h('li', {}, 'An inbox on the domain: ', link('#/inboxes', 'create one'), '.')),
    h('p', {}, 'Then send yourself a message from Gmail, or locally run ', h('code', {}, `openmailwire dev-deliver --to ${inbox || '<inbox>'}`),
      '. The README section "Receiving mail" has the details.'));
}

function trustCell(trust) {
  const t = L.trustBadge(trust);
  return toneBadge(t.tone, t.label, t.detail);
}

// fetchNewCount counts messages received since the last visit to Mail,
// for the nav badge. It reads the first page of at most five inboxes.
export async function fetchNewCount(request) {
  const since = Number(getPref(SEEN_PREF)) || 0;
  if (!since) return 0;
  const boxes = ((await request('GET', '/inboxes')) || {}).data || [];
  const pages = await Promise.all(boxes.slice(0, 5).map((b) => request('GET', L.listPath(`/inboxes/${encodeURIComponent(b.id)}/messages`, { limit: PER_INBOX }))
    .then((p) => ({ ...p, inbox_id: b.id })).catch(() => null)));
  return L.countNew(L.mergeMessages(pages.filter(Boolean)), since);
}

export async function pageMail(ctx, { onSeen } = {}) {
  const inboxes = ((await ctx.api('GET', '/inboxes')) || {}).data || [];
  const head = pageHead('Mail', 'Every inbox this key may read, newest first.');
  if (!inboxes.length) {
    ctx.mount(head, panel(null, receivingHelp('')));
    return;
  }
  const byID = new Map(inboxes.map((b) => [b.id, b]));
  const cursors = new Map(inboxes.map((b) => [b.id, { next: null, more: false }]));
  let rows = [];
  const previousVisit = Number(getPref(SEEN_PREF)) || 0;
  setPref(SEEN_PREF, String(Date.now()));
  if (onSeen) onSeen();

  const fetchFirst = async () => {
    const pages = await Promise.all(inboxes.map(async (b) => {
      const p = await ctx.api('GET', L.listPath(`/inboxes/${encodeURIComponent(b.id)}/messages`, { limit: PER_INBOX }));
      return { ...p, inbox_id: b.id, inbox: b.address };
    }));
    for (const p of pages) cursors.set(p.inbox_id, { next: p.next_before, more: Boolean(p.has_more && p.next_before) });
    return L.mergeMessages([{ data: rows }, ...pages]);
  };
  const fetchMore = async () => {
    const pages = await Promise.all(inboxes.filter((b) => cursors.get(b.id).more).map(async (b) => {
      const p = await ctx.api('GET', L.listPath(`/inboxes/${encodeURIComponent(b.id)}/messages`, { limit: PER_INBOX, before: cursors.get(b.id).next }));
      cursors.set(b.id, { next: p.next_before, more: Boolean(p.has_more && p.next_before) });
      return { ...p, inbox_id: b.id, inbox: b.address };
    }));
    rows = L.mergeMessages([{ data: rows }, ...pages]);
  };

  // Filters.
  const q = input({ name: 'q', type: 'search', placeholder: 'Search sender, subject, text', 'aria-label': 'Search mail', 'data-search': 'true' });
  const inboxSel = select({ name: 'inbox', 'aria-label': 'Inbox' }, [['', 'All inboxes'], ...inboxes.map((b) => [b.id, b.address])], '');
  const trustSel = select({ name: 'trust', 'aria-label': 'Sender' }, [['', 'Any sender'], ['authenticated', 'Authenticated'], ['unauthenticated', 'Unauthenticated'], ['failed', 'Failed authentication']], '');
  const heldCb = checkbox('Held only', { name: 'held' });
  const suspCb = checkbox('Suspicious only', { name: 'suspicious' });
  const filterBar = h('div', { class: 'filter-bar', role: 'search' }, q, inboxSel, trustSel, heldCb.el, suspCb.el);

  const list = h('div');
  const status = formStatus();
  const summary = h('p', { class: 'mail-summary', 'aria-live': 'polite' });
  const more = h('button', { type: 'button', class: 'btn' }, 'Load more');
  more.addEventListener('click', async () => {
    more.disabled = true;
    more.textContent = 'Loading…';
    try {
      await fetchMore();
      if (ctx.live) draw();
    } catch (err) {
      if (ctx.live) status.replaceChildren(errorView(err, 'inboxes') || '');
    } finally {
      more.disabled = false;
      more.textContent = 'Load more';
    }
  });

  const releaseButton = (m) => {
    const btn = h('button', { type: 'button', class: 'btn btn-small', 'aria-label': `Release the message from ${m.from || 'unknown sender'}` }, 'Release');
    btn.addEventListener('click', async () => {
      if (!await confirmed('Agent keys that may read this inbox will see it, including any instructions written in it.', { title: 'Release this message?', confirm: 'Release' })) return;
      btn.disabled = true;
      try {
        await ctx.api('POST', `/messages/${encodeURIComponent(m.id)}/release`);
        if (!ctx.live) return;
        rows = rows.map((r) => (r.id === m.id ? { ...r, held: false } : r));
        toast('Message released');
        draw();
      } catch (err) {
        btn.disabled = false;
        if (ctx.live) status.replaceChildren(errorView(err, 'inboxes') || '');
      }
    });
    return btn;
  };

  const columns = [
    { label: 'Sender', class: 'col-status', cell: (m) => trustCell(m.trust) },
    {
      label: 'From',
      class: 'col-addr',
      cell: (m) => h('span', { class: 'status-cell' }, untrusted(mono(m.from || '—')),
        inboxes.length > 1 ? h('span', { class: 'small muted' }, 'to ', (byID.get(m.inbox_id) || {}).address || '') : null),
    },
    {
      label: 'Subject',
      class: 'col-main',
      cell: (m) => h('span', { class: 'subject-cell' },
        h('span', { class: 'subject-line' },
          new Date(m.received_at).getTime() > previousVisit && previousVisit ? toneBadge('info', 'New') : null, ' ',
          untrusted(link(route('messages', m.id), m.subject || '(no subject)')), ' ',
          m.held ? toneBadge('info', 'Held') : null,
          m.suspicious_instructions ? toneBadge('bad', 'Suspicious instructions', 'The text matches a prompt-injection rule.') : null,
          m.in_reply_to_email_id ? muted(' reply to your email') : null),
        m.snippet ? h('span', { class: 'snippet' }, untrusted(L.truncate(m.snippet, 160))) : null),
    },
    { label: 'Received', class: 'col-time', cell: (m) => when(m.received_at) },
    { label: 'Actions', class: 'col-actions', cell: (m) => (m.held ? releaseButton(m) : null) },
  ];

  const filters = () => ({ q: q.value, inbox: inboxSel.value, trust: trustSel.value, held: heldCb.cb.checked, suspicious: suspCb.cb.checked });
  const draw = () => {
    const shown = L.filterMessages(rows, filters());
    const fresh = L.countNew(rows, previousVisit);
    const anyMore = [...cursors.values()].some((c) => c.more);
    put(summary,
      `${L.formatCount(shown.length)} of ${L.formatCount(rows.length)} loaded`,
      fresh ? [' · ', h('strong', {}, `${fresh} new since your last visit`)] : null,
      rows.some((m) => m.held) ? [' · ', `${rows.filter((m) => m.held).length} held`] : null);
    if (!rows.length) {
      const only = inboxes.length === 1 ? inboxes[0].address : '';
      put(list, receivingHelp(only || inboxes.map((b) => b.address).slice(0, 3).join(', ')));
    } else if (!shown.length) {
      put(list, emptyState('No loaded message matches these filters.', anyMore ? 'Load more to search further.' : null));
    } else {
      put(list, dataTable(columns, shown, 'Messages').el);
    }
    put(moreBox, anyMore ? more : null);
  };
  const moreBox = h('div', { class: 'pager' });
  for (const el of [q, inboxSel, trustSel, heldCb.cb, suspCb.cb]) el.addEventListener('input', draw);

  put(list, loadingLine('Loading mail…', 5));
  ctx.mount(
    pageHead('Mail', 'Every inbox this key may read, newest first. Open a message for its thread, to reply or release it.',
      link('#/inboxes', 'Inboxes')),
    panel(null,
      filterBar,
      h('p', { class: 'legend-note' }, senderTag(), ' From, subject and snippet are written by the sender and shown as text only. The sender badge is what SPF, DKIM and DMARC prove.'),
      summary, status, list, moreBox));
  rows = await fetchFirst();
  if (!ctx.live) return;
  draw();

  // Live: new mail appears while the page is open and visible.
  let last = Date.now();
  while (ctx.live) {
    await sleep(2000, ctx.signal);
    if (!ctx.live) return;
    if (document.hidden || Date.now() - last < REFRESH_MS) continue;
    last = Date.now();
    try {
      const before = new Set(rows.map((m) => m.id));
      const next = await fetchFirst();
      if (!ctx.live) return;
      const added = next.filter((m) => !before.has(m.id)).length;
      rows = next;
      setPref(SEEN_PREF, String(Date.now()));
      if (added) {
        draw();
        announce(`${added} new ${added === 1 ? 'message' : 'messages'}`);
        toast(`${added} new ${added === 1 ? 'message' : 'messages'}`);
      }
    } catch (err) {
      if (isAbort(err)) return;
    }
  }
}
