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
  assert.deepEqual(L.parseRoute(''), { page: 'emails', id: '' });
  assert.deepEqual(L.parseRoute('#/'), { page: 'emails', id: '' });
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
