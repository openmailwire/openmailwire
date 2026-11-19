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

    /**
     * The domain's DKIM keys, with the records to publish.
     *
     * @return array<string, mixed>
     */
    public function dkim(string $id): array
    {
        return $this->client->request('GET', '/domains/' . rawurlencode($id) . '/dkim');
    }

    /**
     * Rotates the DKIM key in two steps: the first call creates a key to
     * publish ('status' => 'pending'), a later call switches signing to it
     * once its record is in DNS ('rotated'). The API answers 409
     * dkim_record_not_found while the record is missing.
     *
     * @return array<string, mixed>
     */
    public function rotateDkim(string $id): array
    {
        return $this->client->request('POST', '/domains/' . rawurlencode($id) . '/dkim/rotate');
    }

    /**
     * Turns the extra Ed25519 DKIM signature on or off. Turning it on creates
     * a key to publish first; call again once its record is in DNS.
     *
     * @return array<string, mixed>
     */
    public function setEd25519(string $id, bool $enabled): array
    {
        return $this->client->request('POST', '/domains/' . rawurlencode($id) . '/dkim/ed25519', body: ['enabled' => $enabled]);
    }

    /**
     * DMARC aggregate and TLS-RPT reports received for the domain (default:
     * the last 30 days).
     *
     * @return array<string, mixed>
     */
    public function reports(string $id, ?int $days = null): array
    {
        return $this->client->request('GET', '/domains/' . rawurlencode($id) . '/reports', query: ['days' => $days]);
    }
}
