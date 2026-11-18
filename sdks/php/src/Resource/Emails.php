<?php

declare(strict_types=1);

namespace OpenMailWire\Resource;

use OpenMailWire\Client;

final class Emails
{
    public function __construct(private readonly Client $client)
    {
    }

    /**
     * Queues an email and returns ['id' => …].
     *
     * $params uses the API's field names: from, to, cc, bcc, reply_to, subject,
     * html, text, headers, tags, attachments, scheduled_at, expires_at and
     * retry_uncertain. Attachment 'content' must be base64
     * (base64_encode(file_get_contents($path))). scheduled_at and expires_at
     * may be DateTimeInterface objects.
     *
     * @param array<string, mixed> $params
     * @return array{id: string}
     */
    public function send(array $params, ?string $idempotencyKey = null): array
    {
        return $this->client->request('POST', '/emails', body: self::body($params), idempotencyKey: $idempotencyKey);
    }

    /**
     * One email with each recipient's status, every attempt and the event timeline.
     *
     * @return array<string, mixed>
     */
    public function get(string $id): array
    {
        return $this->client->request('GET', '/emails/' . rawurlencode($id));
    }

    /**
     * Emails, newest first. Pass 'next_before' as $before for the next page.
     *
     * @return array{object?: string, data: list<array<string, mixed>>, has_more: bool, next_before: string|null}
     */
    public function list(?int $limit = null, ?string $before = null, ?string $apiKeyId = null): array
    {
        return $this->client->request('GET', '/emails', query: ['limit' => $limit, 'before' => $before, 'api_key_id' => $apiKeyId]);
    }

    /**
     * Cancels every recipient not tried yet. The API answers 409 cannot_cancel
     * if there is none.
     *
     * @return array<string, mixed>
     */
    public function cancel(string $id): array
    {
        return $this->client->request('POST', '/emails/' . rawurlencode($id) . '/cancel');
    }

    /**
     * Queues 1 to 100 emails (without attachments) in one transaction.
     *
     * @param list<array<string, mixed>> $emails
     * @return array{data: list<array{id: string}>}
     */
    public function batch(array $emails, ?string $idempotencyKey = null): array
    {
        $body = array_values(array_map(static fn (array $e): array => self::body($e), $emails));
        return $this->client->request('POST', '/emails/batch', body: $body, idempotencyKey: $idempotencyKey);
    }

    /**
     * Queues a complete MIME message (base64-encoded for you). Its From
     * domain must be verified.
     *
     * @param list<string>|null $rcpts envelope recipients; default: the To and Cc headers
     * @return array{id: string}
     */
    public function sendRaw(string $raw, ?string $mailFrom = null, ?array $rcpts = null, ?string $idempotencyKey = null): array
    {
        $body = ['raw' => base64_encode($raw)];
        if ($mailFrom !== null) {
            $body['mail_from'] = $mailFrom;
        }
        if ($rcpts !== null) {
            $body['rcpts'] = array_values($rcpts);
        }
        return $this->client->request('POST', '/emails/raw', body: $body, idempotencyKey: $idempotencyKey);
    }

    /**
     * @param array<string, mixed> $params
     * @return array<string, mixed>
     */
    private static function body(array $params): array
    {
        $body = array_filter($params, static fn ($v): bool => $v !== null);
        foreach (['scheduled_at', 'expires_at'] as $key) {
            if (isset($body[$key]) && $body[$key] instanceof \DateTimeInterface) {
                $body[$key] = $body[$key]->format(\DateTimeInterface::RFC3339);
            }
        }
        if (isset($body['headers']) && $body['headers'] === []) {
            unset($body['headers']);
        }
        if (isset($body['attachments']) && is_array($body['attachments'])) {
            $body['attachments'] = array_values(array_map(
                static fn (array $a): array => array_filter($a, static fn ($v): bool => $v !== null),
                $body['attachments'],
            ));
        }
        return $body;
    }
}
