<?php

declare(strict_types=1);

namespace OpenMailWire\Resource;

use OpenMailWire\Client;

/**
 * Circuit-breaker pauses: sending stops by itself for an API key, a domain or
 * the project whose bounce or complaint rate is too high, and its sends are
 * refused with 403 sending_paused until a full-access key resumes it.
 */
final class Pauses
{
    public function __construct(private readonly Client $client)
    {
    }

    /**
     * The pauses, newest first. $active = true lists only those still in force.
     *
     * @return array{object: string, data: list<array<string, mixed>>}
     */
    public function list(?bool $active = null, ?int $limit = null): array
    {
        return $this->client->request('GET', '/pauses', query: ['active' => $active, 'limit' => $limit]);
    }

    /**
     * Lets the paused key, domain or project send again. Needs a full-access key.
     *
     * @return array<string, mixed>
     */
    public function resume(string $id): array
    {
        return $this->client->request('POST', '/pauses/' . rawurlencode($id) . '/resume');
    }
}
