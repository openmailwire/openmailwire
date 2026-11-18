import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { test } from "node:test";

import { WebhookVerificationError, verifyWebhook } from "../dist/index.js";

// The reference vector from Svix's own libraries; the server's signer
// (internal/webhook.Sign) produces the same signature.
const VECTOR = {
  secret: "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw",
  id: "msg_p5jXN8AQM9LWM0D4loKWxJek",
  ts: "1614265330",
  body: '{"test": 2432232314}',
  sig: "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=",
};
const FOREVER = 100 * 365 * 24 * 3600;

const SECRET = "whsec_" + Buffer.from("0123456789abcdef0123456789abcdef").toString("base64");
const BODY = JSON.stringify({ type: "email.delivered", created_at: "2026-11-18T10:00:00Z", data: { email_id: "e1", to: ["x@example.test"] } });

function sign(secret, id, ts, body) {
  const key = Buffer.from(secret.replace(/^whsec_/, ""), "base64");
  return "v1," + createHmac("sha256", key).update(`${id}.${ts}.${body}`).digest("base64");
}

function svixHeaders({ id = "msg_1", ts = Math.floor(Date.now() / 1000), body = BODY, secret = SECRET } = {}) {
  return { "svix-id": id, "svix-timestamp": String(ts), "svix-signature": sign(secret, id, ts, body) };
}

async function rejects(promise, pattern) {
  await assert.rejects(promise, (err) => {
    assert.ok(err instanceof WebhookVerificationError, `expected WebhookVerificationError, got ${err}`);
    assert.match(err.message, pattern);
    return true;
  });
}

test("verifies the Svix reference vector", async () => {
  const headers = { "svix-id": VECTOR.id, "svix-timestamp": VECTOR.ts, "svix-signature": VECTOR.sig };
  const event = await verifyWebhook(VECTOR.secret, headers, VECTOR.body, FOREVER);
  assert.deepEqual(event, { test: 2432232314 });
});

test("accepts svix-* headers and returns the parsed event", async () => {
  const event = await verifyWebhook(SECRET, svixHeaders(), BODY);
  assert.equal(event.type, "email.delivered");
  assert.equal(event.data.email_id, "e1");
});

test("accepts webhook-* headers, in any case, and a Headers object", async () => {
  const h = svixHeaders();
  const plain = { "Webhook-Id": h["svix-id"], "WEBHOOK-TIMESTAMP": h["svix-timestamp"], "webhook-signature": h["svix-signature"] };
  assert.equal((await verifyWebhook(SECRET, plain, BODY)).type, "email.delivered");
  const fetchHeaders = new Headers(plain);
  assert.equal((await verifyWebhook(SECRET, fetchHeaders, BODY)).type, "email.delivered");
});

test("accepts the body as bytes", async () => {
  assert.equal((await verifyWebhook(SECRET, svixHeaders(), Buffer.from(BODY))).type, "email.delivered");
});

test("accepts any one of several space-separated signatures", async () => {
  const h = svixHeaders();
  const other = sign("whsec_" + Buffer.from("another secret, rotated out").toString("base64"), "msg_1", h["svix-timestamp"], BODY);
  h["svix-signature"] = `v2,ignored ${other} ${h["svix-signature"]}`;
  assert.equal((await verifyWebhook(SECRET, h, BODY)).type, "email.delivered");
});

test("refuses a wrong signature, a changed body and a wrong secret", async () => {
  const h = svixHeaders();
  await rejects(verifyWebhook(SECRET, h, BODY.replace("delivered", "bounced")), /no webhook signature matches/);
  await rejects(
    verifyWebhook("whsec_" + Buffer.from("wrong").toString("base64"), h, BODY),
    /no webhook signature matches/,
  );
  await rejects(verifyWebhook(SECRET, { ...h, "svix-signature": "v1,AAAA" }, BODY), /no webhook signature matches/);
  await rejects(verifyWebhook(SECRET, { ...h, "svix-id": "msg_2" }, BODY), /no webhook signature matches/);
});

test("refuses old and future timestamps, with a configurable tolerance", async () => {
  const now = Math.floor(Date.now() / 1000);
  await rejects(verifyWebhook(SECRET, svixHeaders({ ts: now - 301 }), BODY), /too old/);
  await rejects(verifyWebhook(SECRET, svixHeaders({ ts: now + 301 }), BODY), /future/);
  assert.ok(await verifyWebhook(SECRET, svixHeaders({ ts: now - 290 }), BODY));
  await rejects(verifyWebhook(SECRET, svixHeaders({ ts: now - 20 }), BODY, 10), /too old/);
  assert.ok(await verifyWebhook(SECRET, svixHeaders({ ts: now - 600 }), BODY, 900));
});

test("refuses missing headers, a bad timestamp and a bad secret", async () => {
  await rejects(verifyWebhook(SECRET, {}, BODY), /missing webhook headers/);
  const h = svixHeaders();
  delete h["svix-signature"];
  await rejects(verifyWebhook(SECRET, h, BODY), /missing webhook headers/);
  await rejects(verifyWebhook(SECRET, { ...svixHeaders(), "svix-timestamp": "soon" }, BODY), /timestamp/);
  await rejects(verifyWebhook("whsec_***", svixHeaders(), BODY), /signing secret/);
});
