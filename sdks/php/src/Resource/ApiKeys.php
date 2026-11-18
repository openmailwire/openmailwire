<?php

declare(strict_types=1);

namespace OpenMailWire\Resource;

use OpenMailWire\Client;

/**
 * API keys. These calls need a full-access key.
 */
final class ApiKeys
{
    public function __construct(private readonly Client $client)
    {
    }

    /**
     * Creates a key. The answer carries 'token', shown once.
     *
     * @param string|null               $permission 'full_access' (the default) or 'sending_access'
     * @param array<string, mixed>|null $policy     max_per_hour, max_per_day, allowed_recipients, allowed_from
     * @return array<string, mixed>
     */
    public function create(string $name, ?string $permission = null, ?array $policy = null): array
    {
        $body = ['name' => $name];
        if ($permission !== null) {
            $body['permission'] = $permission;
        }
        if ($policy !== null) {
            $body['policy'] = self::policy($policy);
        }
        return $this->client->request('POST', '/api-keys', body: $body);
    }

    /**
     * @return array{object: string, data: list<array<string, mixed>>}
     */
    public function list(): array
    {
        return $this->client->request('GET', '/api-keys');
    }

    /**
     * @return array<string, mixed>
     */
    public function get(string $id): array
    {
        return $this->client->request('GET', '/api-keys/' . rawurlencode($id));
    }

    /**
     * Renames a key or replaces its policy. Pass [] as $policy to remove every limit.
     *
     * @param array<string, mixed>|null $policy
     * @return array<string, mixed>
     */
    public function update(string $id, ?string $name = null, ?array $policy = null): array
    {
        $body = [];
        if ($name !== null) {
            $body['name'] = $name;
        }
        if ($policy !== null) {
            $body['policy'] = self::policy($policy);
        }
        return $this->client->request('PATCH', '/api-keys/' . rawurlencode($id), body: (object) $body);
    }

    /**
     * Revokes a key.
     *
     * @return array{object: string, id: string, deleted: bool}
     */
    public function delete(string $id): array
    {
        return $this->client->request('DELETE', '/api-keys/' . rawurlencode($id));
    }

    /**
     * An empty policy must be sent as {} rather than [].
     *
     * @param array<string, mixed> $policy
     * @return array<string, mixed>|\stdClass
     */
    private static function policy(array $policy): array|\stdClass
    {
        return $policy === [] ? new \stdClass() : $policy;
    }
}
