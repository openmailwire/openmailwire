import assert from "node:assert/strict";
import { afterEach, test } from "node:test";

import { ErrorNames, OpenMailWire, OpenMailWireError, WaitTimeoutError } from "../dist/index.js";
import { startMock } from "./mock-server.mjs";

const ID = "ed01a077-1c2d-4e5f-8a9b-0c1d2e3f4a5b";
const KEY = "omw_test_key";

let mock;
afterEach(async () => {
  await mock?.close();
  mock = undefined;
});

async function client(handler, options = {}) {
  mock = await startMock(handler);
  return new OpenMailWire({ apiKey: KEY, baseUrl: mock.url, ...options });
}

function email(statuses) {
  return {
    object: "email",
    id: ID,
    from: "a@acme.test",
    to: ["x@example.test"],
    subject: "hi",
    recipients: statuses.map((status, i) => ({ email: `r${i}@example.test`, status, attempts: 0 })),
    attempts: [],
    events: [],
  };
}

// ---------------------------------------------------------------- configuration

test("reads OMW_API_URL and OMW_API_KEY when no options are given", async () => {
  mock = await startMock(() => ({ body: { object: "list", data: [] } }));
  const saved = { url: process.env.OMW_API_URL, key: process.env.OMW_API_KEY };
  process.env.OMW_API_URL = mock.url + "/";
  process.env.OMW_API_KEY = "omw_from_env";
  try {
    const c = new OpenMailWire();
    assert.equal(c.baseUrl, mock.url);
    await c.domains.list();
    assert.equal(mock.requests[0].headers.authorization, "Bearer omw_from_env");
  } finally {
    for (const [k, v] of [["OMW_API_URL", saved.url], ["OMW_API_KEY", saved.key]]) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
  }
});

test("defaults to http://localhost:8080 and needs an API key", () => {
  const saved = { url: process.env.OMW_API_URL, key: process.env.OMW_API_KEY };
  delete process.env.OMW_API_URL;
  delete process.env.OMW_API_KEY;
  try {
    assert.equal(new OpenMailWire({ apiKey: KEY }).baseUrl, "http://localhost:8080");
    assert.throws(() => new OpenMailWire(), /OMW_API_KEY/);
  } finally {
    if (saved.url !== undefined) process.env.OMW_API_URL = saved.url;
    if (saved.key !== undefined) process.env.OMW_API_KEY = saved.key;
  }
});

// ---------------------------------------------------------------- emails

test("emails.send converts fields, encodes attachments and sends the idempotency key", async () => {
  const c = await client(() => ({ body: { id: ID } }));
  const at = new Date("2030-01-02T03:04:05.000Z");
  const res = await c.emails.send(
    {
      from: "Acme <hello@acme.test>",
      to: ["you@example.test"],
      replyTo: "support@acme.test",
      subject: "Your code",
      text: "482913",
      tags: [{ name: "kind", value: "otp" }],
      attachments: [
        { filename: "a.txt", content: new TextEncoder().encode("hello"), contentType: "text/plain" },
        { filename: "logo.png", content: "iVBORw0K", contentId: "logo" },
      ],
      scheduledAt: at,
      retryUncertain: false,
    },
    { idempotencyKey: "signup-42" },
  );
  assert.deepEqual(res, { id: ID });
  const r = mock.requests[0];
  assert.equal(r.method, "POST");
  assert.equal(r.path, "/emails");
  assert.equal(r.headers.authorization, `Bearer ${KEY}`);
  assert.equal(r.headers["content-type"], "application/json");
  assert.equal(r.headers["idempotency-key"], "signup-42");
  assert.match(r.headers["user-agent"], /^openmailwire-node\//);
  assert.deepEqual(r.json, {
    from: "Acme <hello@acme.test>",
    to: ["you@example.test"],
    reply_to: "support@acme.test",
    subject: "Your code",
    text: "482913",
    tags: [{ name: "kind", value: "otp" }],
    attachments: [
      { filename: "a.txt", content: "aGVsbG8=", content_type: "text/plain" },
      { filename: "logo.png", content: "iVBORw0K", content_id: "logo" },
    ],
    scheduled_at: "2030-01-02T03:04:05.000Z",
    retry_uncertain: false,
  });
});

test("emails.send leaves out the idempotency key when none is given", async () => {
  const c = await client(() => ({ body: { id: ID } }));
  await c.emails.send({ from: "a@acme.test", to: "b@example.test", subject: "s", html: "<p>x</p>" });
  assert.equal(mock.requests[0].headers["idempotency-key"], undefined);
});

test("emails.get, list and cancel use the right paths", async () => {
  const c = await client((r) => {
    if (r.method === "GET" && r.path === `/emails/${ID}`) return { body: email(["delivered"]) };
    if (r.method === "GET" && r.path === "/emails") {
      return { body: { object: "list", data: [{ id: ID }], has_more: true, next_before: ID } };
    }
    if (r.method === "POST" && r.path === `/emails/${ID}/cancel`) return { body: { object: "email", id: ID } };
  });
  assert.equal((await c.emails.get(ID)).recipients[0].status, "delivered");
  const page = await c.emails.list({ limit: 10, before: ID, apiKeyId: "k1" });
  assert.equal(page.has_more, true);
  assert.equal(mock.requests[1].query.toString(), `limit=10&before=${ID}&api_key_id=k1`);
  await c.emails.list();
  assert.equal(mock.requests[2].query.toString(), "");
  assert.equal((await c.emails.cancel(ID)).id, ID);
});

test("emails.batch sends an array and the batch idempotency key", async () => {
  const c = await client(() => ({ body: { data: [{ id: "1" }, { id: "2" }] } }));
  const res = await c.emails.batch(
    [
      { from: "a@acme.test", to: "x@example.test", subject: "1", text: "one", replyTo: "r@acme.test" },
      { from: "a@acme.test", to: ["y@example.test"], subject: "2", text: "two" },
    ],
    { idempotencyKey: "nightly" },
  );
  assert.deepEqual(res.data.map((d) => d.id), ["1", "2"]);
  const r = mock.requests[0];
  assert.equal(r.path, "/emails/batch");
  assert.equal(r.headers["idempotency-key"], "nightly");
  assert.ok(Array.isArray(r.json));
  assert.equal(r.json[0].reply_to, "r@acme.test");
  assert.equal(r.json[1].subject, "2");
});

test("emails.sendRaw base64-encodes the MIME message", async () => {
  const c = await client(() => ({ body: { id: ID } }));
  const raw = "From: a@acme.test\r\nTo: x@example.test\r\nSubject: hi\r\n\r\nbody\r\n";
  await c.emails.sendRaw({ raw, mailFrom: "a@acme.test", rcpts: ["x@example.test"] }, { idempotencyKey: "r1" });
  const r = mock.requests[0];
  assert.equal(r.path, "/emails/raw");
  assert.equal(r.headers["idempotency-key"], "r1");
  assert.equal(Buffer.from(r.json.raw, "base64").toString("utf8"), raw);
  assert.equal(r.json.mail_from, "a@acme.test");
  assert.deepEqual(r.json.rcpts, ["x@example.test"]);
});

// ---------------------------------------------------------------- domains, webhooks, keys

test("domains: create, list, get, verify, delete", async () => {
  const dom = { object: "domain", id: ID, name: "acme.test", status: "pending", records: [] };
  const c = await client((r) => {
    if (r.path === "/domains" && r.method === "POST") return { status: 201, body: dom };
    if (r.path === "/domains") return { body: { object: "list", data: [dom] } };
    if (r.path === `/domains/${ID}/verify`) return { body: { ...dom, status: "verified" } };
    if (r.path === `/domains/${ID}` && r.method === "DELETE") return { body: { object: "domain", id: ID, deleted: true } };
    if (r.path === `/domains/${ID}`) return { body: dom };
  });
  assert.equal((await c.domains.create("acme.test")).name, "acme.test");
  assert.deepEqual(mock.requests[0].json, { name: "acme.test" });
  assert.equal((await c.domains.list()).data.length, 1);
  assert.equal((await c.domains.get(ID)).id, ID);
  assert.equal((await c.domains.verify(ID)).status, "verified");
  assert.equal(mock.requests[3].method, "POST");
  assert.equal((await c.domains.delete(ID)).deleted, true);
});

test("webhooks: create, list, get, delete", async () => {
  const hook = { object: "webhook", id: ID, endpoint: "https://h.test/x", events: ["email.bounced"] };
  const c = await client((r) => {
    if (r.path === "/webhooks" && r.method === "POST") return { status: 201, body: { ...hook, signing_secret: "whsec_abc" } };
    if (r.path === "/webhooks") return { body: { object: "list", data: [hook] } };
    if (r.path === `/webhooks/${ID}` && r.method === "DELETE") return { body: { object: "webhook", id: ID, deleted: true } };
    if (r.path === `/webhooks/${ID}`) return { body: { ...hook, recent_deliveries: [] } };
  });
  const created = await c.webhooks.create({ endpoint: hook.endpoint, events: ["email.bounced"] });
  assert.equal(created.signing_secret, "whsec_abc");
  assert.deepEqual(mock.requests[0].json, { endpoint: hook.endpoint, events: ["email.bounced"] });
  assert.equal((await c.webhooks.list()).data[0].id, ID);
  assert.deepEqual((await c.webhooks.get(ID)).recent_deliveries, []);
  assert.equal((await c.webhooks.delete(ID)).deleted, true);
});

test("apiKeys: create, list, get, update, delete", async () => {
  const key = { id: ID, name: "agent", prefix: "omw_ab", permission: "sending_access", policy: { max_per_hour: 10 } };
  const c = await client((r) => {
    if (r.path === "/api-keys" && r.method === "POST") return { status: 201, body: { ...key, token: "omw_secret" } };
    if (r.path === "/api-keys") return { body: { object: "list", data: [key] } };
    if (r.path === `/api-keys/${ID}` && r.method === "PATCH") return { body: { ...key, ...r.json } };
    if (r.path === `/api-keys/${ID}` && r.method === "DELETE") return { body: { object: "api_key", id: ID, deleted: true } };
    if (r.path === `/api-keys/${ID}`) return { body: key };
  });
  const policy = { max_per_hour: 10, allowed_recipients: ["*@acme.test"] };
  const created = await c.apiKeys.create({ name: "agent", permission: "sending_access", policy });
  assert.equal(created.token, "omw_secret");
  assert.deepEqual(mock.requests[0].json, { name: "agent", permission: "sending_access", policy });
  assert.equal((await c.apiKeys.list()).data[0].prefix, "omw_ab");
  assert.equal((await c.apiKeys.get(ID)).name, "agent");
  const updated = await c.apiKeys.update(ID, { name: "renamed" });
  assert.equal(updated.name, "renamed");
  assert.deepEqual(mock.requests[3].json, { name: "renamed" });
  assert.equal((await c.apiKeys.delete(ID)).deleted, true);
});

// ---------------------------------------------------------------- inboxes and messages

test("inboxes: create, list, delete, listMessages", async () => {
  const inbox = { id: "i1", address: "agent@acme.test", created_at: "2026-01-01T00:00:00Z" };
  const c = await client((r) => {
    if (r.path === "/inboxes" && r.method === "POST") return { status: 201, body: inbox };
    if (r.path === "/inboxes") return { body: { object: "list", data: [inbox] } };
    if (r.path === "/inboxes/i1" && r.method === "DELETE") return { body: { object: "inbox", id: "i1", deleted: true } };
    if (r.path === "/inboxes/i1/messages") return { body: { data: [{ id: "m1" }], has_more: false, next_before: null } };
  });
  assert.equal((await c.inboxes.create("agent@acme.test")).address, "agent@acme.test");
  assert.deepEqual(mock.requests[0].json, { address: "agent@acme.test" });
  assert.equal((await c.inboxes.list()).data.length, 1);
  const page = await c.inboxes.listMessages("i1", { limit: 5, threadId: "t1" });
  assert.equal(page.data[0].id, "m1");
  assert.equal(mock.requests[2].query.toString(), "limit=5&thread_id=t1");
  assert.equal((await c.inboxes.delete("i1")).deleted, true);
});

test("messages: get, getAttachment, reply", async () => {
  const bytes = new Uint8Array([0, 1, 2, 250, 255]);
  const c = await client((r) => {
    if (r.path === "/messages/m1") return { body: { id: "m1", subject: "Re: hi", attachments: [] } };
    if (r.path === "/messages/m1/attachments/0") return { body: bytes };
    if (r.path === "/messages/m1/reply") return { body: { id: ID } };
  });
  assert.equal((await c.messages.get("m1")).subject, "Re: hi");
  assert.deepEqual(await c.messages.getAttachment("m1", 0), bytes);
  assert.deepEqual(await c.messages.reply("m1", { text: "thanks" }), { id: ID });
  assert.deepEqual(mock.requests[2].json, { text: "thanks" });
  assert.equal(mock.requests[2].method, "POST");
});

// ---------------------------------------------------------------- v0.6: approvals, checks, replies

test("emails.send passes stream and returns the held fields", async () => {
  const c = await client(() => ({ body: { id: ID, held: true, hold_reasons: ["first_contact"] } }));
  const res = await c.emails.send({ from: "a@acme.test", to: "b@example.test", subject: "s", text: "x", stream: "broadcast" });
  assert.deepEqual(res, { id: ID, held: true, hold_reasons: ["first_contact"] });
  assert.equal(mock.requests[0].json.stream, "broadcast");
});

test("emails.check posts the send body to /emails/check", async () => {
  const result = {
    object: "email_check", ok: false, would: "hold", stream: "transactional", size: 812,
    hold_reasons: ["has_links"], refusals: [], warnings: [{ code: "no_text_part", message: "add a text part" }],
  };
  const c = await client(() => ({ body: result }));
  const at = new Date("2030-01-02T03:04:05.000Z");
  const res = await c.emails.check({ from: "a@acme.test", to: "b@example.test", subject: "s", html: "<a href=x>x</a>", scheduledAt: at });
  assert.deepEqual(res, result);
  const r = mock.requests[0];
  assert.equal(r.method, "POST");
  assert.equal(r.path, "/emails/check");
  assert.deepEqual(r.json, { from: "a@acme.test", to: "b@example.test", subject: "s", html: "<a href=x>x</a>", scheduled_at: "2030-01-02T03:04:05.000Z" });
});

test("emails.listHeld, approve, reject and replies", async () => {
  const decision = (d) => ({ object: "email", id: ID, decision: d, recipients: ["b@example.test"], suppressed: [] });
  const c = await client((r) => {
    if (r.method === "GET" && r.path === "/emails/held") {
      return { body: { object: "list", data: [{ object: "held_email", id: ID }], has_more: false, next_before: null } };
    }
    if (r.method === "POST" && r.path === `/emails/${ID}/approve`) return { body: decision("approved") };
    if (r.method === "POST" && r.path === `/emails/${ID}/reject`) return { body: decision("rejected") };
    if (r.method === "GET" && r.path === `/emails/${ID}/replies`) {
      return { body: { object: "list", email_id: ID, thread_id: "t1", data: [{ id: "m1", trust: "authenticated" }] } };
    }
  });
  const held = await c.emails.listHeld({ limit: 5, before: "e0" });
  assert.equal(held.data[0].object, "held_email");
  assert.equal(mock.requests[0].query.toString(), "limit=5&before=e0");
  await c.emails.listHeld();
  assert.equal(mock.requests[1].query.toString(), "");

  assert.equal((await c.emails.approve(ID, { note: "looks fine" })).decision, "approved");
  assert.deepEqual(mock.requests[2].json, { note: "looks fine" });
  assert.equal((await c.emails.approve(ID)).decision, "approved");
  assert.deepEqual(mock.requests[3].json, {});
  assert.equal((await c.emails.reject(ID, { note: "wrong customer" })).decision, "rejected");
  assert.deepEqual(mock.requests[4].json, { note: "wrong customer" });

  const replies = await c.emails.replies(ID);
  assert.equal(replies.thread_id, "t1");
  assert.equal(replies.data[0].trust, "authenticated");
});

// ---------------------------------------------------------------- v0.6: suppressions, pauses, project

test("suppressions: list, create, delete, export", async () => {
  const row = { address: "x@example.test", stream: "all", reason: "manual", detail: "", created_at: "2026-01-01T00:00:00Z" };
  const csv = "address,stream,reason,detail,created_at\nx@example.test,all,manual,,2026-01-01T00:00:00Z\n";
  const c = await client((r) => {
    if (r.method === "GET" && r.path === "/suppressions") return { body: { object: "list", data: [row], has_more: true, next_before: "c1" } };
    if (r.method === "POST" && r.path === "/suppressions") return { status: 201, body: { object: "suppression", ...row, created: true } };
    if (r.method === "DELETE" && r.path === "/suppressions/x%40example.test") {
      return { body: { object: "suppression", address: row.address, deleted: true, removed: [row] } };
    }
    if (r.method === "GET" && r.path === "/suppressions/export") return { body: csv, headers: { "content-type": "text/csv" } };
  });
  const page = await c.suppressions.list({ limit: 2, before: "c0", reason: "hard_bounce", stream: "broadcast", address: "x@example.test", domain: "example.test" });
  assert.equal(page.next_before, "c1");
  assert.equal(mock.requests[0].query.toString(), "limit=2&before=c0&reason=hard_bounce&stream=broadcast&address=x%40example.test&domain=example.test");

  const created = await c.suppressions.create({ address: "x@example.test", reason: "customer_request", detail: "asked by phone", stream: "all" });
  assert.equal(created.created, true);
  assert.deepEqual(mock.requests[1].json, { address: "x@example.test", reason: "customer_request", detail: "asked by phone", stream: "all" });
  await c.suppressions.create({ address: "x@example.test" });
  assert.deepEqual(mock.requests[2].json, { address: "x@example.test" });

  const removed = await c.suppressions.delete("x@example.test", { stream: "broadcast" });
  assert.equal(removed.removed.length, 1);
  assert.equal(mock.requests[3].query.toString(), "stream=broadcast");
  await c.suppressions.delete("x@example.test");
  assert.equal(mock.requests[4].query.toString(), "");

  assert.equal(await c.suppressions.export({ reason: "complaint" }), csv);
  assert.equal(mock.requests[5].query.toString(), "reason=complaint");
  assert.match(mock.requests[5].headers.accept, /text\/csv/);
  assert.equal(await c.suppressions.export(), csv);
});

test("pauses: list and resume", async () => {
  const pause = { object: "pause", id: "p1", scope: "domain", scope_id: "d1", reason: "bounce_rate", rate: 0.09, threshold: 0.05, active: true };
  const c = await client((r) => {
    if (r.method === "GET" && r.path === "/pauses") return { body: { object: "list", data: [pause] } };
    if (r.method === "POST" && r.path === "/pauses/p1/resume") return { body: { ...pause, active: false, resumed_by: "k1" } };
  });
  assert.equal((await c.pauses.list({ active: true, limit: 10 })).data[0].scope, "domain");
  assert.equal(mock.requests[0].query.toString(), "active=true&limit=10");
  await c.pauses.list();
  assert.equal(mock.requests[1].query.toString(), "");
  const resumed = await c.pauses.resume("p1");
  assert.equal(resumed.active, false);
  assert.equal(mock.requests[2].body, "");
});

test("project: get and update", async () => {
  const project = { object: "project", id: "pr1", name: "acme", max_per_recipient_per_day: 5, circuit_breaker: { enabled: true, bounce_rate: 0.05 } };
  const c = await client((r) => {
    if (r.method === "GET" && r.path === "/project") return { body: project };
    if (r.method === "PATCH" && r.path === "/project") return { body: { ...project, ...r.json } };
  });
  assert.equal((await c.project.get()).max_per_recipient_per_day, 5);
  const updated = await c.project.update({ maxPerRecipientPerDay: 3, circuitBreaker: { bounce_rate: 0.02, min_recipients: 100 } });
  assert.equal(updated.max_per_recipient_per_day, 3);
  assert.deepEqual(mock.requests[1].json, { max_per_recipient_per_day: 3, circuit_breaker: { bounce_rate: 0.02, min_recipients: 100 } });
  await c.project.update({ maxPerRecipientPerDay: null });
  assert.equal(mock.requests[2].body, '{"max_per_recipient_per_day":null}');
});

// ---------------------------------------------------------------- v0.6: audit log, insights

test("auditLog: list and verify", async () => {
  const c = await client((r) => {
    if (r.path === "/audit-log") return { body: { object: "list", data: [{ object: "audit_entry", id: 7, action: "api_key.created" }], has_more: true, next_before: "7" } };
    if (r.path === "/audit-log/verify") return { body: { object: "audit_verification", valid: true, entries: 7, head_hash: "ab", first_invalid_id: null } };
  });
  const page = await c.auditLog.list({ limit: 1, before: 9, action: "api_key.created" });
  assert.equal(page.data[0].id, 7);
  assert.equal(mock.requests[0].query.toString(), "limit=1&before=9&action=api_key.created");
  const v = await c.auditLog.verify();
  assert.equal(v.valid, true);
  assert.equal(v.head_hash, "ab");
});

test("insights.get returns every window, or only the one asked for", async () => {
  const w = (name) => ({ window: name, total: { attempts: 1, accepted: 1, deferred: 0, bounced: 0, unknown: 0 }, by_provider: [], by_sending_ip: [] });
  const c = await client((r) => {
    if (r.path === "/insights") return { body: { object: "insights", emails_considered: 1, truncated: false, windows: [w("24h"), w("7d"), w("30d")] } };
  });
  assert.deepEqual((await c.insights.get()).windows.map((x) => x.window), ["24h", "7d", "30d"]);
  const week = await c.insights.get({ window: "7d" });
  assert.deepEqual(week.windows.map((x) => x.window), ["7d"]);
  assert.equal(mock.requests[1].query.toString(), "");
});

// ---------------------------------------------------------------- v0.6: domains, webhooks, keys, inboxes

test("domains: dkim, rotateDkim, setEd25519, reports", async () => {
  const state = (object, status) => ({ object, status, keys: [{ selector: "omw2", algorithm: "rsa", state: "pending", signs: false, record: {} }] });
  const c = await client((r) => {
    if (r.method === "GET" && r.path === `/domains/${ID}/dkim`) return { body: state("dkim") };
    if (r.method === "POST" && r.path === `/domains/${ID}/dkim/rotate`) return { status: 202, body: state("dkim_rotation", "pending") };
    if (r.method === "POST" && r.path === `/domains/${ID}/dkim/ed25519`) return { body: state("dkim_ed25519", r.json.enabled ? "active" : "disabled") };
    if (r.method === "GET" && r.path === `/domains/${ID}/reports`) {
      return { body: { object: "domain_reports", domain_id: ID, domain: "acme.test", days: 7, dmarc: { pass: 3 }, tls: {}, reports: [] } };
    }
    if (r.method === "GET" && r.path === `/domains/${ID}`) {
      return { body: { object: "domain", id: ID, claim_expires_at: null, report_addresses: { dmarc_rua: "mailto:d@rp.acme.test" } } };
    }
  });
  assert.equal((await c.domains.dkim(ID)).keys[0].selector, "omw2");
  assert.equal((await c.domains.rotateDkim(ID)).status, "pending");
  assert.equal(mock.requests[1].body, "");
  assert.equal((await c.domains.setEd25519(ID, true)).status, "active");
  assert.deepEqual(mock.requests[2].json, { enabled: true });
  assert.equal((await c.domains.setEd25519(ID, false)).status, "disabled");
  assert.deepEqual(mock.requests[3].json, { enabled: false });
  assert.equal((await c.domains.reports(ID, { days: 7 })).dmarc.pass, 3);
  assert.equal(mock.requests[4].query.toString(), "days=7");
  await c.domains.reports(ID);
  assert.equal(mock.requests[5].query.toString(), "");
  const dom = await c.domains.get(ID);
  assert.equal(dom.claim_expires_at, null);
  assert.equal(dom.report_addresses?.dmarc_rua, "mailto:d@rp.acme.test");
});

test("webhooks: asymmetric signing on create, public key on get, rotateSecret", async () => {
  const hook = { object: "webhook", id: ID, endpoint: "https://h.test/x", events: null, asymmetric_signing: true, public_key: "whpk_abc" };
  const c = await client((r) => {
    if (r.method === "POST" && r.path === "/webhooks") return { status: 201, body: { ...hook, signing_secret: "whsec_1" } };
    if (r.method === "GET" && r.path === `/webhooks/${ID}`) return { body: hook };
    if (r.method === "POST" && r.path === `/webhooks/${ID}/rotate-secret`) {
      return { body: { ...hook, signing_secret: "whsec_2", previous_public_key: "whpk_abc", public_key: "whpk_def" } };
    }
  });
  await c.webhooks.create({ endpoint: hook.endpoint, events: ["email.held", "sending.paused"], asymmetricSigning: true });
  assert.deepEqual(mock.requests[0].json, { endpoint: hook.endpoint, events: ["email.held", "sending.paused"], asymmetric_signing: true });
  assert.equal((await c.webhooks.get(ID)).public_key, "whpk_abc");
  const rotated = await c.webhooks.rotateSecret(ID, { overlapSeconds: 0 });
  assert.equal(rotated.signing_secret, "whsec_2");
  assert.deepEqual(mock.requests[2].json, { overlap_seconds: 0 });
  await c.webhooks.rotateSecret(ID);
  assert.deepEqual(mock.requests[3].json, {});
});

test("apiKeys: allowedIps, expiresAt and the approval policy", async () => {
  const c = await client((r) => ({ status: r.method === "POST" ? 201 : 200, body: { id: ID, ...r.json } }));
  const policy = { require_approval: { first_contact: true, recipients_over: 5 }, can_approve: false, inboxes: ["support@acme.test"] };
  await c.apiKeys.create({
    name: "agent",
    permission: "sending_access",
    policy,
    allowedIps: ["10.0.0.0/8"],
    expiresAt: new Date("2031-01-01T00:00:00.000Z"),
  });
  assert.deepEqual(mock.requests[0].json, {
    name: "agent",
    permission: "sending_access",
    policy,
    allowed_ips: ["10.0.0.0/8"],
    expires_at: "2031-01-01T00:00:00.000Z",
  });
  await c.apiKeys.update(ID, { allowedIps: null, expiresAt: null });
  assert.equal(mock.requests[1].body, '{"allowed_ips":null,"expires_at":null}');
  await c.apiKeys.update(ID, { expiresAt: "2032-01-01T00:00:00Z", policy: { can_approve: true } });
  assert.deepEqual(mock.requests[2].json, { policy: { can_approve: true }, expires_at: "2032-01-01T00:00:00Z" });
});

test("inboxes: create with holdUnauthenticated, get, update", async () => {
  const inbox = { object: "inbox", id: "i1", address: "agent@acme.test", hold_unauthenticated: true };
  const c = await client((r) => {
    if (r.method === "POST" && r.path === "/inboxes") return { status: 201, body: { ...inbox, ...r.json } };
    if (r.method === "GET" && r.path === "/inboxes/i1") return { body: inbox };
    if (r.method === "PATCH" && r.path === "/inboxes/i1") return { body: { ...inbox, ...r.json } };
  });
  await c.inboxes.create("agent@acme.test", { holdUnauthenticated: false });
  assert.deepEqual(mock.requests[0].json, { address: "agent@acme.test", hold_unauthenticated: false });
  assert.equal((await c.inboxes.get("i1")).hold_unauthenticated, true);
  assert.equal((await c.inboxes.update("i1", { holdUnauthenticated: false })).hold_unauthenticated, false);
  assert.deepEqual(mock.requests[2].json, { hold_unauthenticated: false });
});

test("messages: trust fields, release, reply to and warnings", async () => {
  const msg = {
    id: "m1", trust: "unauthenticated", held: true, suspicious_instructions: true,
    authentication_results: { dmarc: { result: "none" } }, attachments: [],
  };
  const c = await client((r) => {
    if (r.method === "GET" && r.path === "/messages/m1") return { body: msg };
    if (r.method === "POST" && r.path === "/messages/m1/release") return { body: { ...msg, held: false, released_at: "2026-01-01T00:00:00Z" } };
    if (r.method === "POST" && r.path === "/messages/m1/reply") {
      return { body: { id: ID, thread_id: "t1", to: r.json.to, warnings: ["Reply-To is on another domain than From"] } };
    }
  });
  const m = await c.messages.get("m1");
  assert.deepEqual([m.trust, m.held, m.suspicious_instructions], ["unauthenticated", true, true]);
  const released = await c.messages.release("m1");
  assert.equal(released.held, false);
  assert.equal(mock.requests[1].body, "");
  const sent = await c.messages.reply("m1", { text: "thanks", to: ["a@example.test", "b@example.test"] });
  assert.deepEqual(sent.to, ["a@example.test", "b@example.test"]);
  assert.equal(sent.warnings.length, 1);
  assert.deepEqual(mock.requests[2].json, { text: "thanks", to: ["a@example.test", "b@example.test"] });
});

// ---------------------------------------------------------------- errors

test("API errors become OpenMailWireError with statusCode, name and message", async () => {
  const c = await client(() => ({
    status: 422,
    body: { statusCode: 422, name: "validation_error", message: "subject is required" },
  }));
  await assert.rejects(c.emails.send({ from: "a@acme.test", to: "b@example.test", subject: "", text: "x" }), (err) => {
    assert.ok(err instanceof OpenMailWireError);
    assert.equal(err.statusCode, 422);
    assert.equal(err.name, "validation_error");
    assert.equal(err.message, "subject is required");
    assert.equal(err.retryAfter, null);
    return true;
  });
});

test("a 429 carries retryAfter from the Retry-After header", async () => {
  const c = await client(() => ({
    status: 429,
    headers: { "Retry-After": "120" },
    body: { statusCode: 429, name: "rate_limit_exceeded", message: "hourly cap reached" },
  }));
  await assert.rejects(c.emails.send({ from: "a@acme.test", to: "b@example.test", subject: "s", text: "x" }), (err) => {
    assert.equal(err.statusCode, 429);
    assert.equal(err.name, "rate_limit_exceeded");
    assert.equal(err.retryAfter, 120);
    return true;
  });
});

test("recipient_rate_limited and server_busy carry retryAfter too", async () => {
  for (const [status, name] of [[429, "recipient_rate_limited"], [503, "server_busy"]]) {
    const c = await client(() => ({ status, headers: { "Retry-After": "10" }, body: { statusCode: status, name, message: "wait" } }));
    await assert.rejects(c.emails.send({ from: "a@acme.test", to: "b@example.test", subject: "s", text: "x" }), (err) => {
      assert.ok(err instanceof OpenMailWireError);
      assert.equal(err.statusCode, status);
      assert.equal(err.name, name);
      assert.equal(err.retryAfter, 10);
      assert.equal(err.retryable, true);
      return true;
    });
    assert.equal(mock.requests.length, 1, "no retry by default");
    await mock.close();
    mock = undefined;
  }
});

test("ErrorNames lists the v0.6 error names", () => {
  assert.equal(ErrorNames.sendingPaused, "sending_paused");
  assert.equal(ErrorNames.insufficientPermission, "insufficient_permission");
  assert.equal(ErrorNames.cannotApproveOwnEmail, "cannot_approve_own_email");
  assert.equal(ErrorNames.serverBusy, "server_busy");
  assert.equal(ErrorNames.domainVerifiedElsewhere, "domain_verified_elsewhere");
  assert.equal(ErrorNames.ipNotAllowed, "ip_not_allowed");
  assert.equal(ErrorNames.apiKeyExpired, "api_key_expired");
  assert.equal(ErrorNames.webhookLimitReached, "webhook_limit_reached");
  assert.equal(ErrorNames.recipientRateLimited, "recipient_rate_limited");
  assert.equal(new OpenMailWireError(422, "validation_error", "x").retryable, false);
});

test("maxRetries retries 429 and 503 after Retry-After, with the same idempotency key", async () => {
  const answers = [
    { status: 503, headers: { "Retry-After": "0" }, body: { statusCode: 503, name: "server_busy", message: "busy" } },
    { status: 429, headers: { "Retry-After": "0" }, body: { statusCode: 429, name: "rate_limit_exceeded", message: "slow down" } },
    { body: { id: ID } },
  ];
  let n = 0;
  const c = await client(() => answers[n++], { maxRetries: 2 });
  assert.deepEqual(await c.emails.send({ from: "a@acme.test", to: "b@example.test", subject: "s", text: "x" }, { idempotencyKey: "k1" }), { id: ID });
  assert.equal(mock.requests.length, 3);
  assert.ok(mock.requests.every((r) => r.headers["idempotency-key"] === "k1" && r.json.subject === "s"));
});

test("maxRetries gives up after the last retry, and on a Retry-After over maxRetryWaitMs", async () => {
  const busy = { status: 503, headers: { "Retry-After": "0" }, body: { statusCode: 503, name: "server_busy", message: "busy" } };
  let c = await client(() => busy, { maxRetries: 1 });
  await assert.rejects(c.domains.list(), (err) => err.name === "server_busy");
  assert.equal(mock.requests.length, 2);
  await mock.close();

  c = await client(() => ({ ...busy, headers: { "Retry-After": "3600" } }), { maxRetries: 3, maxRetryWaitMs: 5_000 });
  await assert.rejects(c.domains.list(), (err) => err.retryAfter === 3600);
  assert.equal(mock.requests.length, 1);
});

test("a non-JSON error body still gives a typed error", async () => {
  const c = await client(() => ({ status: 502, body: "Bad Gateway" }));
  await assert.rejects(c.domains.list(), (err) => {
    assert.ok(err instanceof OpenMailWireError);
    assert.equal(err.statusCode, 502);
    assert.equal(err.name, "http_error");
    assert.equal(err.message, "Bad Gateway");
    return true;
  });
});

test("redirects are refused, so the API key never goes elsewhere", async () => {
  // R4#10: the Python SDK forwarded Authorization on redirects; check this one.
  const other = await startMock(() => ({ body: { object: "list", data: [] } }));
  try {
    const omw = await client(() => ({ status: 307, body: "", headers: { location: other.url + "/domains" } }));
    await assert.rejects(omw.domains.list(), (err) => {
      assert.ok(err instanceof OpenMailWireError);
      assert.equal(err.statusCode, 307);
      assert.equal(err.name, "redirect_refused");
      assert.match(err.message, new RegExp(other.url.replace(/[.]/g, "\\.")));
      return true;
    });
    assert.equal(other.requests.length, 0);
    assert.equal(mock.requests[0].headers.authorization, `Bearer ${KEY}`);
  } finally {
    await other.close();
  }
});

test("an unreachable server gives connection_error", async () => {
  mock = await startMock(() => ({ body: {} }));
  const url = mock.url;
  await mock.close();
  mock = undefined;
  const c = new OpenMailWire({ apiKey: KEY, baseUrl: url });
  await assert.rejects(c.domains.list(), (err) => {
    assert.ok(err instanceof OpenMailWireError);
    assert.equal(err.statusCode, 0);
    assert.equal(err.name, "connection_error");
    return true;
  });
});

// ---------------------------------------------------------------- waiting

test("waitForDelivery polls until no recipient is queued, sending or deferred", async () => {
  const states = [["queued", "queued"], ["sending", "deferred"], ["delivered", "bounced"]];
  let n = 0;
  const c = await client(() => ({ body: email(states[Math.min(n++, states.length - 1)]) }));
  const e = await c.waitForDelivery(ID, 5_000, { intervalMs: 10 });
  assert.deepEqual(e.recipients.map((r) => r.status), ["delivered", "bounced"]);
  assert.equal(mock.requests.length, 3);
  assert.ok(mock.requests.every((r) => r.path === `/emails/${ID}`));
});

test("waitForDelivery keeps waiting while a recipient is held", async () => {
  const states = [["held"], ["held"], ["rejected"]];
  let n = 0;
  const c = await client(() => ({ body: email(states[Math.min(n++, states.length - 1)]) }));
  const e = await c.waitForDelivery(ID, 5_000, { intervalMs: 10 });
  assert.deepEqual(e.recipients.map((r) => r.status), ["rejected"]);
  assert.equal(mock.requests.length, 3);
});

test("waitForDelivery throws WaitTimeoutError with the last state", async () => {
  const c = await client(() => ({ body: email(["deferred"]) }));
  await assert.rejects(c.waitForDelivery(ID, 50, { intervalMs: 10 }), (err) => {
    assert.ok(err instanceof WaitTimeoutError);
    assert.equal(err.email.recipients[0].status, "deferred");
    return true;
  });
});

test("waitForReply finds messages that answer the email", async () => {
  let polls = 0;
  const c = await client((r) => {
    if (r.path === "/inboxes") return { body: { object: "list", data: [{ id: "i1" }, { id: "i2" }] } };
    if (r.path === "/inboxes/i1/messages") return { body: { data: [{ id: "m0", in_reply_to_email_id: "other" }], has_more: false } };
    if (r.path === "/inboxes/i2/messages") {
      assert.equal(r.query.get("limit"), "50");
      polls++;
      const data = polls < 2 ? [] : [{ id: "m1", in_reply_to_email_id: ID, received_at: "2026-01-01T00:00:00Z" }];
      return { body: { data, has_more: false } };
    }
  });
  const replies = await c.waitForReply(ID, 5_000, { intervalMs: 10 });
  assert.deepEqual(replies.map((m) => m.id), ["m1"]);
});

test("waitForReply returns [] after the timeout, and can watch one inbox", async () => {
  const c = await client((r) => {
    if (r.path === "/inboxes/i9/messages") return { body: { data: [], has_more: false } };
  });
  assert.deepEqual(await c.waitForReply(ID, 40, { inboxId: "i9", intervalMs: 10 }), []);
  assert.ok(mock.requests.every((r) => r.path === "/inboxes/i9/messages"));
});
