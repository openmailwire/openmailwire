<?php

declare(strict_types=1);

namespace OpenMailWire;

/**
 * An error answered by the OpenMailWire API, in its {statusCode, name, message}
 * shape. $name is the machine-readable error name, e.g. "validation_error",
 * "domain_not_verified" or "rate_limit_exceeded" (the constants below list
 * them); getMessage() is the message.
 *
 * Network failures use statusCode 0 and name "connection_error".
 */
class OpenMailWireException extends \RuntimeException
{
    public const MISSING_API_KEY = 'missing_api_key';
    public const INVALID_API_KEY = 'invalid_api_key';
    /** 401: the key's expires_at has passed. */
    public const API_KEY_EXPIRED = 'api_key_expired';
    /** 403: the key may be used only from its allowed_ips. */
    public const IP_NOT_ALLOWED = 'ip_not_allowed';
    /** 403: this needs a full_access key (or can_approve, for held emails). */
    public const INSUFFICIENT_PERMISSION = 'insufficient_permission';
    public const DOMAIN_NOT_FOUND = 'domain_not_found';
    public const DOMAIN_NOT_VERIFIED = 'domain_not_verified';
    public const SENDER_NOT_ALLOWED = 'sender_not_allowed';
    public const RECIPIENT_NOT_ALLOWED = 'recipient_not_allowed';
    /** 403: a circuit breaker paused the key, domain or project (pauses->resume()). */
    public const SENDING_PAUSED = 'sending_paused';
    /** 403: no key approves or rejects its own email. */
    public const CANNOT_APPROVE_OWN_EMAIL = 'cannot_approve_own_email';
    public const NOT_FOUND = 'not_found';
    public const IDEMPOTENCY_CONFLICT = 'idempotency_conflict';
    public const CANNOT_CANCEL = 'cannot_cancel';
    /** 409: approve or reject on an email that is not held any more. */
    public const NOT_HELD = 'not_held';
    public const DOMAIN_EXISTS = 'domain_exists';
    /** 409: another project verified this domain first. */
    public const DOMAIN_VERIFIED_ELSEWHERE = 'domain_verified_elsewhere';
    /** 409: a new DKIM key's TXT record is not published yet. */
    public const DKIM_RECORD_NOT_FOUND = 'dkim_record_not_found';
    /** 409: the project has as many webhooks as it may (10 by default). */
    public const WEBHOOK_LIMIT_REACHED = 'webhook_limit_reached';
    public const PAYLOAD_TOO_LARGE = 'payload_too_large';
    public const VALIDATION_ERROR = 'validation_error';
    /** 429, with $retryAfter: a key's cap, or too many requests per second. */
    public const RATE_LIMIT_EXCEEDED = 'rate_limit_exceeded';
    /** 429, with $retryAfter: the project's max_per_recipient_per_day. */
    public const RECIPIENT_RATE_LIMITED = 'recipient_rate_limited';
    public const INTERNAL_ERROR = 'internal_error';
    /** 503, with $retryAfter: too many large requests at once. */
    public const SERVER_BUSY = 'server_busy';
    /** Not from the API: the request got no answer (statusCode 0). */
    public const CONNECTION_ERROR = 'connection_error';
    /** Not from the API: a 3xx answer, which the SDK never follows. */
    public const REDIRECT_REFUSED = 'redirect_refused';

    /**
     * @param int      $statusCode HTTP status, or 0 when the request never got an answer
     * @param string   $name       the API's error name
     * @param int|null $retryAfter seconds to wait, from the Retry-After header; set on 429 answers
     *                             (rate_limit_exceeded, recipient_rate_limited) and on 503 server_busy
     */
    public function __construct(
        public readonly int $statusCode,
        public readonly string $name,
        string $message,
        public readonly ?int $retryAfter = null,
        ?\Throwable $previous = null,
    ) {
        parent::__construct($message, $statusCode, $previous);
    }

    /**
     * True for a 429 or 503 answer: the request was refused before any work,
     * so it may be sent again (with the same idempotency key) after
     * $retryAfter seconds.
     */
    public function isRetryable(): bool
    {
        return $this->statusCode === 429 || $this->statusCode === 503;
    }
}
