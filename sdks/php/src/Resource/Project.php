<?php

declare(strict_types=1);

namespace OpenMailWire\Resource;

use OpenMailWire\Client;

/**
 * The project's sending settings: the per-recipient cap and the circuit
 * breaker.
 */
final class Project
{
    public function __construct(private readonly Client $client)
    {
    }

    /**
     * @return array<string, mixed>
     */
    public function get(): array
    {
        return $this->client->request('GET', '/project');
    }

    /**
     * Changes the settings. Arguments left null keep their value;
     * $clearMaxPerRecipientPerDay turns the per-recipient cap off. Needs a
     * full-access key.
     *
     * @param int|null                  $maxPerRecipientPerDay 1 to 10000 emails per recipient in any 24 hours
     * @param array<string, mixed>|null $circuitBreaker        enabled, bounce_rate, complaint_rate, min_recipients
     * @return array<string, mixed>
     */
    public function update(
        ?int $maxPerRecipientPerDay = null,
        ?array $circuitBreaker = null,
        bool $clearMaxPerRecipientPerDay = false,
    ): array {
        $body = [];
        if ($clearMaxPerRecipientPerDay) {
            $body['max_per_recipient_per_day'] = null;
        } elseif ($maxPerRecipientPerDay !== null) {
            $body['max_per_recipient_per_day'] = $maxPerRecipientPerDay;
        }
        if ($circuitBreaker !== null) {
            $body['circuit_breaker'] = (object) $circuitBreaker;
        }
        return $this->client->request('PATCH', '/project', body: (object) $body);
    }
}
