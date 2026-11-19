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
     * Replies from the inbox address, threaded with In-Reply-To and
     * References. $to defaults to the first Reply-To address, or From. The
     * answer has 'id', 'thread_id', 'to' and, when addresses were left out or
     * Reply-To is on another domain, 'warnings'.
     *
     * @param list<string>|null $to
     * @return array{id: string, thread_id?: string, to?: list<string>, warnings?: list<string>}
     */
    public function reply(string $id, ?string $text = null, ?string $html = null, ?array $to = null): array
    {
        $body = [];
        if ($text !== null) {
            $body['text'] = $text;
        }
        if ($html !== null) {
            $body['html'] = $html;
        }
        if ($to !== null) {
            $body['to'] = array_values($to);
        }
        return $this->client->request('POST', '/messages/' . rawurlencode($id) . '/reply', body: (object) $body);
    }

    /**
     * Releases a held message, so keys with access to its inbox see it. Needs
     * a full-access key. Answers the message without its content.
     *
     * @return array<string, mixed>
     */
    public function release(string $id): array
    {
        return $this->client->request('POST', '/messages/' . rawurlencode($id) . '/release');
    }
}
