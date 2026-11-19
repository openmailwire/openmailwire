<?php

declare(strict_types=1);

namespace OpenMailWire\Resource;

use OpenMailWire\Client;

final class Inboxes
{
    public function __construct(private readonly Client $client)
    {
    }

    /**
     * Creates an inbox for an address on one of the project's domains.
     * $holdUnauthenticated (default true) holds mail whose sender is not
     * authenticated from sending_access keys until it is released.
     *
     * @return array<string, mixed>
     */
    public function create(string $address, ?bool $holdUnauthenticated = null): array
    {
        $body = ['address' => $address];
        if ($holdUnauthenticated !== null) {
            $body['hold_unauthenticated'] = $holdUnauthenticated;
        }
        return $this->client->request('POST', '/inboxes', body: $body);
    }

    /**
     * @return array{object: string, data: list<array<string, mixed>>}
     */
    public function list(): array
    {
        return $this->client->request('GET', '/inboxes');
    }

    /**
     * @return array<string, mixed>
     */
    public function get(string $id): array
    {
        return $this->client->request('GET', '/inboxes/' . rawurlencode($id));
    }

    /**
     * Changes whether mail from unauthenticated senders is held. Needs a
     * full-access key.
     *
     * @return array<string, mixed>
     */
    public function update(string $id, bool $holdUnauthenticated): array
    {
        return $this->client->request('PATCH', '/inboxes/' . rawurlencode($id), body: ['hold_unauthenticated' => $holdUnauthenticated]);
    }

    /**
     * @return array{object: string, id: string, deleted: bool}
     */
    public function delete(string $id): array
    {
        return $this->client->request('DELETE', '/inboxes/' . rawurlencode($id));
    }

    /**
     * Messages received by an inbox, newest first.
     *
     * @return array{data: list<array<string, mixed>>, has_more: bool, next_before: string|null}
     */
    public function listMessages(string $inboxId, ?int $limit = null, ?string $before = null, ?string $threadId = null): array
    {
        return $this->client->request(
            'GET',
            '/inboxes/' . rawurlencode($inboxId) . '/messages',
            query: ['limit' => $limit, 'before' => $before, 'thread_id' => $threadId],
        );
    }
}
