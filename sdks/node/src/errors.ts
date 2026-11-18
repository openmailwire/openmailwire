import type { Email } from "./types.js";

/**
 * An error answered by the OpenMailWire API, in its `{statusCode, name, message}`
 * shape. `name` is the API's machine-readable error name, e.g.
 * `validation_error`, `domain_not_verified` or `rate_limit_exceeded`.
 *
 * Network failures use `statusCode: 0` and `name: "connection_error"`.
 */
export class OpenMailWireError extends Error {
  /** HTTP status, or 0 when the request never got an answer. */
  readonly statusCode: number;
  /** For 429 answers: seconds to wait, from the `Retry-After` header. */
  readonly retryAfter: number | null;

  constructor(statusCode: number, name: string, message: string, retryAfter: number | null = null) {
    super(message);
    this.name = name;
    this.statusCode = statusCode;
    this.retryAfter = retryAfter;
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
