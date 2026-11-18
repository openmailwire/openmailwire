<?php

declare(strict_types=1);

namespace OpenMailWire\Resource;

use OpenMailWire\Client;

final class Webhooks
{
    public function __construct(private readonly Client $client)
    {
    }

    /**
     * Creates a webhook. The answer carries 'signing_secret', shown once.
     * Leave $events empty to receive every type.
     *
     * @param list<string> $events e.g. ['email.bounced', 'email.delivered']
     * @return array<string, mixed>
     */
    public function create(string $endpoint, array $events = []): array
    {
        $body = ['endpoint' => $endpoint];
        if ($events !== []) {
            $body['events'] = array_values($events);
        }
        return $this->client->request('POST', '/webhooks', body: $body);
    }

    /**
     * @return array{object: string, data: list<array<string, mixed>>}
     */
    public function list(): array
    {
        return $this->client->request('GET', '/webhooks');
    }

    /**
     * One webhook with its 20 most recent deliveries.
     *
     * @return array<string, mixed>
     */
    public function get(string $id): array
    {
        return $this->client->request('GET', '/webhooks/' . rawurlencode($id));
    }

    /**
     * @return array{object: string, id: string, deleted: bool}
     */
    public function delete(string $id): array
    {
        return $this->client->request('DELETE', '/webhooks/' . rawurlencode($id));
    }
}
