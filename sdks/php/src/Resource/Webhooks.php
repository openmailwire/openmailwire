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
     * Leave $events empty to receive every type. $asymmetricSigning also
     * signs every request with Ed25519 ('v1a'); the answer then has the
     * 'public_key' (whpk_…) that verifies them.
     *
     * @param list<string> $events e.g. ['email.bounced', 'email.held'] (Webhook::EVENTS lists them)
     * @return array<string, mixed>
     */
    public function create(string $endpoint, array $events = [], ?bool $asymmetricSigning = null): array
    {
        $body = ['endpoint' => $endpoint];
        if ($events !== []) {
            $body['events'] = array_values($events);
        }
        if ($asymmetricSigning !== null) {
            $body['asymmetric_signing'] = $asymmetricSigning;
        }
        return $this->client->request('POST', '/webhooks', body: $body);
    }

    /**
     * Replaces the signing secret (and Ed25519 key). The previous ones keep
     * signing for $overlapSeconds (default one day; 0 stops them at once).
     * The answer carries the new 'signing_secret', shown once.
     *
     * @return array<string, mixed>
     */
    public function rotateSecret(string $id, ?int $overlapSeconds = null): array
    {
        $body = new \stdClass();
        if ($overlapSeconds !== null) {
            $body->overlap_seconds = $overlapSeconds;
        }
        return $this->client->request('POST', '/webhooks/' . rawurlencode($id) . '/rotate-secret', body: $body);
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
