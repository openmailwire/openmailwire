import type { Email } from "./types.js";

/**
 * Error names the API answers with (the `name` of an `OpenMailWireError`).
 * Branch on these rather than on the message. Newer servers may add names.
 */
export const ErrorNames = {
  missingApiKey: "missing_api_key",
  invalidApiKey: "invalid_api_key",
  /** 401: the key's `expires_at` has passed. */
  apiKeyExpired: "api_key_expired",
  /** 403: the key may be used only from its `allowed_ips`. */
  ipNotAllowed: "ip_not_allowed",
  /** 403: this needs a `full_access` key (or `can_approve`, for held emails). */
  insufficientPermission: "insufficient_permission",
  domainNotFound: "domain_not_found",
  domainNotVerified: "domain_not_verified",
  senderNotAllowed: "sender_not_allowed",
  recipientNotAllowed: "recipient_not_allowed",
  /** 403: a circuit breaker paused the key, domain or project (`pauses.resume`). */
  sendingPaused: "sending_paused",
  /** 403: no key approves or rejects its own email. */
  cannotApproveOwnEmail: "cannot_approve_own_email",
  notFound: "not_found",
  idempotencyConflict: "idempotency_conflict",
  cannotCancel: "cannot_cancel",
  /** 409: `approve`/`reject` on an email that is not held any more. */
  notHeld: "not_held",
  domainExists: "domain_exists",
  /** 409: another project verified this domain first. */
  domainVerifiedElsewhere: "domain_verified_elsewhere",
  /** 409: a new DKIM key's TXT record is not published yet. */
  dkimRecordNotFound: "dkim_record_not_found",
  /** 409: the project has as many webhooks as it may (10 by default). */
  webhookLimitReached: "webhook_limit_reached",
  payloadTooLarge: "payload_too_large",
  validationError: "validation_error",
  /** 429, with `retryAfter`: a key's cap, or too many requests per second. */
  rateLimitExceeded: "rate_limit_exceeded",
  /** 429, with `retryAfter`: the project's `max_per_recipient_per_day`. */
  recipientRateLimited: "recipient_rate_limited",
  internalError: "internal_error",
  /** 503, with `retryAfter`: too many large requests at once. */
  serverBusy: "server_busy",
  /** Not from the API: the request got no answer (`statusCode` 0). */
  connectionError: "connection_error",
  /** Not from the API: a 3xx answer, which the SDK never follows. */
  redirectRefused: "redirect_refused",
} as const;

export type ErrorName = (typeof ErrorNames)[keyof typeof ErrorNames];

/**
 * An error answered by the OpenMailWire API, in its `{statusCode, name, message}`
 * shape. `name` is the API's machine-readable error name, e.g.
 * `validation_error`, `domain_not_verified` or `rate_limit_exceeded`; the
 * `ErrorNames` constants list them.
 *
 * Network failures use `statusCode: 0` and `name: "connection_error"`.
 */
export class OpenMailWireError extends Error {
  /** HTTP status, or 0 when the request never got an answer. */
  readonly statusCode: number;
  /**
   * Seconds to wait, from the `Retry-After` header, or null. Set on 429
   * answers (`rate_limit_exceeded`, `recipient_rate_limited`) and on 503
   * `server_busy`.
   */
  readonly retryAfter: number | null;

  constructor(statusCode: number, name: string, message: string, retryAfter: number | null = null) {
    super(message);
    this.name = name;
    this.statusCode = statusCode;
    this.retryAfter = retryAfter;
  }

  /**
   * True for a 429 or 503 answer: the request was refused before any work,
   * so it may be sent again (with the same idempotency key) after
   * `retryAfter` seconds.
   */
  get retryable(): boolean {
    return this.statusCode === 429 || this.statusCode === 503;
  }
}

/** Thrown by `verifyWebhook` when a request is not a genuine, fresh webhook. */
export class WebhookVerificationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "WebhookVerificationError";
  }
}

/**
 * Thrown by `waitForDelivery` when the timeout passes while a recipient is
 * still queued, sending or deferred. `email` is the last state seen.
 */
export class WaitTimeoutError extends Error {
  readonly email: Email;

  constructor(message: string, email: Email) {
    super(message);
    this.name = "WaitTimeoutError";
    this.email = email;
  }
}
