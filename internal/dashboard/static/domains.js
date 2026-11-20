// OpenMailWire dashboard: a domain's DKIM keys (rotation and Ed25519),
// where its aggregate reports go, and what those reports say (v0.6).
// DMARC and TLS reports are written by whoever sends them, so their
// contents are shown as text and marked as such.

import * as L from './lib.js';
import {
  h, put, nextID, announce, mono, muted, badge, toneBadge, when, exact, copyable,
  emptyState, notice, errorView, dataTable, formStatus, select, confirmed, senderTag, toast, loadingLine,
} from './ui.js';

// reportAddressesPanel shows the rua addresses to publish, or null for
// servers that do not receive reports.
export function reportAddressesPanel(d) {
  const a = d && d.report_addresses;
  if (!a || typeof a !== 'object') return null;
  const rows = [];
  if (a.dmarc_record_example) {
    rows.push({ record: 'DMARC', name: `_dmarc.${d.name}`, value: a.dmarc_record_example, note: a.dmarc_rua ? `or add ${a.dmarc_rua} to your record's rua=` : '' });
  }
  if (a.tls_rpt_record && a.tls_rpt_record.name) {
    rows.push({ record: 'TLS-RPT', name: a.tls_rpt_record.name, value: a.tls_rpt_record.value, note: '' });
  }
  if (!rows.length) return null;
  return h('section', { class: 'panel' },
    h('h2', { class: 'panel-title' }, 'Report addresses'),
    h('p', { class: 'hint' }, 'Receivers send daily DMARC aggregate and TLS reports here, and they appear below under DMARC and TLS reports. ',
      'The addresses are on the return-path domain, so its MX record (above) must point to an inbound node.'),
    dataTable([
      { label: 'Record', cell: (r) => h('strong', {}, r.record) },
      { label: 'Type', cell: () => 'TXT' },
      { label: 'Name', class: 'col-name', cell: (r) => copyable(r.name, `${r.record} record name`) },
      { label: 'Value', class: 'col-value', cell: (r) => h('span', { class: 'status-cell' }, copyable(r.value, `${r.record} record value`), r.note ? h('span', { class: 'small muted' }, r.note) : null) },
    ], rows, 'Report records').el);
}

// ---- DKIM ----

function keysTable(keys) {
  return dataTable([
    { label: 'Selector', cell: (k) => h('span', { class: 'status-cell' }, mono(k.selector), k.signs ? toneBadge('ok', 'Signs new mail') : null) },
    { label: 'Algorithm', cell: (k) => (k.algorithm === 'ed25519' ? 'Ed25519' : 'RSA') },
    { label: 'State', class: 'col-status', cell: (k) => badge(k.state) },
    { label: 'Record name', class: 'col-name', cell: (k) => (k.record ? copyable(k.record.name, `${k.selector} record name`) : muted('—')) },
    { label: 'Record value', class: 'col-value', cell: (k) => (k.record ? copyable(k.record.value, `${k.selector} record value`) : muted('—')) },
    {
      label: 'Notes',
      cell: (k) => h('span', { class: 'status-cell' },
        k.retire_at ? h('span', {}, 'Keep until ', exact(k.retire_at)) : null,
        k.record && k.record.detail ? h('span', { class: 'small muted' }, k.record.detail) : null),
    },
  ], keys, 'DKIM keys');
}

function step(n, current, title, text) {
  return h('li', { class: current ? 'step current' : 'step', 'aria-current': current ? 'step' : null },
    h('strong', {}, `${n}. ${title}`), ' ', h('span', { class: 'muted' }, text));
}

function issuesTable(issues) {
  return dataTable([
    { label: 'Scope', cell: (i) => L.statusLabel(i.scope) },
    { label: 'Code', cell: (i) => mono(i.enhanced_code || '—') },
    { label: 'Count', class: 'col-num', cell: (i) => L.formatCount(i.count ?? 0) },
    { label: 'First seen', class: 'col-time', cell: (i) => when(i.first_seen_at) },
    { label: 'Last seen', class: 'col-time', cell: (i) => when(i.last_seen_at) },
    { label: "Receiver's reply", cell: (i) => (i.last_response ? h('span', { class: 'mono small' }, i.last_response) : muted('—')) },
  ], issues, 'Authentication problems');
}

// dkimPanel loads GET /domains/{id}/dkim and offers the rotation and the
// Ed25519 switch.
export function dkimPanel(ctx, d) {
  const base = `/domains/${encodeURIComponent(d.id)}/dkim`;
  const box = h('section', { class: 'panel', 'aria-labelledby': nextID('dkim') });
  const head = h('h2', { class: 'panel-title', id: box.getAttribute('aria-labelledby') }, 'DKIM keys');
  let issues = [];

  const act = async (btn, label, path, body, question, opts) => {
    if (question && !await confirmed(question, opts)) return;
    const status = box.querySelector('.dkim-status');
    btn.disabled = true;
    btn.textContent = label;
    if (status) status.replaceChildren();
    try {
      const res = await ctx.api('POST', path, body);
      if (!ctx.live) return;
      render(res.keys || [], res.message ? notice('ok', 'Done', res.message) : null);
      if (res.message) announce(res.message);
    } catch (err) {
      if (!ctx.live) return;
      const fresh = box.querySelector('.dkim-status');
      if (fresh) fresh.replaceChildren(errorView(err, 'DKIM keys') || '');
      btn.disabled = false;
      btn.textContent = btn.dataset.label;
    }
  };
  const button = (text, cls, onclick) => {
    const b = h('button', { type: 'button', class: `btn ${cls || ''}`.trim(), 'data-label': text }, text);
    b.addEventListener('click', () => onclick(b));
    return b;
  };

  const render = (keys, message) => {
    const st = L.dkimState(keys);
    const rotate = st.rotation === 'pending'
      ? button('Switch signing to the new key', 'btn-primary', (b) => act(b, 'Checking DNS…', `${base}/rotate`, undefined))
      : button('Start a rotation', '', (b) => act(b, 'Creating a key…', `${base}/rotate`, undefined,
        'Mail stays signed with the current key until you publish the new record and switch.', { title: 'Start a DKIM rotation?', confirm: 'Create a new key' }));
    const edButtons = {
      off: [button('Turn on Ed25519', '', (b) => act(b, 'Creating a key…', `${base}/ed25519`, { enabled: true }))],
      pending: [
        button('Start signing with Ed25519', 'btn-primary', (b) => act(b, 'Checking DNS…', `${base}/ed25519`, { enabled: true })),
        button('Cancel', 'btn-quiet', (b) => act(b, 'Removing…', `${base}/ed25519`, { enabled: false })),
      ],
      active: [button('Turn off Ed25519', 'btn-danger', (b) => act(b, 'Turning off…', `${base}/ed25519`, { enabled: false },
        'New mail is signed with RSA only; you can delete its TXT record afterwards.', { title: 'Turn off Ed25519?', confirm: 'Turn off', danger: true }))],
    }[st.ed25519];
    const edState = {
      off: toneBadge('muted', 'Off'),
      pending: toneBadge('warn', 'Waiting for its DNS record'),
      active: toneBadge('ok', 'On'),
    }[st.ed25519];
    put(box, head,
      message,
      h('div', { class: 'dkim-status form-status', 'aria-live': 'polite' }),
      h('p', { class: 'hint' }, 'Receivers check each signature against the TXT record at its selector. Every listed record must stay published.'),
      keys.length ? keysTable(keys).el : emptyState('No keys.'),
      h('div', { class: 'subsection' },
        h('h3', {}, 'Rotate the RSA key'),
        h('ol', { class: 'steps' },
          step(1, st.rotation === 'idle', 'Start.', 'A new key is made under a new selector; publish its TXT record. Mail is still signed with the current key.'),
          step(2, st.rotation === 'pending', 'Switch.', 'Once the record is published, signing moves to the new key. The previous key stays listed for 8 days, because mail queued before the switch carries its signature; delete its record after that.')),
        st.next ? h('p', { class: 'hint' }, 'Publish ', mono(st.next.record.name), ' (TXT, value in the table above), wait for DNS, then switch.') : null,
        h('div', { class: 'form-actions' }, rotate)),
      h('div', { class: 'subsection' },
        h('h3', {}, 'Ed25519 signature ', edState),
        h('p', { class: 'hint' }, 'An optional second signature next to the RSA one (RFC 8463). Receivers that do not support Ed25519 use the RSA signature. Turning it on also takes two steps: create the key and publish its record, then start signing.'),
        h('div', { class: 'form-actions' }, edButtons)),
      issues.length
        ? h('div', { class: 'subsection' },
          h('h3', {}, 'Authentication problems receivers reported'),
          h('p', { class: 'hint' }, 'SPF, DKIM or DMARC did not pass for mail from this domain (Gmail 4.7.26 to 4.7.32 and similar). The reply text is the receiver\'s.'),
          issuesTable(issues).el)
        : null);
  };

  fill();
  async function fill() {
    put(box, head, h('p', { class: 'loading', role: 'status' }, 'Loading…'));
    try {
      const res = await ctx.api('GET', base);
      if (!ctx.live) return;
      issues = Array.isArray(res.auth_issues) ? res.auth_issues : [];
      render(res.keys || []);
    } catch (err) {
      if (ctx.live) put(box, head, errorView(err, 'DKIM keys'));
    }
  }
  return box;
}

// ---- DMARC and TLS reports ----

function rateText(pass, total) {
  return total ? L.formatRate(pass / total) : '—';
}

function listText(list) {
  const l = Array.isArray(list) ? list.filter(Boolean) : [];
  return l.length ? l.join(', ') : '—';
}

function reportBody(rep) {
  const dm = rep.dmarc || {};
  const tls = rep.tls || {};
  const reports = Array.isArray(rep.reports) ? rep.reports : [];
  const sources = Array.isArray(dm.by_source) ? dm.by_source : [];
  const unknown = Array.isArray(dm.unknown_senders) ? dm.unknown_senders : [];
  const orgs = Array.isArray(dm.by_organization) ? dm.by_organization : [];
  const tlsOrgs = Array.isArray(tls.by_organization) ? tls.by_organization : [];
  const failures = Array.isArray(tls.failures) ? tls.failures : [];
  if (!reports.length && !sources.length && !tlsOrgs.length) {
    return emptyState(`No reports in the last ${rep.days} days. They arrive about daily once the records under Report addresses are published.`);
  }
  const tile = (label, value, sub) => h('div', { class: 'stat' }, h('dt', {}, label), h('dd', {}, h('span', { class: 'stat-value' }, value), sub ? muted(sub) : null));
  const sourceTable = (rows, caption) => dataTable([
    { label: 'Source IP', class: 'col-main', cell: (s) => h('span', { class: 'status-cell' }, mono(s.source_ip), s.openmailwire_worker ? toneBadge('ok', 'OpenMailWire worker') : null) },
    { label: 'Messages', class: 'col-num', cell: (s) => L.formatCount(s.messages ?? 0) },
    { label: 'DMARC pass', class: 'col-num', cell: (s) => L.formatRate(s.pass_rate) },
    { label: 'DKIM pass', class: 'col-num', cell: (s) => rateText(s.dkim_pass, s.messages) },
    { label: 'SPF pass', class: 'col-num', cell: (s) => rateText(s.spf_pass, s.messages) },
    { label: 'Header From', cell: (s) => h('span', { class: 'mono small' }, listText(s.header_from)) },
    { label: 'Reported by', cell: (s) => listText(s.organizations) },
  ], rows, caption).el;
  return [
    h('p', { class: 'legend-note' }, senderTag('reporter-provided'), ' ',
      'Reports come from mailbox providers, but anyone can send one: organization names, hosts and texts in them are shown as text and prove nothing.'),
    h('dl', { class: 'stats' },
      tile('DMARC messages', L.formatCount(dm.messages ?? 0)),
      tile('DMARC pass', L.formatRate(dm.pass_rate), `${L.formatCount(dm.fail ?? 0)} failed`),
      tile('TLS sessions', L.formatCount((tls.successful_sessions ?? 0) + (tls.failed_sessions ?? 0))),
      tile('TLS failures', L.formatCount(tls.failed_sessions ?? 0))),
    unknown.length
      ? h('div', { class: 'subsection' },
        notice('warn', `${unknown.length === 1 ? 'A source' : `${unknown.length} sources`} sent mail as this domain without passing DMARC`,
          'They are not OpenMailWire workers: a spoofer, or a service of yours that still needs SPF or DKIM for this domain.'),
        sourceTable(unknown, 'Unknown senders'))
      : null,
    sources.length ? h('div', { class: 'subsection' }, h('h3', {}, 'DMARC by source'), sourceTable(sources, 'DMARC by source')) : null,
    orgs.length ? h('div', { class: 'subsection' }, h('h3', {}, 'DMARC by reporter'), dataTable([
      { label: 'Organization', class: 'col-main', cell: (o) => o.organization || '—' },
      { label: 'Reports', class: 'col-num', cell: (o) => L.formatCount(o.reports ?? 0) },
      { label: 'Messages', class: 'col-num', cell: (o) => L.formatCount(o.messages ?? 0) },
      { label: 'Pass', class: 'col-num', cell: (o) => L.formatRate(o.pass_rate) },
    ], orgs, 'DMARC by reporter').el) : null,
    tlsOrgs.length ? h('div', { class: 'subsection' }, h('h3', {}, 'TLS by reporter'), dataTable([
      { label: 'Organization', class: 'col-main', cell: (o) => o.organization || '—' },
      { label: 'Policy', cell: (o) => [o.policy_type, o.policy_domain].filter(Boolean).join(' · ') || '—' },
      { label: 'Successful', class: 'col-num', cell: (o) => L.formatCount(o.successful_sessions ?? 0) },
      { label: 'Failed', class: 'col-num', cell: (o) => L.formatCount(o.failed_sessions ?? 0) },
    ], tlsOrgs, 'TLS by reporter').el) : null,
    failures.length ? h('div', { class: 'subsection' }, h('h3', {}, 'TLS failures'), dataTable([
      { label: 'Result', cell: (f) => mono(f.result_type || '—') },
      { label: 'Reporter', cell: (f) => f.organization || '—' },
      { label: 'Sending MTA', cell: (f) => mono(f.sending_mta_ip || '—') },
      { label: 'Receiving MX', cell: (f) => mono(f.receiving_mx_hostname || f.receiving_ip || '—') },
      { label: 'Sessions', class: 'col-num', cell: (f) => L.formatCount(f.failed_sessions ?? 0) },
      { label: 'Details', cell: (f) => (f.additional_information || f.failure_reason_code ? h('span', { class: 'small' }, [f.failure_reason_code, f.additional_information].filter(Boolean).join(' · ')) : muted('—')) },
    ], failures, 'TLS failures').el) : null,
    reports.length ? h('details', { class: 'subsection source' },
      h('summary', {}, `The ${reports.length} most recent reports`),
      dataTable([
        { label: 'Kind', cell: (r) => (r.kind === 'tls' ? 'TLS' : String(r.kind || '').toUpperCase() || '—') },
        { label: 'Reporter', cell: (r) => r.organization || '—' },
        { label: 'Report ID', cell: (r) => h('span', { class: 'mono small' }, r.report_id || '—') },
        { label: 'Period', cell: (r) => h('span', {}, exact(r.begin), muted(' to '), exact(r.end)) },
        { label: 'Messages', class: 'col-num', cell: (r) => L.formatCount(r.messages ?? 0) },
        { label: 'Failed', class: 'col-num', cell: (r) => L.formatCount(r.failed ?? 0) },
        { label: 'Received', class: 'col-time', cell: (r) => when(r.received_at) },
      ], reports, 'Recent reports').el) : null,
  ];
}

// reportsPanel loads GET /domains/{id}/reports for a chosen number of days.
export function reportsPanel(ctx, d) {
  const box = h('section', { class: 'panel' });
  const days = select({ name: 'days', 'aria-label': 'Period' },
    [[7, 'Last 7 days'], [30, 'Last 30 days'], [90, 'Last 90 days'], [180, 'Last 180 days']], 30);
  const head = h('div', { class: 'panel-head' }, h('h2', { class: 'panel-title' }, 'DMARC and TLS reports'), days);
  const body = h('div');
  const status = formStatus();
  const load = async () => {
    put(body, h('p', { class: 'loading', role: 'status' }, 'Loading…'));
    try {
      const rep = await ctx.api('GET', L.listPath(`/domains/${encodeURIComponent(d.id)}/reports`, { days: days.value }));
      if (ctx.live) put(body, reportBody(rep));
    } catch (err) {
      if (ctx.live) put(body, errorView(err, 'DMARC reports', 'see'));
    }
  };
  days.addEventListener('change', load);
  put(box, head, status, body);
  load();
  return box;
}

// ---- automatic DNS setup (Cloudflare) ----

function changesTable(changes) {
  return dataTable([
    { label: 'Action', class: 'col-status', cell: (c) => { const [tone, label] = L.dnsActionBadge(c.action); return toneBadge(tone, label); } },
    { label: 'Record', cell: (c) => h('strong', {}, c.record || '—') },
    { label: 'Type', cell: (c) => c.type || '—' },
    { label: 'Name', class: 'col-name', cell: (c) => mono(c.name || '—') },
    { label: 'Value', class: 'col-value', cell: (c) => h('span', { class: 'mono small' }, c.value || '—') },
    { label: 'Notes', cell: (c) => (c.detail ? h('span', { class: 'small' }, c.detail) : muted('—')) },
  ], changes, 'DNS changes');
}

function warningsList(warnings) {
  const list = Array.isArray(warnings) ? warnings.filter(Boolean) : [];
  if (!list.length) return null;
  return notice('warn', list.length === 1 ? 'Read this before applying' : `Read these ${list.length} warnings before applying`, null,
    h('ul', { class: 'issues' }, list.map((w) => h('li', {}, typeof w === 'string' ? w : (w.message || JSON.stringify(w))))));
}

// cloudflarePanel writes the domain's DNS records through the Cloudflare
// API. The token is typed into a password field and sent only in the body
// of POST /domains/{id}/dns/auto: never stored in this browser, never put
// in a URL, and cleared from the field once used. onDomain receives the
// domain as the server returns it after the run.
export function cloudflarePanel(ctx, d, onDomain) {
  const path = `/domains/${encodeURIComponent(d.id)}/dns/auto`;
  const summaryState = h('span');
  const body = h('div', { class: 'cf-body' });
  const box = h('details', { class: 'panel collapsible panel-accent', open: d.status !== 'verified' },
    h('summary', {}, h('span', { class: 'panel-title' }, 'Set up DNS with Cloudflare'), summaryState),
    body);

  const tokenInput = h('input', {
    class: 'input mono', type: 'password', name: 'cloudflare-token', autocomplete: 'off', spellcheck: 'false',
    autocapitalize: 'off', id: nextID('cf-token'), 'data-secret': 'true',
  });
  const zoneInput = h('input', { class: 'input mono', type: 'text', name: 'zone_id', autocomplete: 'off', spellcheck: 'false', id: nextID('cf-zone'), placeholder: 'Found from the domain name' });
  const remember = h('input', { type: 'checkbox', id: nextID('cf-remember'), name: 'remember' });
  const status = formStatus();
  const result = h('div');
  const forget = () => { tokenInput.value = ''; };

  // run sends one request. With remembered, the server uses the token it
  // keeps, and none is sent.
  const run = async (dryRun, remembered) => {
    const req = { provider: 'cloudflare', dry_run: dryRun, remember: remembered || remember.checked };
    if (!remembered) {
      const apiToken = tokenInput.value.trim();
      if (!apiToken) throw Object.assign(new Error('Paste the Cloudflare API token first.'), { local: true });
      req.api_token = apiToken;
    }
    const zone = zoneInput.value.trim();
    if (zone) req.zone_id = zone;
    return ctx.api('POST', path, req);
  };

  const showPreview = (res, remembered) => {
    const changes = Array.isArray(res.changes) ? res.changes : [];
    const pending = changes.some((c) => c.action === 'create' || c.action === 'update');
    const apply = h('button', { type: 'button', class: 'btn btn-primary' }, pending ? 'Apply these changes' : 'Apply');
    const cancel = h('button', { type: 'button', class: 'btn btn-quiet' }, 'Cancel');
    cancel.addEventListener('click', () => { forget(); result.replaceChildren(); announce('Canceled'); });
    apply.addEventListener('click', async () => {
      apply.disabled = true;
      apply.textContent = 'Writing records…';
      status.replaceChildren();
      try {
        const done = await run(false, remembered);
        forget();
        if (!ctx.live) return;
        const verified = done.domain && done.domain.status === 'verified';
        result.replaceChildren(
          notice(verified ? 'ok' : 'calm', verified ? 'Records written, and the domain is verified' : 'Records written',
            verified ? 'This domain can send.' : 'DNS changes can take a few minutes to be seen. Verify again shortly.'),
          warningsList(done.warnings),
          changesTable(Array.isArray(done.changes) ? done.changes : []).el);
        toast(verified ? 'DNS set up: the domain is verified' : 'DNS records written');
        if (done.domain && onDomain) onDomain(done.domain);
        load();
      } catch (err) {
        forget();
        if (!ctx.live) return;
        status.replaceChildren(errorView(err, 'automatic DNS setup') || '');
        apply.disabled = false;
        apply.textContent = 'Apply';
      }
    });
    result.replaceChildren(
      h('p', { class: 'cf-zone' }, 'Zone ', h('strong', { class: 'mono' }, (res.zone && res.zone.name) || '—'), ': ', L.dnsSummary(changes), '. Nothing has changed yet.'),
      warningsList(res.warnings),
      changes.length ? changesTable(changes).el : emptyState('No records to write.'),
      h('div', { class: 'form-actions' }, apply, cancel));
    apply.focus();
  };

  const preview = async (btn, remembered) => {
    btn.disabled = true;
    const label = btn.textContent;
    btn.textContent = 'Asking Cloudflare…';
    status.replaceChildren();
    result.replaceChildren();
    try {
      const res = await run(true, remembered);
      if (ctx.live) showPreview(res, remembered);
    } catch (err) {
      if (!ctx.live) return;
      status.replaceChildren(err.local ? notice('error', err.message, null) : errorView(err, 'automatic DNS setup') || '');
      if (!remembered && !err.local) forget();
    } finally {
      btn.disabled = false;
      btn.textContent = label;
    }
  };

  const tokenForm = () => {
    const previewBtn = h('button', { type: 'submit', class: 'btn btn-primary' }, 'Preview changes');
    const form = h('form', { class: 'form', novalidate: true, autocomplete: 'off' },
      h('ol', { class: 'steps' },
        h('li', { class: 'step' }, h('strong', {}, '1. Create a token. '), muted('In Cloudflare, open My Profile, then API Tokens, then Create Token, and pick the "Edit zone DNS" template.')),
        h('li', { class: 'step' }, h('strong', {}, '2. Limit it to this zone. '), muted(`Under Zone Resources, include only the zone that holds ${d.name}.`)),
        h('li', { class: 'step' }, h('strong', {}, '3. Paste it here and preview. '), muted('You see every change before anything is written.'))),
      h('div', { class: 'field' },
        h('label', { for: tokenInput.id }, 'Cloudflare API token'), tokenInput,
        h('p', { class: 'hint' }, 'Sent once to your control plane over this connection. This browser does not keep it.')),
      h('details', { class: 'source' }, h('summary', {}, 'Zone ID (optional)'),
        h('div', { class: 'field' }, h('label', { for: zoneInput.id, class: 'sr-only' }, 'Zone ID'), zoneInput,
          h('p', { class: 'hint' }, 'Only when the token can see several zones that could hold this domain.'))),
      h('div', { class: 'check' }, remember,
        h('label', { for: remember.id }, h('strong', {}, 'Remember this token for automatic DKIM rotation'),
          muted(' Stored encrypted on the server. Lets agents and rotations run DNS setup without ever seeing the token. You can disconnect at any time.'))),
      h('div', { class: 'form-actions' }, previewBtn));
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      preview(previewBtn, false);
    });
    return form;
  };

  async function load() {
    put(body, loadingLine('Checking the Cloudflare connection…', 2));
    let state;
    try {
      state = await ctx.api('GET', path);
    } catch (err) {
      if (!ctx.live) return;
      put(summaryState, null);
      put(body, errorView(err, 'automatic DNS setup'));
      return;
    }
    if (!ctx.live) return;
    if (state && state.remembered) {
      put(summaryState, ' ', toneBadge('ok', 'Connected to Cloudflare'));
      const again = h('button', { type: 'button', class: 'btn btn-primary' }, 'Run again');
      again.addEventListener('click', () => preview(again, true));
      const disconnect = h('button', { type: 'button', class: 'btn btn-danger' }, 'Disconnect');
      disconnect.addEventListener('click', async () => {
        if (!await confirmed('The server deletes the token it keeps. Automatic DKIM rotation then needs you to publish records by hand, or to connect again.', { title: 'Disconnect Cloudflare?', confirm: 'Disconnect', danger: true })) return;
        disconnect.disabled = true;
        try {
          await ctx.api('DELETE', path);
          if (!ctx.live) return;
          toast('Disconnected from Cloudflare');
          load();
        } catch (err) {
          if (ctx.live) status.replaceChildren(errorView(err, 'automatic DNS setup') || '');
          disconnect.disabled = false;
        }
      });
      const last = state.last_run;
      put(body,
        h('p', {}, 'The server keeps an encrypted Cloudflare token for this domain and uses it to publish DKIM rotations.'),
        last ? h('p', { class: 'hint' }, 'Last run ', when(last.at || last.ran_at || last.created_at),
          last.zone && last.zone.name ? [' on zone ', mono(last.zone.name)] : null,
          Array.isArray(last.changes) ? `: ${L.dnsSummary(last.changes)}.` : '.') : null,
        h('div', { class: 'form-actions' }, again, disconnect),
        status, result);
      return;
    }
    put(summaryState, null);
    put(body, tokenForm(), status, result);
  }
  load();
  return box;
}
