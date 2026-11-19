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
     * @param array<string, mixed>|null $policy     max_per_hour, max_per_day, allowed_recipients, allowed_from,
     *                                              require_approval, can_approve, inboxes
     * @param list<string>|null         $allowedIps addresses or CIDR prefixes the key may be used from
     * @param \DateTimeInterface|string|null $expiresAt when the key stops working (RFC 3339 when a string)
     * @return array<string, mixed>
     */
    public function create(
        string $name,
        ?string $permission = null,
        ?array $policy = null,
        ?array $allowedIps = null,
        \DateTimeInterface|string|null $expiresAt = null,
    ): array {
        $body = ['name' => $name];
        if ($permission !== null) {
            $body['permission'] = $permission;
        }
        if ($policy !== null) {
            $body['policy'] = self::policy($policy);
        }
        if ($allowedIps !== null) {
            $body['allowed_ips'] = array_values($allowedIps);
        }
        if ($expiresAt !== null) {
            $body['expires_at'] = self::time($expiresAt);
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
     * Renames a key, or replaces its policy, allowed networks or expiry.
     * Arguments left null are unchanged. Pass [] as $policy to remove every
     * limit; $clearAllowedIps removes the network restriction and
     * $clearExpiresAt makes the key never expire.
     *
     * @param array<string, mixed>|null $policy
     * @param list<string>|null         $allowedIps
     * @return array<string, mixed>
     */
    public function update(
        string $id,
        ?string $name = null,
        ?array $policy = null,
        ?array $allowedIps = null,
        \DateTimeInterface|string|null $expiresAt = null,
        bool $clearAllowedIps = false,
        bool $clearExpiresAt = false,
    ): array {
        $body = [];
        if ($name !== null) {
            $body['name'] = $name;
        }
        if ($policy !== null) {
            $body['policy'] = self::policy($policy);
        }
        if ($clearAllowedIps) {
            $body['allowed_ips'] = null;
        } elseif ($allowedIps !== null) {
            $body['allowed_ips'] = array_values($allowedIps);
        }
        if ($clearExpiresAt) {
            $body['expires_at'] = null;
        } elseif ($expiresAt !== null) {
            $body['expires_at'] = self::time($expiresAt);
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

    private static function time(\DateTimeInterface|string $value): string
    {
        return $value instanceof \DateTimeInterface ? $value->format(\DateTimeInterface::RFC3339) : $value;
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
