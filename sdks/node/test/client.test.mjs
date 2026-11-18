import assert from "node:assert/strict";
import { afterEach, test } from "node:test";

import { OpenMailWire, OpenMailWireError, WaitTimeoutError } from "../dist/index.js";
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
