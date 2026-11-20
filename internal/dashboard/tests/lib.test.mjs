// Tests for the dashboard's pure helpers. Run: node --test internal/dashboard/tests/
import test from 'node:test';
import assert from 'node:assert/strict';

import * as L from '../static/lib.js';

test('statusTone maps statuses to colour families', () => {
  assert.equal(L.statusTone('delivered'), 'ok');
  assert.equal(L.statusTone('verified'), 'ok');
  assert.equal(L.statusTone('queued'), 'info');
  assert.equal(L.statusTone('sending'), 'info');
  assert.equal(L.statusTone('pending'), 'info');
  assert.equal(L.statusTone('deferred'), 'warn');
  assert.equal(L.statusTone('unknown'), 'warn');
  assert.equal(L.statusTone('missing'), 'warn');
  assert.equal(L.statusTone('bounced'), 'bad');
  assert.equal(L.statusTone('failed'), 'bad');
  assert.equal(L.statusTone('expired'), 'bad');
  assert.equal(L.statusTone('suppressed'), 'bad');
  assert.equal(L.statusTone('canceled'), 'muted');
  assert.equal(L.statusTone('DELIVERED'), 'ok');
  assert.equal(L.statusTone('something_new'), 'muted');
  assert.equal(L.statusTone(undefined), 'muted');
});

test('statusLabel reads like words', () => {
  assert.equal(L.statusLabel('delivered'), 'Delivered');
  assert.equal(L.statusLabel('unknown_retry'), 'Unknown, will retry');
  assert.equal(L.statusLabel('lease_expired'), 'Worker lost');
  assert.equal(L.statusLabel('late_report_bounced'), 'Late report: bounced');
  assert.equal(L.statusLabel('email.delivery_delayed'), 'Delivery delayed');
  assert.equal(L.statusLabel('sending_access'), 'Sending only');
  assert.equal(L.statusLabel('full_access'), 'Full access');
  assert.equal(L.statusLabel(''), '—');
});

test('isFinal and isPending', () => {
  for (const s of ['delivered', 'bounced', 'expired', 'suppressed', 'unknown', 'canceled']) assert.ok(L.isFinal(s), s);
  for (const s of ['queued', 'sending', 'deferred']) assert.ok(!L.isFinal(s), s);
  assert.ok(L.isPending({ recipients: [{ status: 'delivered' }, { status: 'deferred' }] }));
  assert.ok(!L.isPending({ recipients: [{ status: 'delivered' }, { status: 'bounced' }] }));
  assert.ok(!L.isPending({}));
});

test('formatDateTime', () => {
  assert.equal(L.formatDateTime('2026-11-18T14:03:22.123Z', 'UTC'), '2026-11-18 14:03:22');
  assert.equal(L.formatDateTime('2026-01-02T00:00:05Z', 'UTC'), '2026-01-02 00:00:05');
  assert.equal(L.formatDateTime('2026-11-18T23:30:00Z', 'Asia/Tehran'), '2026-11-19 03:00:00');
  assert.equal(L.formatDateTime(null), '—');
  assert.equal(L.formatDateTime('not a date'), 'not a date');
});

test('relativeTime', () => {
  const now = Date.parse('2026-11-18T12:00:00Z');
  assert.equal(L.relativeTime('2026-11-18T12:00:00Z', now), 'just now');
  assert.equal(L.relativeTime('2026-11-18T11:59:30Z', now), '30 s ago');
  assert.equal(L.relativeTime('2026-11-18T11:55:00Z', now), '5 min ago');
  assert.equal(L.relativeTime('2026-11-18T09:00:00Z', now), '3 h ago');
  assert.equal(L.relativeTime('2026-11-15T12:00:00Z', now), '3 d ago');
  assert.equal(L.relativeTime('2026-11-18T12:04:00Z', now), 'in 4 min');
  assert.equal(L.relativeTime('2026-11-18T12:00:02Z', now), 'in a few seconds');
  assert.equal(L.relativeTime(''), '—');
});

test('formatDuration', () => {
  assert.equal(L.formatDuration(850), '850 ms');
  assert.equal(L.formatDuration(4200), '4.2 s');
  assert.equal(L.formatDuration(185000), '3 min 5 s');
  assert.equal(L.formatDuration(120000), '2 min');
  assert.equal(L.formatDuration(-1), '');
  assert.equal(L.formatDuration(null), '');
});

test('formatBytes and truncate', () => {
  assert.equal(L.formatBytes(512), '512 B');
  assert.equal(L.formatBytes(2048), '2.0 KB');
  assert.equal(L.formatBytes(5 * 1024 * 1024), '5.0 MB');
  assert.equal(L.formatBytes(undefined), '');
  assert.equal(L.truncate('abcdef', 4), 'abc…');
  assert.equal(L.truncate('abc', 4), 'abc');
  assert.equal(L.truncate(null, 4), '');
});

test('addresses', () => {
  assert.deepEqual(L.addressList('a@x.com'), ['a@x.com']);
  assert.deepEqual(L.addressList(['a@x.com', 'b@y.com']), ['a@x.com', 'b@y.com']);
  assert.deepEqual(L.addressList(null), []);
  assert.equal(L.summarizeAddresses(['a@x.com', 'b@y.com', 'c@z.com']), 'a@x.com +2');
  assert.equal(L.summarizeAddresses('a@x.com'), 'a@x.com');
  assert.equal(L.summarizeAddresses([]), '—');
});

test('smtpReply does not repeat codes', () => {
  assert.equal(L.smtpReply(250, '2.0.0', 'OK queued as 12345'), '250 2.0.0 OK queued as 12345');
  assert.equal(L.smtpReply(550, '5.1.1', '550 5.1.1 User unknown'), '550 5.1.1 User unknown');
  assert.equal(L.smtpReply(null, '4.4.2', 'connection reset'), '4.4.2 connection reset');
  assert.equal(L.smtpReply(421, '', ''), '421');
  assert.equal(L.smtpReply(null, '', ''), '');
});

test('eventDetails', () => {
  const data = { attempt_id: 'x', smtp_code: 250, response: 'OK', mx_host: 'mx.example.com', source: 'dsn', extra: { a: 1 }, empty: '' };
  assert.deepEqual(L.eventDetails(data), [
    ['attempt id', 'x'], ['smtp code', '250'], ['response', 'OK'], ['mx host', 'mx.example.com'],
    ['source', 'dsn'], ['extra', '{"a":1}'],
  ]);
  assert.deepEqual(L.eventDetails(data, { fromAttempt: true }), [['source', 'dsn'], ['extra', '{"a":1}']]);
  assert.deepEqual(L.eventDetails(null), []);
  assert.deepEqual(L.eventDetails([1, 2]), []);
  assert.equal(L.eventDetails({ long: 'x'.repeat(500) })[0][1].length, 300);
});

test('keys', () => {
  assert.equal(L.keyLabel('omw_abcdefghijklmnop'), 'omw_abcdef…');
  assert.equal(L.keyLabel('omw_ab'), 'omw_ab');
  assert.ok(L.isCurrentKey('omw_abcdef', 'omw_abcdefghijk'));
  assert.ok(!L.isCurrentKey('omw_zzzzzz', 'omw_abcdefghijk'));
  assert.ok(!L.isCurrentKey('', 'omw_abcdefghijk'));
  assert.equal(L.checkKeyFormat('omw_abc123_-XYZ'), '');
  assert.match(L.checkKeyFormat(''), /Paste/);
  assert.match(L.checkKeyFormat('omwk_abc'), /worker token/);
  assert.match(L.checkKeyFormat('sk_live_abc'), /start with omw_/);
  assert.match(L.checkKeyFormat('omw_abc def'), /spaces/);
  assert.match(L.checkKeyFormat('omw_abc\n'), /spaces/);
});

test('isUUID and safeFilename', () => {
  assert.ok(L.isUUID('ed01a077-1c2b-4d3e-8f90-a1b2c3d4e5f6'));
  assert.ok(L.isUUID(' ED01A077-1C2B-4D3E-8F90-A1B2C3D4E5F6 '));
  assert.ok(!L.isUUID('ed01a077'));
  assert.ok(!L.isUUID('../../etc/passwd'));
  assert.equal(L.safeFilename('report.pdf'), 'report.pdf');
  assert.equal(L.safeFilename('../../etc/passwd'), '_.._etc_passwd');
  assert.equal(L.safeFilename('a\u0000b\nc.txt'), 'abc.txt');
  assert.equal(L.safeFilename(''), 'attachment');
  assert.equal(L.safeFilename('...'), 'attachment');
});

test('parseList', () => {
  assert.deepEqual(L.parseList(' *@Acme.com\nops@example.org, ops@example.org\n\n'), ['*@acme.com', 'ops@example.org']);
  assert.deepEqual(L.parseList('A@x.com, b@y.com', { lower: false }), ['A@x.com', 'b@y.com']);
  assert.deepEqual(L.parseList(''), []);
});

test('buildPolicy', () => {
  assert.deepEqual(L.buildPolicy({}), {});
  assert.deepEqual(L.buildPolicy({ maxPerHour: '100', maxPerDay: ' 1000 ', allowedRecipients: '*@acme.com\nops@example.org', allowedFrom: 'Agent@Acme.com' }), {
    max_per_hour: 100,
    max_per_day: 1000,
    allowed_recipients: ['*@acme.com', 'ops@example.org'],
    allowed_from: ['agent@acme.com'],
  });
  assert.throws(() => L.buildPolicy({ maxPerHour: '0' }), /Max per hour/);
  assert.throws(() => L.buildPolicy({ maxPerDay: '1.5' }), /Max per day/);
  assert.throws(() => L.buildPolicy({ maxPerDay: '-3' }), /Max per day/);
  assert.throws(() => L.buildPolicy({ allowedRecipients: 'acme.com' }), /"acme.com" is not an address pattern/);
  assert.throws(() => L.buildPolicy({ allowedFrom: 'a@b@c' }), /Allowed senders/);
  assert.throws(() => L.buildPolicy({ allowedFrom: '@acme.com' }), /Allowed senders/);
});

test('policySummary', () => {
  assert.deepEqual(L.policySummary({}), ['No limits']);
  assert.deepEqual(L.policySummary(null), ['No limits']);
  assert.deepEqual(L.policySummary({
    max_per_hour: 100, max_per_day: 1000,
    allowed_recipients: ['*@acme.com', 'ops@example.org', 'x@y.z'], allowed_from: ['agent@acme.com'],
  }), ['100 per hour', '1,000 per day', 'to *@acme.com, ops@example.org +1', 'from agent@acme.com']);
});

test('buildTimeline groups and orders by recipient', () => {
  const email = {
    recipients: [{ email: 'a@x.com', status: 'delivered' }, { email: 'b@y.com', status: 'deferred' }],
    attempts: [
      { id: 'a2', recipient: 'b@y.com', started_at: '2026-11-18T10:05:00Z', finished_at: '2026-11-18T10:05:02Z', outcome: 'deferred' },
      { id: 'a1', recipient: 'a@x.com', started_at: '2026-11-18T10:00:01Z', finished_at: '2026-11-18T10:00:02.5Z', outcome: 'delivered' },
      { id: 'a3', recipient: 'b@y.com', started_at: '2026-11-18T10:00:01Z', finished_at: null, outcome: '' },
    ],
    events: [
      { type: 'queued', recipient: 'a@x.com', created_at: '2026-11-18T10:00:00Z' },
      { type: 'queued', recipient: 'b@y.com', created_at: '2026-11-18T10:00:00Z' },
      { type: 'delivered', recipient: 'a@x.com', created_at: '2026-11-18T10:00:02.5Z', data: { attempt_id: 'a1' } },
      { type: 'deferred', recipient: 'b@y.com', created_at: '2026-11-18T10:05:02Z', data: { attempt_id: 'a2' } },
      { type: 'note', created_at: '2026-11-18T10:01:00Z' },
    ],
  };
  const groups = L.buildTimeline(email);
  assert.deepEqual(groups.map((g) => [g.recipient, g.status]), [['a@x.com', 'delivered'], ['b@y.com', 'deferred']]);

  const a = groups[0].entries;
  assert.deepEqual(a.map((e) => (e.kind === 'attempt' ? `attempt ${e.n}` : e.event.type)), ['queued', 'attempt 1', 'delivered', 'note']);
  assert.equal(a[1].duration, 1500);
  assert.equal(a[2].attemptNo, 1);
  assert.equal(a[0].attemptNo, null);

  const b = groups[1].entries;
  assert.deepEqual(b.map((e) => (e.kind === 'attempt' ? `attempt ${e.n}` : e.event.type)), ['queued', 'attempt 1', 'note', 'attempt 2', 'deferred']);
  assert.equal(b[1].duration, null); // still running
  assert.equal(b[4].attemptNo, 2);
});

test('buildTimeline puts an attempt before the event it caused at the same time', () => {
  const groups = L.buildTimeline({
    recipients: [{ email: 'a@x.com', status: 'delivered' }],
    attempts: [{ id: 'a1', recipient: 'a@x.com', started_at: '2026-11-18T10:00:00Z' }],
    events: [{ type: 'delivered', recipient: 'a@x.com', created_at: '2026-11-18T10:00:00Z', data: { attempt_id: 'a1' } }],
  });
  assert.deepEqual(groups[0].entries.map((e) => e.kind), ['attempt', 'event']);
  assert.deepEqual(L.buildTimeline({}), []);
});

test('classifyError', () => {
  assert.equal(L.classifyError({ status: 0 }).kind, 'network');
  assert.equal(L.classifyError({ status: 401, code: 'invalid_api_key' }).kind, 'auth');

  const forbidden = L.classifyError({ status: 403, code: 'insufficient_permission', message: 'needs full_access' }, 'API keys');
  assert.equal(forbidden.kind, 'forbidden');
  assert.equal(forbidden.title, "This key can't manage API keys");

  // Missing endpoints: the catch-all 404, a 404 without a JSON body, and 405.
  for (const err of [
    { status: 404, code: 'not_found', message: 'no such endpoint' },
    { status: 404, code: '', message: '' },
    { status: 405, code: '', message: '' },
  ]) {
    const c = L.classifyError(err, 'inboxes');
    assert.equal(c.kind, 'unavailable', JSON.stringify(err));
    assert.equal(c.title, 'Not available on this server version');
    assert.match(c.detail, /inboxes/);
  }
  // A missing resource is not a missing endpoint.
  assert.equal(L.classifyError({ status: 404, code: 'not_found', message: 'email not found' }).kind, 'notfound');

  const rate = L.classifyError({ status: 429, code: 'rate_limit_exceeded', message: 'hourly cap reached', retryAfter: 30 });
  assert.equal(rate.kind, 'rate');
  assert.equal(rate.detail, 'hourly cap reached. Try again in 30 s.');
  assert.equal(L.classifyError({ status: 429, message: 'Slow down.' }).detail, 'Slow down.');
  assert.equal(L.classifyError({ status: 422, code: 'validation_error', message: 'to is required' }).detail, 'to is required');
  assert.equal(L.classifyError({ status: 409, code: 'domain_exists', message: 'already added' }).kind, 'conflict');
  assert.equal(L.classifyError({ status: 403, code: 'domain_not_verified', message: 'publish its DNS records' }).kind, 'refused');
  assert.equal(L.classifyError({ status: 500, code: 'internal_error', message: 'internal error' }).kind, 'server');
  assert.match(L.classifyError({ status: 500, message: 'internal error' }).detail, /Try again/);
  assert.equal(L.classifyError({ status: 418 }).kind, 'other');
});

test('parseRoute', () => {
  assert.deepEqual(L.parseRoute(''), { page: 'overview', id: '' });
  assert.deepEqual(L.parseRoute('#/'), { page: 'overview', id: '' });
  assert.deepEqual(L.parseRoute('#/compose'), { page: 'compose', id: '' });
  assert.deepEqual(L.parseRoute('#/mail'), { page: 'mail', id: '' });
  assert.deepEqual(L.parseRoute('#/domains'), { page: 'domains', id: '' });
  assert.deepEqual(L.parseRoute('#/emails/ed01a077-1c2b-4d3e-8f90-a1b2c3d4e5f6'), { page: 'emails', id: 'ed01a077-1c2b-4d3e-8f90-a1b2c3d4e5f6' });
  assert.deepEqual(L.parseRoute('#/api-keys/'), { page: 'api-keys', id: '' });
  assert.deepEqual(L.parseRoute('#/messages/a%2Fb'), { page: 'messages', id: 'a/b' });
  assert.deepEqual(L.parseRoute('#/nope'), { page: 'notfound', id: '' });
  assert.deepEqual(L.parseRoute('#/emails/x/y'), { page: 'notfound', id: '' });
  assert.deepEqual(L.parseRoute('#/emails/%E0%A4%A'), { page: 'notfound', id: '' });
  assert.equal(L.parseRoute('#main'), null);
});

test('apiBase and listPath', () => {
  assert.equal(L.apiBase('/dashboard/'), '/');
  assert.equal(L.apiBase('/dashboard/index.html'), '/');
  assert.equal(L.apiBase('/mail/dashboard/'), '/mail/');
  assert.equal(L.apiBase('/'), '/');
  assert.equal(L.listPath('/emails', { limit: 25, before: undefined }), '/emails?limit=25');
  assert.equal(L.listPath('/emails', { limit: 25, before: 'abc' }), '/emails?limit=25&before=abc');
  assert.equal(L.listPath('/inboxes/1/messages', { thread_id: 'a b&c' }), '/inboxes/1/messages?thread_id=a+b%26c');
  assert.equal(L.listPath('/domains'), '/domains');
});

test('webhook event types include the server types', () => {
  const types = L.WEBHOOK_EVENTS.map(([t]) => t);
  for (const t of ['email.sent', 'email.delivered', 'email.delivery_delayed', 'email.bounced', 'email.failed', 'email.suppressed', 'email.delivery_unknown']) {
    assert.ok(types.includes(t), t);
  }
});

// ---- v0.6 ----

test('held and rejected statuses', () => {
  assert.equal(L.statusTone('held'), 'warn');
  assert.equal(L.statusLabel('held'), 'Held for approval');
  assert.equal(L.statusLabel('email.held'), 'Held for approval');
  assert.equal(L.statusTone('rejected'), 'bad');
  assert.ok(L.isFinal('rejected'));
  assert.ok(!L.isFinal('held'));
  const held = { recipients: [{ email: 'a@x.com', status: 'held' }, { email: 'b@y.com', status: 'delivered' }] };
  assert.ok(L.isPending(held));
  assert.ok(!L.isMoving(held), 'held mail waits for a person, so it is not polled');
  assert.ok(L.isMoving({ recipients: [{ status: 'held' }, { status: 'deferred' }] }));
  assert.deepEqual(L.heldRecipients(held), ['a@x.com']);
  assert.deepEqual(L.heldRecipients({}), []);
});

test('parseRoute knows the v0.6 pages', () => {
  for (const p of ['held', 'suppressions', 'pauses', 'settings', 'insights', 'audit-log']) {
    assert.deepEqual(L.parseRoute(`#/${p}`), { page: p, id: '' }, p);
  }
  assert.deepEqual(L.parseRoute('#/api-keys/6a1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d'), { page: 'api-keys', id: '6a1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d' });
});

test('classifyError: v0.6 refusals', () => {
  const own = L.classifyError({ status: 403, code: 'cannot_approve_own_email', message: 'an API key cannot approve the emails it sent itself' });
  assert.equal(own.kind, 'own');
  assert.match(own.title, /your own email/);
  assert.match(own.detail, /another key that has can_approve/);
  const paused = L.classifyError({ status: 403, code: 'sending_paused', message: 'sending is paused for this key' });
  assert.equal(paused.kind, 'paused');
  assert.equal(paused.detail, 'sending is paused for this key');
  assert.equal(L.classifyError({ status: 403, code: 'ip_not_allowed' }).title, 'Not allowed from this address');
  assert.match(L.classifyError({ status: 401, code: 'api_key_expired' }).detail, /expired/);
  assert.equal(L.classifyError({ status: 409, code: 'not_held', message: 'x' }).title, 'Already decided');
  assert.equal(L.classifyError({ status: 409, code: 'dkim_record_not_found', message: 'x' }).title, 'The DNS record is not published yet');
  assert.equal(L.classifyError({ status: 409, code: 'not_paused' }).title, 'Already resumed');
  const see = L.classifyError({ status: 403, code: 'insufficient_permission' }, 'insights', 'see');
  assert.equal(see.title, "This key can't see insights");
  assert.match(see.detail, /to see insights/);
});

test('webhook events include the v0.6 types', () => {
  const types = L.WEBHOOK_EVENTS.map(([t]) => t);
  for (const t of ['email.held', 'email.approved', 'email.rejected', 'sending.paused']) assert.ok(types.includes(t), t);
});

test('formatRate', () => {
  assert.equal(L.formatRate(0.966), '96.6%');
  assert.equal(L.formatRate(1), '100%');
  assert.equal(L.formatRate(0.05), '5%');
  assert.equal(L.formatRate(0.075), '7.5%');
  assert.equal(L.formatRate(0.001), '0.1%');
  assert.equal(L.formatRate(0.0007), '0.07%');
  assert.equal(L.formatRate(0.00123), '0.12%');
  assert.equal(L.formatRate(0), '0%');
  assert.equal(L.formatRate(null), '—');
  assert.equal(L.formatRate(undefined), '—');
  assert.equal(L.formatRate('x'), '—');
});

test('formatSeconds', () => {
  assert.equal(L.formatSeconds(2.1), '2.1 s');
  assert.equal(L.formatSeconds(0.35), '350 ms');
  assert.equal(L.formatSeconds(64.8), '1 min 5 s');
  assert.equal(L.formatSeconds(0), '0 ms');
  assert.equal(L.formatSeconds(null), '—');
});

test('percent inputs', () => {
  assert.equal(L.percentInput(0.05), '5');
  assert.equal(L.percentInput(0.001), '0.1');
  assert.equal(L.percentInput(0.0035), '0.35');
  assert.equal(L.percentInput(null), '');
  assert.equal(L.parsePercent('5', 'x'), 0.05);
  assert.equal(L.parsePercent(' 0.1 %', 'x'), 0.001);
  assert.equal(L.parsePercent('100', 'x'), 1);
  assert.equal(L.parsePercent(L.percentInput(0.003), 'x'), 0.003);
  for (const bad of ['0', '', '-1', '101', 'abc', '1e-3']) {
    assert.throws(() => L.parsePercent(bad, 'Bounce rate'), /Bounce rate must be a percentage/, bad);
  }
});

test('projectPatch sends only what changed', () => {
  const cur = {
    max_per_recipient_per_day: null,
    circuit_breaker: { enabled: true, bounce_rate: 0.05, complaint_rate: 0.001, min_recipients: 50 },
  };
  const same = { maxPerRecipientPerDay: '', enabled: true, bounceRate: '5', complaintRate: '0.1', minRecipients: '50' };
  assert.deepEqual(L.projectPatch(cur, same), {});
  assert.deepEqual(L.projectPatch(cur, { ...same, maxPerRecipientPerDay: '3' }), { max_per_recipient_per_day: 3 });
  assert.deepEqual(L.projectPatch({ ...cur, max_per_recipient_per_day: 3 }, same), { max_per_recipient_per_day: null });
  assert.deepEqual(L.projectPatch(cur, { ...same, bounceRate: '3', enabled: false, minRecipients: '100' }),
    { circuit_breaker: { enabled: false, bounce_rate: 0.03, min_recipients: 100 } });
  assert.deepEqual(L.projectPatch(cur, { ...same, complaintRate: '0.05' }), { circuit_breaker: { complaint_rate: 0.0005 } });
  assert.throws(() => L.projectPatch(cur, { ...same, maxPerRecipientPerDay: '0' }), /Emails per recipient per day must be a whole number from 1 to 10,000/);
  assert.throws(() => L.projectPatch(cur, { ...same, maxPerRecipientPerDay: '10001' }), /per recipient/);
  assert.throws(() => L.projectPatch(cur, { ...same, minRecipients: '' }), /Minimum recipients/);
  assert.throws(() => L.projectPatch(cur, { ...same, bounceRate: '0' }), /Bounce rate/);
});

test('buildPolicy: approval, can_approve and inboxes', () => {
  assert.deepEqual(L.buildPolicy({ requireApproval: {}, canApprove: false, inboxes: '' }), {});
  assert.deepEqual(L.buildPolicy({
    requireApproval: { always: false, firstContact: true, hasAttachments: true, hasLinks: false, recipientsOver: '10' },
    canApprove: true,
    inboxes: 'Support@Acme.com\n*@agents.acme.com',
  }), {
    require_approval: { first_contact: true, has_attachments: true, recipients_over: 10 },
    can_approve: true,
    inboxes: ['support@acme.com', '*@agents.acme.com'],
  });
  // 0 is a real value: hold every email with more than 0 recipients.
  assert.deepEqual(L.buildPolicy({ requireApproval: { recipientsOver: '0' } }), { require_approval: { recipients_over: 0 } });
  assert.throws(() => L.buildPolicy({ requireApproval: { recipientsOver: '1001' } }), /recipients are over must be a whole number from 0 to 1,000/);
  assert.throws(() => L.buildPolicy({ requireApproval: { recipientsOver: '-1' } }), /recipients are over/);
  assert.throws(() => L.buildPolicy({ inboxes: 'support' }), /Inboxes: "support" is not an address pattern/);
});

test('policyForm is the inverse of buildPolicy', () => {
  const policy = {
    max_per_hour: 100, allowed_recipients: ['*@acme.com', 'ops@example.org'],
    require_approval: { always: true, recipients_over: 0 }, can_approve: true, inboxes: ['support@acme.com'],
  };
  const v = L.policyForm(policy);
  assert.equal(v.maxPerHour, '100');
  assert.equal(v.maxPerDay, '');
  assert.equal(v.allowedRecipients, '*@acme.com\nops@example.org');
  assert.deepEqual(v.requireApproval, { always: true, firstContact: false, hasAttachments: false, hasLinks: false, recipientsOver: '0' });
  assert.equal(v.canApprove, true);
  assert.deepEqual(L.buildPolicy(v), policy);
  assert.deepEqual(L.buildPolicy(L.policyForm(null)), {});
});

test('policySummary: v0.6 fields', () => {
  assert.deepEqual(L.policySummary({ require_approval: { first_contact: true, has_links: true, recipients_over: 5 }, can_approve: true, inboxes: ['a@x.com'] }),
    ['holds: first contact, links, over 5 recipients', 'can approve', 'inboxes a@x.com']);
  assert.deepEqual(L.policySummary({ require_approval: { always: true } }), ['holds: every email']);
  assert.equal(L.holdReasonLabel('first_contact'), 'First contact');
  assert.equal(L.holdReasonLabel('recipients_over'), 'Many recipients');
  assert.equal(L.holdReasonLabel('something_new'), 'Something new');
});

test('parseAllowedIPs', () => {
  assert.equal(L.parseAllowedIPs(''), null);
  assert.equal(L.parseAllowedIPs('  \n '), null);
  assert.deepEqual(L.parseAllowedIPs('203.0.113.7\n10.0.0.0/8, 2001:DB8::/32\n::1\n::ffff:192.0.2.1'),
    ['203.0.113.7', '10.0.0.0/8', '2001:db8::/32', '::1', '::ffff:192.0.2.1']);
  assert.deepEqual(L.parseAllowedIPs('2001:db8:0:0:0:0:0:1'), ['2001:db8:0:0:0:0:0:1']);
  for (const bad of ['256.1.1.1', '10.0.0.0/33', '10.0.0', 'example.com', '2001:db8::/129', '1::2::3', '10.0.0.0/8/1', '1:2:3:4:5:6:7:8:9']) {
    assert.throws(() => L.parseAllowedIPs(bad), /is not an address or CIDR prefix/, bad);
  }
  assert.throws(() => L.parseAllowedIPs(Array.from({ length: 101 }, (_, i) => `10.0.0.${i}`).join('\n')), /at most 100/);
});

test('expiry inputs round-trip in the local zone', () => {
  const now = Date.parse('2026-11-20T10:00:00Z');
  assert.equal(L.expiryFromInput('', now), null);
  const iso = L.expiryFromInput('2026-12-01T09:30', now);
  assert.equal(L.localInputValue(iso), '2026-12-01T09:30');
  assert.equal(new Date(iso).toISOString(), iso);
  assert.throws(() => L.expiryFromInput('2026-01-01T00:00', now), /in the future/);
  assert.throws(() => L.expiryFromInput('tomorrow', now), /enter a date and time/);
  assert.equal(L.localInputValue(null), '');
  assert.equal(L.localInputValue('nonsense'), '');
});

test('keyAccessSummary and isExpired', () => {
  const now = Date.parse('2026-11-20T10:00:00Z');
  assert.deepEqual(L.keyAccessSummary({ allowed_ips: null, expires_at: null }, now), []);
  assert.deepEqual(L.keyAccessSummary({ allowed_ips: ['10.0.0.0/8', '203.0.113.7', '::1'], expires_at: '2026-11-23T10:00:00Z' }, now),
    ['from 10.0.0.0/8, 203.0.113.7 +1', 'expires in 3 d']);
  assert.deepEqual(L.keyAccessSummary({ expires_at: '2026-11-19T10:00:00Z' }, now), ['expired 1 d ago']);
  assert.ok(L.isExpired({ expires_at: '2026-11-19T10:00:00Z' }, now));
  assert.ok(!L.isExpired({ expires_at: null }, now));
});

test('trustBadge maps the sender verdict', () => {
  assert.deepEqual([L.trustBadge('authenticated').tone, L.trustBadge('authenticated').label], ['ok', 'Authenticated']);
  assert.deepEqual([L.trustBadge('unauthenticated').tone, L.trustBadge('unauthenticated').label], ['warn', 'Unauthenticated']);
  assert.deepEqual([L.trustBadge('failed').tone, L.trustBadge('failed').label], ['bad', 'Failed authentication']);
  assert.deepEqual([L.trustBadge(undefined).tone, L.trustBadge(undefined).label], ['muted', 'Not checked']);
  assert.equal(L.trustBadge('__proto__').label, 'Not checked');
  assert.equal(L.suspiciousLabel('conceal_from_user'), 'Asks to hide something from the user');
  assert.equal(L.suspiciousLabel('new_rule'), 'New rule');
});

test('authRows and authTone', () => {
  const rows = L.authRows({
    trust: 'authenticated', reason: 'dkim aligned',
    spf: { result: 'pass', domain: 'example.org' },
    spf_helo: { result: 'none' },
    dkim: [{ result: 'pass', domain: 'example.org', selector: 's1', aligned: true }, { result: 'fail', domain: 'esp.net', aligned: false, reason: 'body hash' }],
    dmarc: { result: 'pass', from_domain: 'example.org', policy: 'reject', spf_aligned: true, dkim_aligned: true },
  });
  assert.deepEqual(rows, [
    ['SPF (envelope sender)', 'pass', 'example.org'],
    ['SPF (HELO)', 'none', ''],
    ['DKIM signature 1', 'pass', 'd=example.org · s=s1 · aligned'],
    ['DKIM signature 2', 'fail', 'd=esp.net · not aligned · body hash'],
    ['DMARC', 'pass', 'From domain example.org · policy p=reject · SPF aligned · DKIM aligned'],
  ]);
  assert.deepEqual(L.authRows({ spf: { result: 'fail' }, dkim: [] }), [['SPF (envelope sender)', 'fail', ''], ['DKIM', 'none', 'no signature']]);
  assert.deepEqual(L.authRows(null), []);
  assert.deepEqual(L.authRows([1]), []);
  assert.equal(L.authTone('pass'), 'ok');
  assert.equal(L.authTone('FAIL'), 'bad');
  assert.equal(L.authTone('softfail'), 'warn');
  assert.equal(L.authTone('none'), 'muted');
});

test('buildSendBody and checkVerdict', () => {
  assert.deepEqual(L.buildSendBody({ from: ' a@x.com ', to: 'B@y.com, c@z.com', subject: 'Hi', text: 'Body', stream: 'transactional' }),
    { from: 'a@x.com', to: ['B@y.com', 'c@z.com'], subject: 'Hi', text: 'Body' });
  assert.deepEqual(L.buildSendBody({ from: 'a@x.com', to: 'b@y.com', subject: 'S', text: 'T', stream: 'broadcast' }).stream, 'broadcast');
  assert.deepEqual(L.checkVerdict({ ok: true, would: 'send', warnings: [] }), { tone: 'ok', title: 'Ready to send' });
  assert.deepEqual(L.checkVerdict({ ok: false, would: 'send', warnings: [{ code: 'no_text_part' }] }), { tone: 'warn', title: 'Would send, with a warning' });
  assert.equal(L.checkVerdict({ would: 'send', warnings: [{}, {}] }).title, 'Would send, with 2 warnings');
  assert.deepEqual(L.checkVerdict({ would: 'hold', hold_reasons: ['always'] }), { tone: 'warn', title: 'Would be held for approval' });
  assert.deepEqual(L.checkVerdict({ would: 'refuse', refusals: [{}] }), { tone: 'bad', title: 'Would be refused' });
});

test('outcomeShares add up to 100', () => {
  const shares = L.outcomeShares({ attempts: 412, accepted: 398, deferred: 11, bounced: 3, unknown: 0 });
  assert.deepEqual(shares.map((x) => [x.key, x.percent]), [['accepted', 96], ['deferred', 3], ['bounced', 1]]);
  assert.equal(shares.reduce((a, x) => a + x.percent, 0), 100);
  const thirds = L.outcomeShares({ accepted: 1, deferred: 1, bounced: 1, unknown: 0 });
  assert.equal(thirds.reduce((a, x) => a + x.percent, 0), 100);
  assert.deepEqual(L.outcomeShares({ accepted: 0 }), []);
  assert.deepEqual(L.outcomeShares(null), []);
});

test('insight windows and providers', () => {
  const data = { windows: [{ window: '24h', total: { attempts: 1 }, by_provider: [{ provider: 'yahoo', attempts: 2 }, { provider: 'google', attempts: 9 }], by_sending_ip: [] }] };
  assert.equal(L.insightWindow(data, '24h').total.attempts, 1);
  assert.equal(L.insightWindow(data, '7d').total.attempts, 0);
  assert.deepEqual(L.insightWindow(null, '30d').by_provider, []);
  assert.deepEqual(L.byAttempts(data.windows[0].by_provider).map((g) => g.provider), ['google', 'yahoo']);
  assert.equal(L.providerLabel('google'), 'Google');
  assert.equal(L.providerLabel('other'), 'Other');
  assert.equal(L.providerLabel(''), 'Unknown');
});

test('rateBar and pauseCounts', () => {
  assert.deepEqual(L.rateBar(0.075, 0.05), { value: 80, limit: 53.3 });
  assert.deepEqual(L.rateBar(0.01, 0.05), { value: 16, limit: 80 });
  assert.deepEqual(L.rateBar(0, 0), { value: 0, limit: 0 });
  assert.equal(L.pauseCounts({ reason: 'bounce_rate', bounces: 45, recipients: 600 }), '45 bounces of 600 recipients');
  assert.equal(L.pauseCounts({ reason: 'complaint_rate', complaints: 1, recipients: 1200 }), '1 complaint of 1,200 recipients');
  assert.equal(L.pauseCounts({ reason: 'bounce_rate', bounces: 1 }), '1 bounce');
});

test('dkimState reads the rotation and Ed25519 steps', () => {
  const active = { selector: 'omw1', algorithm: 'rsa', state: 'active', signs: true };
  assert.deepEqual(L.dkimState([active]), { rotation: 'idle', next: null, ed25519: 'off', edKey: null, retiring: [] });
  const next = { selector: 'omw2', algorithm: 'rsa', state: 'pending', signs: false };
  const ed = { selector: 'omwe1', algorithm: 'ed25519', state: 'pending', signs: false };
  const s1 = L.dkimState([active, next, ed]);
  assert.equal(s1.rotation, 'pending');
  assert.equal(s1.next.selector, 'omw2');
  assert.equal(s1.ed25519, 'pending');
  const prev = { selector: 'omw0', algorithm: 'rsa', state: 'retiring', signs: false };
  const s2 = L.dkimState([active, prev, { ...ed, state: 'active', signs: true }]);
  assert.equal(s2.rotation, 'idle');
  assert.equal(s2.ed25519, 'active');
  assert.deepEqual(s2.retiring.map((k) => k.selector), ['omw0']);
  assert.equal(L.dkimState(null).rotation, 'idle');
});

test('audit log helpers', () => {
  const names = new Map([['6a1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d', 'ops']]);
  assert.equal(L.auditActor({ actor: 'cli' }, names), 'Operator (CLI)');
  assert.equal(L.auditActor({ actor: 'system' }, names), 'System');
  assert.equal(L.auditActor({ actor: 'api_key', actor_key_id: '6a1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d' }, names), 'Key ops');
  assert.equal(L.auditActor({ actor: 'api_key', actor_key_id: 'ffffffff-4e5f-4a6b-8c7d-9e0f1a2b3c4d' }, names), 'Key ffffffff…');
  assert.equal(L.auditActor({ actor: 'api_key' }), 'API key');
  assert.equal(L.shortHash('a'.repeat(64)), `${'a'.repeat(12)}…`);
  assert.equal(L.shortHash(''), '');
  assert.ok(L.AUDIT_ACTIONS.includes('api_key.created'));
});

test('suppression filters and reasons', () => {
  assert.deepEqual(L.suppressionQuery({}), {});
  assert.deepEqual(L.suppressionQuery({ address: ' Jane@Example.org ', domain: '@Example.org', reason: ' Hard_Bounce', stream: 'broadcast' }),
    { address: 'jane@example.org', domain: 'example.org', reason: 'hard_bounce', stream: 'broadcast' });
  assert.deepEqual(L.suppressionQuery({ stream: 'everything' }), {});
  assert.equal(L.listPath('/suppressions/export', L.suppressionQuery({ domain: 'a.com' })), '/suppressions/export?domain=a.com');
  assert.equal(L.checkSuppressionReason(''), '');
  assert.equal(L.checkSuppressionReason('customer_request'), '');
  assert.match(L.checkSuppressionReason('Customer Request'), /short name/);
  assert.match(L.checkSuppressionReason('x'.repeat(33)), /short name/);
  assert.equal(L.datedFilename('suppressions', 'csv', new Date(2026, 10, 20, 9)), 'suppressions-2026-11-20.csv');
});

test('webhookHealth', () => {
  const now = Date.parse('2026-11-20T10:00:00Z');
  assert.equal(L.webhookHealth({ consecutive_failures: 0 }, now).label, 'Healthy');
  assert.equal(L.webhookHealth({ consecutive_failures: 3 }, now).tone, 'warn');
  assert.equal(L.webhookHealth({ consecutive_failures: 5, paused_until: '2026-11-20T10:05:00Z' }, now).label, 'Breaker open');
  assert.equal(L.webhookHealth({ consecutive_failures: 0, paused_until: '2026-11-20T09:00:00Z' }, now).label, 'Healthy');
  assert.ok(L.ROTATE_OVERLAPS.some(([s]) => s === 0));
  assert.equal(L.ROTATE_OVERLAPS[0][0], 86400);
});

test('lookups ignore prototype names from the API', () => {
  for (const k of ['__proto__', 'constructor', 'toString', 'hasOwnProperty']) {
    assert.equal(L.statusTone(k), 'muted', k);
    assert.equal(typeof L.statusLabel(k), 'string', k);
    assert.equal(L.trustBadge(k).label, 'Not checked', k);
    assert.equal(typeof L.suspiciousLabel(k), 'string', k);
    assert.equal(typeof L.providerLabel(k), 'string', k);
    assert.equal(L.PAUSE_SCOPES[k], undefined, k);
    assert.equal(L.STREAM_LABELS[k], undefined, k);
    assert.equal(L.classifyError({ status: 409, code: k }).title, 'Conflict', k);
    assert.equal(typeof L.holdReasonLabel(k), 'string', k);
  }
});
