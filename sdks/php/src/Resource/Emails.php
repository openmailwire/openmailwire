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
     * Queues an email and returns ['id' => …], plus 'held' => true and
     * 'hold_reasons' when the key's require_approval policy holds it.
     *
     * $params uses the API's field names: from, to, cc, bcc, reply_to, subject,
     * html, text, headers, tags, attachments, scheduled_at, expires_at,
     * retry_uncertain and stream ('transactional', the default, or
     * 'broadcast'). Attachment 'content' must be base64
     * (base64_encode(file_get_contents($path))). scheduled_at and expires_at
     * may be DateTimeInterface objects.
     *
     * @param array<string, mixed> $params
     * @return array{id: string, held?: bool, hold_reasons?: list<string>}
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
     * Reports what send() would do with $params now ('would': send, hold or
     * refuse, with every refusal and warning), without sending anything.
     *
     * @param array<string, mixed> $params the same fields as send()
     * @return array<string, mixed>
     */
    public function check(array $params): array
    {
        return $this->client->request('POST', '/emails/check', body: self::body($params));
    }

    /**
     * Emails waiting for approval, newest first.
     *
     * @return array{object?: string, data: list<array<string, mixed>>, has_more: bool, next_before: string|null}
     */
    public function listHeld(?int $limit = null, ?string $before = null): array
    {
        return $this->client->request('GET', '/emails/held', query: ['limit' => $limit, 'before' => $before]);
    }

    /**
     * Approves a held email: its recipients are queued. Needs a full_access
     * key or one with can_approve; no key approves its own email.
     *
     * @return array<string, mixed>
     */
    public function approve(string $id, ?string $note = null): array
    {
        return $this->client->request('POST', '/emails/' . rawurlencode($id) . '/approve', body: self::decision($note));
    }

    /**
     * Rejects a held email: it is never sent.
     *
     * @return array<string, mixed>
     */
    public function reject(string $id, ?string $note = null): array
    {
        return $this->client->request('POST', '/emails/' . rawurlencode($id) . '/reject', body: self::decision($note));
    }

    /**
     * The received messages that answer a sent email, oldest first.
     *
     * @return array{object: string, email_id: string, thread_id: string, data: list<array<string, mixed>>}
     */
    public function replies(string $id): array
    {
        return $this->client->request('GET', '/emails/' . rawurlencode($id) . '/replies');
    }

    private static function decision(?string $note): \stdClass
    {
        $body = new \stdClass();
        if ($note !== null) {
            $body->note = $note;
        }
        return $body;
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
