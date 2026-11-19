<?php

declare(strict_types=1);

namespace OpenMailWire\Resource;

use OpenMailWire\Client;

/**
 * Addresses OpenMailWire does not send to. A suppression's stream is 'all'
 * (no mail at all) or 'broadcast' (only broadcast mail, as after a one-click
 * unsubscribe).
 */
final class Suppressions
{
    public function __construct(private readonly Client $client)
    {
    }

    /**
     * Suppressed addresses, newest first. Pass 'next_before' as $before for
     * the next page.
     *
     * @return array{object?: string, data: list<array<string, mixed>>, has_more: bool, next_before: string|null}
     */
    public function list(
        ?int $limit = null,
        ?string $before = null,
        ?string $reason = null,
        ?string $stream = null,
        ?string $address = null,
        ?string $domain = null,
    ): array {
        return $this->client->request('GET', '/suppressions', query: [
            'limit' => $limit,
            'before' => $before,
            'reason' => $reason,
            'stream' => $stream,
            'address' => $address,
            'domain' => $domain,
        ]);
    }

    /**
     * Stops sending to an address. 'created' is false if it was already
     * suppressed for that stream. Needs a full-access key.
     *
     * @param string|null $reason a short name such as 'customer_request'; default 'manual'
     * @param string|null $stream 'all' (the default) or 'broadcast'
     * @return array<string, mixed>
     */
    public function create(string $address, ?string $reason = null, ?string $detail = null, ?string $stream = null): array
    {
        $body = ['address' => $address];
        if ($reason !== null) {
            $body['reason'] = $reason;
        }
        if ($detail !== null) {
            $body['detail'] = $detail;
        }
        if ($stream !== null) {
            $body['stream'] = $stream;
        }
        return $this->client->request('POST', '/suppressions', body: $body);
    }

    /**
     * Sends to an address again: removes its suppressions, or only those of
     * $stream. Needs a full-access key.
     *
     * @return array{object: string, address: string, deleted: bool, removed: list<array<string, mixed>>}
     */
    public function delete(string $address, ?string $stream = null): array
    {
        return $this->client->request('DELETE', '/suppressions/' . rawurlencode($address), query: ['stream' => $stream]);
    }

    /**
     * Every matching suppression, as CSV text.
     */
    public function export(?string $reason = null, ?string $stream = null, ?string $address = null, ?string $domain = null): string
    {
        return $this->client->request('GET', '/suppressions/export', query: [
            'reason' => $reason,
            'stream' => $stream,
            'address' => $address,
            'domain' => $domain,
        ], text: true);
    }
}
