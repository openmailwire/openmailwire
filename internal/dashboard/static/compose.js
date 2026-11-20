// OpenMailWire dashboard: Compose. Writes and sends one email with
// POST /emails, after an optional POST /emails/check. The HTML preview is
// shown only inside a sandboxed frame (ui.js previewFrame), never in this
// document.

import * as L from './lib.js';
import {
  h, put, nextID, announce, route, mono, muted, toneBadge, link, panel, pageHead, emptyState, kv,
  notice, errorView, field, input, textarea, select, checkbox, formStatus, fieldError, withBusy,
  randomID, previewFrame, setPreview, toast, heldChanged,
} from './ui.js';

// chipsField is an address field that turns what is typed or pasted into
// removable chips, one address each.
function chipsField(label, name, hint) {
  const id = nextID(name);
  const list = h('ul', { class: 'chip-list', 'aria-label': `${label} addresses` });
  const entry = h('input', {
    class: 'chip-entry', id, type: 'text', name, inputmode: 'email', autocomplete: 'off', spellcheck: 'false',
    autocapitalize: 'off',
  });
  const box = h('div', { class: 'chips-input' }, list, entry);
  let values = [];
  const onChange = [];
  const draw = () => {
    put(list, values.map((v, i) => {
      const ok = L.isAddress(v);
      const remove = h('button', { type: 'button', class: 'chip-x', 'aria-label': `Remove ${v}` }, '×');
      remove.addEventListener('click', () => {
        values.splice(i, 1);
        draw();
        entry.focus();
        onChange.forEach((f) => f());
      });
      return h('li', { class: ok ? 'chip addr' : 'chip addr chip-bad', title: ok ? null : 'Not an email address' },
        h('span', { class: 'mono' }, v), ok ? null : h('span', { class: 'sr-only' }, ' (not an email address)'), remove);
    }));
  };
  const commit = () => {
    const add = L.splitAddresses(entry.value);
    if (!add.length) return;
    for (const a of add) if (!values.some((v) => v.toLowerCase() === a.toLowerCase())) values.push(a);
    entry.value = '';
    draw();
    announce(`${add.length === 1 ? add[0] : `${add.length} addresses`} added to ${label}`);
    onChange.forEach((f) => f());
  };
  entry.addEventListener('keydown', (e) => {
    if ((e.key === 'Enter' || e.key === ',' || e.key === ';' || e.key === ' ') && entry.value.trim()) {
      e.preventDefault();
      commit();
    } else if (e.key === 'Backspace' && !entry.value && values.length) {
      values.pop();
      draw();
      onChange.forEach((f) => f());
    }
  });
  entry.addEventListener('blur', commit);
  entry.addEventListener('paste', () => setTimeout(commit, 0));
  box.addEventListener('click', (e) => { if (e.target === box || e.target === list) entry.focus(); });
  const el = h('div', { class: 'field' }, h('label', { for: id }, label), box, hint ? h('p', { class: 'hint', id: `${id}-hint` }, hint) : null);
  if (hint) entry.setAttribute('aria-describedby', `${id}-hint`);
  return {
    el,
    values: () => { commit(); return [...values]; },
    clear: () => { values = []; entry.value = ''; draw(); },
    onChange: (f) => onChange.push(f),
  };
}

function readFile(file) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(L.base64FromDataURL(r.result));
    r.onerror = () => reject(r.error || new Error('could not read the file'));
    r.readAsDataURL(file);
  });
}

function checkView(r) {
  const v = L.checkVerdict(r);
  const kind = { ok: 'ok', warn: 'warn', bad: 'error' }[v.tone];
  const refusals = Array.isArray(r.refusals) ? r.refusals : [];
  const warnings = Array.isArray(r.warnings) ? r.warnings : [];
  const issue = (code, message) => h('li', {}, h('code', {}, code || '—'), ' ', message || '');
  return h('div', { class: `notice notice-${kind}`, role: 'status' },
    h('p', { class: 'notice-title' }, v.title),
    kv([
      ['Stream', L.STREAM_LABELS[r.stream] || r.stream],
      ['Size', `${L.formatBytes(r.size)} as sent, DKIM signature included (estimated)`],
      ['Held because', (r.hold_reasons || []).length ? h('span', { class: 'chips' }, r.hold_reasons.map((x) => toneBadge('warn', L.holdReasonLabel(x)))) : null],
    ]),
    refusals.length ? [h('p', { class: 'list-title' }, 'Refusals'), h('ul', { class: 'issues' }, refusals.map((x) => issue(x.name, x.message)))] : null,
    warnings.length ? [h('p', { class: 'list-title' }, 'Warnings'), h('ul', { class: 'issues' }, warnings.map((x) => issue(x.code, x.message)))] : null);
}

export async function pageCompose(ctx) {
  const res = await ctx.api('GET', '/domains');
  const verified = ((res && res.data) || []).filter((d) => d.status === 'verified');
  if (!verified.length) {
    ctx.mount(pageHead('Compose', 'Write and send an email from one of your domains.'),
      panel(null, emptyState('You need a verified domain to send from.', link('#/domains', 'Add or verify a domain'))));
    return;
  }

  // From.
  const name = input({ name: 'from_name', placeholder: 'Acme Support', spellcheck: 'true', autocomplete: 'off' });
  const local = input({ name: 'from_local', value: 'hello', class: 'input mono', required: true });
  const domain = select({ name: 'from_domain', 'aria-label': 'Domain' }, verified.map((d) => [d.name, `@${d.name}`]), verified[0].name);
  const fromPreview = h('p', { class: 'hint from-preview', 'aria-live': 'polite' });
  const showFrom = () => {
    try {
      put(fromPreview, 'Sends as ', mono(L.formatFrom({ name: name.value, local: local.value, domain: domain.value })));
    } catch (err) {
      put(fromPreview, h('span', { class: 'field-error' }, err.message));
    }
  };
  const localID = local.id || (local.id = nextID('local'));

  // Recipients.
  const to = chipsField('To', 'to', 'Type or paste addresses; Enter, comma or space adds each.');
  const cc = chipsField('Cc', 'cc');
  const bcc = chipsField('Bcc', 'bcc');
  const replyTo = chipsField('Reply-To', 'reply_to', 'Where answers should go, when not to the From address.');
  const subject = input({ name: 'subject', required: true, spellcheck: 'true', autocomplete: 'off' });

  // Bodies and the live preview.
  const text = textarea({ name: 'text', rows: '9', spellcheck: 'true' });
  const html = textarea({ name: 'html', rows: '9', class: 'input mono' });
  const frame = previewFrame('Preview of the HTML body (sandboxed)');
  const textPreview = h('pre', { class: 'body preview-text' });
  const previewMode = { value: 'html' };
  const modeName = nextID('mode');
  const modeSwitch = h('fieldset', { class: 'segmented' }, h('legend', { class: 'sr-only' }, 'Preview'),
    [['html', 'HTML'], ['text', 'Text']].map(([v, label]) => {
      const r = h('input', { type: 'radio', name: modeName, value: v, id: nextID('pm') });
      r.checked = v === previewMode.value;
      r.addEventListener('change', () => { previewMode.value = v; drawPreview(); });
      return h('span', { class: 'segment' }, r, h('label', { for: r.id }, label));
    }));
  const previewBox = h('div', { class: 'preview-box' });
  let previewTimer;
  const drawPreview = () => {
    if (previewMode.value === 'html' && html.value.trim()) {
      setPreview(frame, html.value);
      put(previewBox, frame);
    } else if (previewMode.value === 'html') {
      put(previewBox, emptyState('No HTML body. Write one to preview it here.'));
    } else {
      textPreview.textContent = text.value || (html.value ? '(No text part: receivers that cannot show HTML see nothing. Add one.)' : '');
      put(previewBox, text.value || html.value ? textPreview : emptyState('No text body yet.'));
    }
  };
  const schedulePreview = () => {
    clearTimeout(previewTimer);
    previewTimer = setTimeout(drawPreview, 250);
  };
  text.addEventListener('input', schedulePreview);
  html.addEventListener('input', schedulePreview);

  // Attachments.
  const files = [];
  const filePick = h('input', { type: 'file', multiple: true, id: nextID('files'), class: 'file-input' });
  const fileList = h('ul', { class: 'attachments' });
  const budget = h('p', { class: 'hint' });
  const drawFiles = () => {
    const b = L.attachmentBudget(files);
    put(fileList, files.map((f, i) => {
      const remove = h('button', { type: 'button', class: 'btn btn-small btn-quiet', 'aria-label': `Remove ${f.filename}` }, 'Remove');
      remove.addEventListener('click', () => { files.splice(i, 1); drawFiles(); });
      return h('li', {}, h('span', { class: 'mono' }, f.filename), muted(` ${f.content_type || 'file'} · ${L.formatBytes(f.size)}`), remove);
    }));
    put(budget, b.problem ? h('span', { class: 'field-error' }, b.problem) : null,
      `${b.count} of ${L.ATTACHMENT_LIMITS.files} files, ${L.formatBytes(b.bytes) || '0 B'} of ${L.formatBytes(L.ATTACHMENT_LIMITS.bytes)} in total.`);
  };
  filePick.addEventListener('change', async () => {
    const picked = [...filePick.files];
    filePick.value = '';
    for (const f of picked) {
      try {
        files.push({ filename: f.name, size: f.size, content_type: f.type || '', content: await readFile(f) });
      } catch (err) {
        toast(`Could not read ${f.name}`, 'bad');
      }
    }
    if (!ctx.live) return;
    drawFiles();
    announce(`${picked.length === 1 ? '1 file' : `${picked.length} files`} attached`);
  });

  // Options.
  const stream = select({ name: 'stream' }, [['transactional', 'Transactional'], ['broadcast', 'Broadcast']], 'transactional');
  const scheduled = input({ name: 'scheduled_at', type: 'datetime-local' });
  const expires = input({ name: 'expires_at', type: 'datetime-local' });
  const retry = checkbox('Retry when the outcome is unknown', {
    name: 'retry_uncertain', checked: true,
    hint: 'If a connection drops after sending, try again (the recipient may get two copies). Turn off for receipts and codes.',
  });

  // Idempotency: a new key whenever the email changes, so a retried send
  // after a network error is never queued twice.
  let idem = randomID();
  const idemLine = h('p', { class: 'hint' });
  const showIdem = () => put(idemLine, 'Idempotency key ', mono(idem), ': a repeated Send with it is not queued twice.');
  const renew = () => { idem = randomID(); showIdem(); };

  const status = formStatus();
  const checkBtn = h('button', { type: 'button', class: 'btn' }, 'Check');
  const sendBtn = h('button', { type: 'submit', class: 'btn btn-primary' }, 'Send');
  const form = h('form', { class: 'form compose', novalidate: true, 'aria-label': 'Compose an email' },
    h('fieldset', { class: 'fieldset' }, h('legend', {}, 'From'),
      h('div', { class: 'from-row' },
        field('Name (optional)', name),
        h('div', { class: 'field' }, h('label', { for: localID }, 'Address'),
          h('div', { class: 'addr-split' }, local, domain))),
      fromPreview),
    h('fieldset', { class: 'fieldset' }, h('legend', {}, 'Recipients'),
      to.el,
      h('details', { class: 'source more-recipients' }, h('summary', {}, 'Cc, Bcc and Reply-To'),
        h('div', { class: 'grid-2' }, cc.el, bcc.el), replyTo.el)),
    field('Subject', subject),
    h('div', { class: 'compose-body' },
      h('div', { class: 'compose-editors' },
        field('Text', text, 'The plain-text part. Every email should have one.'),
        field('HTML (optional)', html, 'Previewed in a sandbox: no scripts, no remote images, no inline styles. Mail clients show inline styles.')),
      h('section', { class: 'compose-preview', 'aria-label': 'Preview' },
        h('div', { class: 'panel-head' }, h('h2', { class: 'panel-title' }, 'Preview'), modeSwitch),
        previewBox)),
    h('fieldset', { class: 'fieldset' }, h('legend', {}, 'Attachments'),
      h('div', { class: 'field' }, h('label', { for: filePick.id }, 'Add files'), filePick), fileList, budget),
    h('details', { class: 'panel-lite' }, h('summary', {}, 'Delivery options'),
      h('div', { class: 'grid-3' },
        field('Stream', stream, 'Broadcast: newsletters and bulk mail, to exactly one recipient, with a one-click unsubscribe.'),
        field('Send at (optional)', scheduled, 'Up to 30 days ahead. Your time zone.'),
        field('Give up at (optional)', expires, 'No attempts after this; at most 7 days after sending.')),
      retry.el),
    idemLine,
    h('div', { class: 'form-actions sticky-actions' }, sendBtn, checkBtn,
      h('p', { class: 'hint' }, 'Check sends nothing: it says what Send would do.')),
    status);

  const values = () => L.buildCompose({
    from: { name: name.value, local: local.value, domain: domain.value },
    to: to.values(), cc: cc.values(), bcc: bcc.values(), reply_to: replyTo.values(),
    subject: subject.value, text: text.value, html: html.value,
    stream: stream.value, scheduledAt: scheduled.value, expiresAt: expires.value,
    retryUncertain: retry.cb.checked, attachments: files,
  });

  for (const el of [name, local, domain]) el.addEventListener('input', showFrom);
  domain.addEventListener('change', showFrom);
  form.addEventListener('input', renew);
  for (const c of [to, cc, bcc, replyTo]) c.onChange(renew);
  filePick.addEventListener('change', renew);

  checkBtn.addEventListener('click', async () => {
    status.replaceChildren();
    let body;
    try {
      body = values();
    } catch (err) {
      status.replaceChildren(fieldError(err.message));
      return;
    }
    await withBusy(form, async () => {
      try {
        const r = await ctx.api('POST', '/emails/check', body);
        if (!ctx.live) return;
        status.replaceChildren(checkView(r));
        announce(L.checkVerdict(r).title);
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'emails') || '');
      }
    });
  });

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    status.replaceChildren();
    let body;
    try {
      body = values();
    } catch (err) {
      status.replaceChildren(fieldError(err.message));
      return;
    }
    await withBusy(form, async () => {
      try {
        const r = await ctx.api('POST', '/emails', body, { idempotencyKey: idem });
        if (r.held) {
          heldChanged();
          toast(`Held for approval (${(r.hold_reasons || []).map(L.holdReasonLabel).join(', ') || 'policy'})`, 'warn');
        } else {
          toast(body.scheduled_at ? 'Scheduled' : 'Queued for delivery');
        }
        location.hash = route('emails', r.id);
      } catch (err) {
        if (ctx.live) status.replaceChildren(errorView(err, 'emails') || '');
      }
    });
  });

  showFrom();
  showIdem();
  drawFiles();
  drawPreview();
  ctx.mount(pageHead('Compose', 'Write an email and send it from one of your verified domains. Check first to see what Send would do.'),
    panel(null, form));
}
