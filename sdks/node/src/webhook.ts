import { base64ToBytes, bytesToBase64, timingSafeEqual, toBytes, utf8 } from "./encoding.js";
import { WebhookVerificationError } from "./errors.js";
import type { WebhookEvent } from "./types.js";

/**
 * Request headers: a Fetch `Headers` object, or a plain object such as
 * Node's `req.headers` (names are matched case-insensitively).
 */
export type HeadersLike =
  | { get(name: string): string | null | undefined }
  | Record<string, string | string[] | undefined>;

function header(headers: HeadersLike, name: string): string | undefined {
  if (typeof (headers as { get?: unknown }).get === "function") {
    const v = (headers as { get(name: string): string | null | undefined }).get(name);
    if (v != null) return v;
  }
  for (const [k, v] of Object.entries(headers)) {
    if (k.toLowerCase() === name) return Array.isArray(v) ? v[0] : (v ?? undefined);
  }
  return undefined;
}

async function hmacSha256(key: Uint8Array, data: Uint8Array): Promise<Uint8Array> {
  let subtle = globalThis.crypto?.subtle;
  if (!subtle) {
    // Node 18 exposes Web Crypto only through the module.
    const nodeCrypto = await import("node:crypto");
    subtle = nodeCrypto.webcrypto.subtle as typeof subtle;
  }
  const k = await subtle.importKey("raw", key, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return new Uint8Array(await subtle.sign("HMAC", k, data));
}

function base64Url(bytes: Uint8Array): string {
  return bytesToBase64(bytes).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** Verifies an Ed25519 signature, with Web Crypto where it has Ed25519, else node:crypto. */
async function ed25519Verify(publicKey: Uint8Array, data: Uint8Array, signature: Uint8Array): Promise<boolean> {
  if (publicKey.length !== 32 || signature.length !== 64) return false;
  const subtle = globalThis.crypto?.subtle;
  if (subtle) {
    try {
      const k = await subtle.importKey("raw", publicKey, { name: "Ed25519" }, false, ["verify"]);
      return await subtle.verify({ name: "Ed25519" }, k, signature, data);
    } catch {
      // This runtime's Web Crypto has no Ed25519: use node:crypto below.
    }
  }
  try {
    const nodeCrypto = await import("node:crypto");
    const key = nodeCrypto.createPublicKey({
      key: { kty: "OKP", crv: "Ed25519", x: base64Url(publicKey) },
      format: "jwk",
    });
    return nodeCrypto.verify(null, data, key, signature);
  } catch {
    return false;
  }
}

/**
 * Checks that a webhook request was signed by OpenMailWire and is fresh, and
 * returns its parsed JSON body.
 *
 * `secret` is either the webhook's signing secret (`whsec_…`), checked against
 * the `v1` (HMAC-SHA256) signatures, or its Ed25519 public key (`whpk_…`, from
 * `webhooks.get`), checked against the `v1a` signatures of webhooks created
 * with `asymmetric_signing`.
 *
 * - Accepts both header families: `svix-id`/`svix-timestamp`/`svix-signature`
 *   and `webhook-id`/`webhook-timestamp`/`webhook-signature`.
 * - Accepts any of several space-separated signatures, so it keeps working
 *   while a secret is rotated.
 * - Compares HMAC signatures in constant time.
 * - Refuses timestamps more than `toleranceSeconds` (default 300) away from now.
 *
 * Pass the body exactly as received (a string or bytes), before any JSON
 * parsing: re-serialised JSON will not match the signature.
 *
 * @throws WebhookVerificationError
 */
export async function verifyWebhook(
  secret: string,
  headers: HeadersLike,
  rawBody: string | Uint8Array,
  toleranceSeconds = 300,
): Promise<WebhookEvent> {
  let id = header(headers, "svix-id");
  let ts = header(headers, "svix-timestamp");
  let sigs = header(headers, "svix-signature");
  if (!id || !ts || !sigs) {
    id = header(headers, "webhook-id");
    ts = header(headers, "webhook-timestamp");
    sigs = header(headers, "webhook-signature");
  }
  if (!id || !ts || !sigs) {
    throw new WebhookVerificationError(
      "missing webhook headers: expected svix-id, svix-timestamp and svix-signature, or webhook-id, webhook-timestamp and webhook-signature",
    );
  }
  if (!/^\d{1,15}$/.test(ts)) throw new WebhookVerificationError("invalid webhook timestamp header");

  const now = Math.floor(Date.now() / 1000);
  const timestamp = Number(ts);
  if (now - timestamp > toleranceSeconds) throw new WebhookVerificationError("webhook timestamp is too old");
  if (timestamp - now > toleranceSeconds) throw new WebhookVerificationError("webhook timestamp is in the future");

  const body = toBytes(rawBody);
  const prefix = utf8(`${id}.${ts}.`);
  const signed = new Uint8Array(prefix.length + body.length);
  signed.set(prefix, 0);
  signed.set(body, prefix.length);

  let ok = false;
  if (secret.startsWith("whpk_")) {
    let publicKey: Uint8Array;
    try {
      publicKey = base64ToBytes(secret.slice("whpk_".length));
    } catch {
      throw new WebhookVerificationError("the public key is not a valid whpk_… value");
    }
    if (publicKey.length !== 32) throw new WebhookVerificationError("the public key is not a valid whpk_… value");
    for (const part of sigs.split(" ")) {
      const comma = part.indexOf(",");
      if (comma < 0 || part.slice(0, comma) !== "v1a") continue;
      let sig: Uint8Array;
      try {
        sig = base64ToBytes(part.slice(comma + 1));
      } catch {
        continue;
      }
      if (await ed25519Verify(publicKey, signed, sig)) {
        ok = true;
        break;
      }
    }
  } else {
    let key: Uint8Array;
    try {
      key = base64ToBytes(secret.startsWith("whsec_") ? secret.slice("whsec_".length) : secret);
    } catch {
      throw new WebhookVerificationError("the signing secret is not a valid whsec_… value");
    }
    if (key.length === 0) throw new WebhookVerificationError("the signing secret is empty");
    const expected = bytesToBase64(await hmacSha256(key, signed));
    for (const part of sigs.split(" ")) {
      const comma = part.indexOf(",");
      if (comma < 0 || part.slice(0, comma) !== "v1") continue;
      if (timingSafeEqual(part.slice(comma + 1), expected)) ok = true;
    }
  }
  if (!ok) throw new WebhookVerificationError("no webhook signature matches");

  const text = typeof rawBody === "string" ? rawBody : new TextDecoder().decode(rawBody);
  try {
    return JSON.parse(text) as WebhookEvent;
  } catch {
    throw new WebhookVerificationError("the webhook body is not valid JSON");
  }
}
