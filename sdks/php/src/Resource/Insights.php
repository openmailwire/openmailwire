<?php

declare(strict_types=1);

namespace OpenMailWire\Resource;

use OpenMailWire\Client;

/**
 * Delivery insights. Needs a full-access key.
 */
final class Insights
{
    public function __construct(private readonly Client $client)
    {
    }

    /**
     * Acceptance, deferral and bounce rates and time to acceptance, per
     * mailbox provider and sending IP, for the windows '24h', '7d' and '30d'.
     * The server always computes all three; with $window, only that one is
     * kept in 'windows'.
     *
     * @return array<string, mixed>
     */
    public function get(?string $window = null): array
    {
        $insights = $this->client->request('GET', '/insights');
        if ($window !== null && is_array($insights['windows'] ?? null)) {
            $insights['windows'] = array_values(array_filter(
                $insights['windows'],
                static fn ($w): bool => is_array($w) && ($w['window'] ?? null) === $window,
            ));
        }
        return $insights;
    }
}
