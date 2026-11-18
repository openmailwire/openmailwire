<?php

declare(strict_types=1);

namespace OpenMailWire\Resource;

use OpenMailWire\Client;

final class Messages
{
    public function __construct(private readonly Client $client)
    {
    }

    /**
     * One received message, with its text, HTML, headers and attachment list.
     *
     * @return array<string, mixed>
     */
    public function get(string $id): array
    {
        return $this->client->request('GET', '/messages/' . rawurlencode($id));
    }

    /**
     * The raw bytes of one attachment ($index from the message's attachments[].index).
     */
    public function getAttachment(string $id, int $index): string
    {
        return $this->client->request('GET', '/messages/' . rawurlencode($id) . '/attachments/' . $index, binary: true);
    }

    /**
     * Replies from the inbox address, threaded with In-Reply-To and References.
     *
     * @return array{id: string}
     */
    public function reply(string $id, ?string $text = null, ?string $html = null): array
    {
        $body = [];
        if ($text !== null) {
            $body['text'] = $text;
        }
        if ($html !== null) {
            $body['html'] = $html;
        }
        return $this->client->request('POST', '/messages/' . rawurlencode($id) . '/reply', body: (object) $body);
    }
}
