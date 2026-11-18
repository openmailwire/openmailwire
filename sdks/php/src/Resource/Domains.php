<?php

declare(strict_types=1);

namespace OpenMailWire\Resource;

use OpenMailWire\Client;

final class Domains
{
    public function __construct(private readonly Client $client)
    {
    }

    /**
     * Adds a domain and returns the DNS records to publish.
     *
     * @return array<string, mixed>
     */
    public function create(string $name): array
    {
        return $this->client->request('POST', '/domains', body: ['name' => $name]);
    }

    /**
     * @return array{object: string, data: list<array<string, mixed>>}
     */
    public function list(): array
    {
        return $this->client->request('GET', '/domains');
    }

    /**
     * One domain with its records and their last check.
     *
     * @return array<string, mixed>
     */
    public function get(string $id): array
    {
        return $this->client->request('GET', '/domains/' . rawurlencode($id));
    }

    /**
     * Checks the DNS records now.
     *
     * @return array<string, mixed>
     */
    public function verify(string $id): array
    {
        return $this->client->request('POST', '/domains/' . rawurlencode($id) . '/verify');
    }

    /**
     * @return array{object: string, id: string, deleted: bool}
     */
    public function delete(string $id): array
    {
        return $this->client->request('DELETE', '/domains/' . rawurlencode($id));
    }
}
