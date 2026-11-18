<?php

declare(strict_types=1);

namespace OpenMailWire;

/**
 * Verifies webhook requests signed by OpenMailWire.
 */
final class Webhook
{
    public const DEFAULT_TOLERANCE = 300;

    /**
     * Checks that a webhook request was signed with $secret ("whsec_…") and is
     * fresh, and returns its decoded JSON body.
     *
     * - Accepts both header families: svix-id/svix-timestamp/svix-signature and
     *   webhook-id/webhook-timestamp/webhook-signature, in any case.
     * - Accepts any of several space-separated "v1,<base64>" signatures.
     * - Compares signatures in constant time (hash_equals).
     * - Refuses timestamps more than $tolerance seconds (default 300) from now.
     *
     * $headers may be getallheaders(), a PSR-7 getHeaders() array (lists of
     * values), Symfony's $request->headers->all(), or $_SERVER. $rawBody must be
     * the body exactly as received: file_get_contents('php://input').
     *
     * @param array<string, string|list<string>> $headers
     * @return array<string, mixed>
     *
     * @throws WebhookVerificationException
     */
    public static function verify(string $secret, array $headers, string $rawBody, int $tolerance = self::DEFAULT_TOLERANCE): array
    {
        $id = self::header($headers, 'svix-id');
        $ts = self::header($headers, 'svix-timestamp');
        $sigs = self::header($headers, 'svix-signature');
        if ($id === null || $ts === null || $sigs === null) {
            $id = self::header($headers, 'webhook-id');
            $ts = self::header($headers, 'webhook-timestamp');
            $sigs = self::header($headers, 'webhook-signature');
        }
        if ($id === null || $ts === null || $sigs === null) {
            throw new WebhookVerificationException(
                'missing webhook headers: expected svix-id, svix-timestamp and svix-signature, '
                . 'or webhook-id, webhook-timestamp and webhook-signature'
            );
        }
        if (preg_match('/^\d{1,15}$/', $ts) !== 1) {
            throw new WebhookVerificationException('invalid webhook timestamp header');
        }

        $now = time();
        $timestamp = (int) $ts;
        if ($now - $timestamp > $tolerance) {
            throw new WebhookVerificationException('webhook timestamp is too old');
        }
        if ($timestamp - $now > $tolerance) {
            throw new WebhookVerificationException('webhook timestamp is in the future');
        }

        $encoded = str_starts_with($secret, 'whsec_') ? substr($secret, strlen('whsec_')) : $secret;
        $key = base64_decode($encoded, true);
        if ($key === false || $key === '') {
            throw new WebhookVerificationException('the signing secret is not a valid whsec_… value');
        }

        $expected = base64_encode(hash_hmac('sha256', $id . '.' . $ts . '.' . $rawBody, $key, true));
        $ok = false;
        foreach (explode(' ', $sigs) as $part) {
            $pieces = explode(',', $part, 2);
            if (count($pieces) === 2 && $pieces[0] === 'v1' && hash_equals($expected, $pieces[1])) {
                $ok = true;
            }
        }
        if (!$ok) {
            throw new WebhookVerificationException('no webhook signature matches');
        }

        try {
            $event = json_decode($rawBody, true, 512, JSON_THROW_ON_ERROR);
        } catch (\JsonException $e) {
            throw new WebhookVerificationException('the webhook body is not valid JSON', 0, $e);
        }
        if (!is_array($event)) {
            throw new WebhookVerificationException('the webhook body is not a JSON object');
        }
        return $event;
    }

    /**
     * @param array<string, mixed> $headers
     */
    private static function header(array $headers, string $name): ?string
    {
        foreach ($headers as $key => $value) {
            $key = strtolower((string) $key);
            if (str_starts_with($key, 'http_')) {
                $key = str_replace('_', '-', substr($key, 5));
            }
            if ($key !== $name) {
                continue;
            }
            if (is_array($value)) {
                $value = $value[array_key_first($value)] ?? null;
            }
            if (is_string($value) && $value !== '') {
                return $value;
            }
        }
        return null;
    }
}
