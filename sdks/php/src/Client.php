<?php

declare(strict_types=1);

namespace OpenMailWire;

use OpenMailWire\Resource\ApiKeys;
use OpenMailWire\Resource\Domains;
use OpenMailWire\Resource\Emails;
use OpenMailWire\Resource\Inboxes;
use OpenMailWire\Resource\Messages;
use OpenMailWire\Resource\Webhooks;

/**
 * A client for the OpenMailWire HTTP API. Answers are decoded JSON arrays
 * that use the API's own field names.
 */
final class Client
{
    public const VERSION = '0.1.0';
    public const DEFAULT_BASE_URL = 'http://localhost:8080';

    private const PENDING = ['queued', 'sending', 'deferred'];

    public readonly string $baseUrl;
    public readonly Emails $emails;
    public readonly Domains $domains;
    public readonly Webhooks $webhooks;
    public readonly ApiKeys $apiKeys;
    public readonly Inboxes $inboxes;
    public readonly Messages $messages;

    private readonly string $apiKey;

    /**
     * @param string|null $apiKey  an "omw_…" key; default: the OMW_API_KEY environment variable
     * @param string|null $baseUrl default: OMW_API_URL, then http://localhost:8080
     * @param float       $timeout per-request timeout in seconds
     */
    public function __construct(?string $apiKey = null, ?string $baseUrl = null, private readonly float $timeout = 30.0)
    {
        $apiKey = ($apiKey !== null && $apiKey !== '') ? $apiKey : self::env('OMW_API_KEY');
        if ($apiKey === null) {
            throw new \InvalidArgumentException("OpenMailWire: no API key. Pass \$apiKey ('omw_…') or set OMW_API_KEY.");
        }
        $this->apiKey = $apiKey;
        $baseUrl = ($baseUrl !== null && $baseUrl !== '') ? $baseUrl : (self::env('OMW_API_URL') ?? self::DEFAULT_BASE_URL);
        $this->baseUrl = rtrim($baseUrl, '/');

        $this->emails = new Emails($this);
        $this->domains = new Domains($this);
        $this->webhooks = new Webhooks($this);
        $this->apiKeys = new ApiKeys($this);
        $this->inboxes = new Inboxes($this);
        $this->messages = new Messages($this);
    }

    private static function env(string $name): ?string
    {
        $value = getenv($name);
        if ($value === false || $value === '') {
            $value = $_ENV[$name] ?? $_SERVER[$name] ?? null;
        }
        return is_string($value) && $value !== '' ? $value : null;
    }

    /**
     * Sends one request and returns the decoded JSON answer (or the raw body
     * when $binary is true). Used by the resource classes.
     *
     * @internal
     *
     * @param array<string, scalar|null>|null $query
     *
     * @throws OpenMailWireException
     */
    public function request(
        string $method,
        string $path,
        ?array $query = null,
        mixed $body = null,
        ?string $idempotencyKey = null,
        bool $binary = false,
    ): mixed {
        $url = $this->baseUrl . $path;
        if ($query !== null) {
            $query = array_filter($query, static fn ($v): bool => $v !== null && $v !== '');
            if ($query !== []) {
                $url .= '?' . http_build_query($query, '', '&', PHP_QUERY_RFC3986);
            }
        }

        $headers = [
            'Authorization: Bearer ' . $this->apiKey,
            'Accept: ' . ($binary ? '*/*' : 'application/json'),
            'User-Agent: openmailwire-php/' . self::VERSION,
            'Expect:',
        ];
        if ($idempotencyKey !== null && $idempotencyKey !== '') {
            $headers[] = 'Idempotency-Key: ' . $idempotencyKey;
        }

        $responseHeaders = [];
        $options = [
            CURLOPT_CUSTOMREQUEST => $method,
            CURLOPT_RETURNTRANSFER => true,
            CURLOPT_FOLLOWLOCATION => false, // never send the key to a redirect target
            CURLOPT_TIMEOUT_MS => (int) ($this->timeout * 1000),
            CURLOPT_HEADERFUNCTION => static function ($handle, string $line) use (&$responseHeaders): int {
                if (str_starts_with($line, 'HTTP/')) {
                    $responseHeaders = [];
                } elseif (str_contains($line, ':')) {
                    [$name, $value] = explode(':', $line, 2);
                    $responseHeaders[strtolower(trim($name))] = trim($value);
                }
                return strlen($line);
            },
        ];
        if ($body !== null) {
            $headers[] = 'Content-Type: application/json';
            $options[CURLOPT_POSTFIELDS] = json_encode($body, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
        } elseif ($method !== 'GET' && $method !== 'DELETE') {
            // An empty body with Content-Length: 0, as other HTTP clients send.
            $headers[] = 'Content-Type:';
            $options[CURLOPT_POSTFIELDS] = '';
        }
        $options[CURLOPT_HTTPHEADER] = $headers;

        $handle = curl_init($url);
        if ($handle === false) {
            throw new OpenMailWireException(0, 'connection_error', "$method {$this->baseUrl}$path: curl_init failed");
        }
        curl_setopt_array($handle, $options);
        $raw = curl_exec($handle);
        if (!is_string($raw)) {
            $error = curl_error($handle);
            throw new OpenMailWireException(0, 'connection_error', "$method {$this->baseUrl}$path: $error");
        }
        $status = (int) curl_getinfo($handle, CURLINFO_RESPONSE_CODE);

        if ($status >= 300 && $status < 400) {
            // Redirects are never followed (CURLOPT_FOLLOWLOCATION is off), so
            // the API key never goes to another address.
            $location = $responseHeaders['location'] ?? 'another address';
            throw new OpenMailWireException(
                $status,
                'redirect_refused',
                "$method {$this->baseUrl}$path answered $status, a redirect to $location. Redirects are not followed, "
                . 'so the API key never goes elsewhere: set the base URL to the address the API answers at.',
            );
        }
        if ($status < 200 || $status > 299) {
            throw self::toException($status, $responseHeaders, $raw);
        }
        if ($binary) {
            return $raw;
        }
        if ($raw === '') {
            return [];
        }
        try {
            return json_decode($raw, true, 512, JSON_THROW_ON_ERROR);
        } catch (\JsonException $e) {
            throw new OpenMailWireException(
                $status,
                'invalid_response',
                "$method {$this->baseUrl}$path answered $status with a body that is not JSON: " . substr($raw, 0, 200),
                null,
                $e,
            );
        }
    }

    /**
     * @param array<string, string> $headers
     */
    private static function toException(int $status, array $headers, string $raw): OpenMailWireException
    {
        $retryAfter = self::retryAfter($headers['retry-after'] ?? null);
        $decoded = json_decode($raw, true);
        if (is_array($decoded) && isset($decoded['name']) && is_string($decoded['name'])) {
            $message = isset($decoded['message']) && is_string($decoded['message']) ? $decoded['message'] : '';
            return new OpenMailWireException($status, $decoded['name'], $message, $retryAfter);
        }
        $message = trim(substr($raw, 0, 500));
        return new OpenMailWireException($status, 'http_error', $message !== '' ? $message : "HTTP $status", $retryAfter);
    }

    private static function retryAfter(?string $value): ?int
    {
        if ($value === null || trim($value) === '') {
            return null;
        }
        $value = trim($value);
        if (ctype_digit($value)) {
            return (int) $value;
        }
        $at = strtotime($value);
        return $at === false ? null : max(0, $at - time());
    }

    /**
     * Polls emails->get() until no recipient is queued, sending or deferred,
     * and returns that email. Every recipient then has a final status
     * (delivered, bounced, expired, suppressed, unknown or canceled): check
     * each one.
     *
     * @return array<string, mixed>
     *
     * @throws WaitTimeoutException after $timeout seconds, carrying the last state seen
     * @throws OpenMailWireException
     */
    public function waitForDelivery(string $emailId, float $timeout = 60.0, float $interval = 1.0): array
    {
        $deadline = microtime(true) + $timeout;
        while (true) {
            $email = $this->emails->get($emailId);
            $pending = [];
            foreach ($email['recipients'] ?? [] as $recipient) {
                if (in_array($recipient['status'] ?? null, self::PENDING, true)) {
                    $pending[] = (string) $recipient['status'];
                }
            }
            if ($pending === []) {
                return $email;
            }
            $left = $deadline - microtime(true);
            if ($left <= 0) {
                throw new WaitTimeoutException(
                    sprintf('email %s: %d recipient(s) still %s after %s s', $emailId, count($pending), implode(', ', $pending), $timeout),
                    $email,
                );
            }
            usleep((int) (min($interval, $left) * 1_000_000));
        }
    }

    /**
     * Polls the inboxes until received messages answer the sent email
     * $emailId (their in_reply_to_email_id), and returns them, newest first.
     * Returns [] if none arrived within $timeout seconds. Looks at the 50
     * newest messages of each inbox, or only of $inboxId.
     *
     * @return list<array<string, mixed>>
     *
     * @throws OpenMailWireException
     */
    public function waitForReply(string $emailId, float $timeout = 60.0, float $interval = 2.0, ?string $inboxId = null): array
    {
        $deadline = microtime(true) + $timeout;
        while (true) {
            $ids = $inboxId !== null
                ? [$inboxId]
                : array_map(static fn (array $inbox): string => (string) $inbox['id'], $this->inboxes->list()['data'] ?? []);
            $found = [];
            foreach ($ids as $id) {
                $page = $this->inboxes->listMessages($id, limit: 50);
                foreach ($page['data'] ?? [] as $message) {
                    if (($message['in_reply_to_email_id'] ?? null) === $emailId) {
                        $found[] = $message;
                    }
                }
            }
            if ($found !== []) {
                usort($found, static fn (array $a, array $b): int => strcmp((string) ($b['received_at'] ?? ''), (string) ($a['received_at'] ?? '')));
                return $found;
            }
            $left = $deadline - microtime(true);
            if ($left <= 0) {
                return [];
            }
            usleep((int) (min($interval, $left) * 1_000_000));
        }
    }
}
