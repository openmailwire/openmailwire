<?php

declare(strict_types=1);

namespace OpenMailWire;

/**
 * Thrown by Client::waitForDelivery() when the timeout passes while a recipient
 * is still queued, sending, deferred or held. $email is the last state seen.
 */
class WaitTimeoutException extends \RuntimeException
{
    /**
     * @param array<string, mixed> $email
     */
    public function __construct(string $message, public readonly array $email)
    {
        parent::__construct($message);
    }
}
