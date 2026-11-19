<?php

declare(strict_types=1);

namespace OpenMailWire\Resource;

use OpenMailWire\Client;

/**
 * The project's hash-chained, append-only audit log. Needs a full-access key.
 */
final class AuditLog
{
    public function __construct(private readonly Client $client)
    {
    }

    /**
     * Entries, newest first. Pass 'next_before' as $before for the next page.
     *
     * @param string|null $action only this action, e.g. 'api_key.created'
     * @return array{object?: string, data: list<array<string, mixed>>, has_more: bool, next_before: string|null}
     */
    public function list(?int $limit = null, string|int|null $before = null, ?string $action = null): array
    {
        return $this->client->request('GET', '/audit-log', query: ['limit' => $limit, 'before' => $before, 'action' => $action]);
    }

    /**
     * Checks the whole hash chain on the server. Keep 'head_hash' to compare
     * later: if a later chain no longer contains it, entries were removed.
     *
     * @return array<string, mixed>
     */
    public function verify(): array
    {
        return $this->client->request('GET', '/audit-log/verify');
    }
}
