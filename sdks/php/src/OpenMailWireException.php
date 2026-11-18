<?php

declare(strict_types=1);

namespace OpenMailWire;

/**
 * An error answered by the OpenMailWire API, in its {statusCode, name, message}
 * shape. $name is the machine-readable error name, e.g. "validation_error",
 * "domain_not_verified" or "rate_limit_exceeded"; getMessage() is the message.
 *
 * Network failures use statusCode 0 and name "connection_error".
 */
class OpenMailWireException extends \RuntimeException
{
    /**
     * @param int      $statusCode HTTP status, or 0 when the request never got an answer
     * @param string   $name       the API's error name
     * @param int|null $retryAfter for 429 answers: seconds to wait, from the Retry-After header
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
}
