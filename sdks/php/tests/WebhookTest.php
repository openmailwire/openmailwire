<?php

declare(strict_types=1);

namespace OpenMailWire\Tests;

use OpenMailWire\Webhook;
use OpenMailWire\WebhookVerificationException;
use PHPUnit\Framework\TestCase;

use function OpenMailWire\verifyWebhook;

final class WebhookTest extends TestCase
{
    // The reference vector from Svix's own libraries; the server's signer
    // (internal/webhook.Sign) produces the same signature.
    private const VECTOR_SECRET = 'whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw';
    private const VECTOR_ID = 'msg_p5jXN8AQM9LWM0D4loKWxJek';
    private const VECTOR_TS = '1614265330';
    private const VECTOR_BODY = '{"test": 2432232314}';
    private const VECTOR_SIG = 'v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=';
    private const FOREVER = 100 * 365 * 24 * 3600;

    private const BODY = '{"type":"email.delivered","created_at":"2026-11-18T10:00:00Z","data":{"email_id":"e1","to":["x@example.test"]}}';

    private static function secret(string $raw = '0123456789abcdef0123456789abcdef'): string
    {
        return 'whsec_' . base64_encode($raw);
    }

    private static function sign(string $secret, string $id, int $ts, string $body): string
    {
        $key = base64_decode(substr($secret, strlen('whsec_')), true);
        return 'v1,' . base64_encode(hash_hmac('sha256', "$id.$ts.$body", (string) $key, true));
    }

    /** @return array<string, string> */
    private static function svixHeaders(?int $ts = null, string $id = 'msg_1'): array
    {
        $ts ??= time();
        return ['svix-id' => $id, 'svix-timestamp' => (string) $ts, 'svix-signature' => self::sign(self::secret(), $id, $ts, self::BODY)];
    }

    /** @param array<string, mixed> $headers */
    private function assertRefused(string $pattern, string $secret, array $headers, string $body, int $tolerance = 300): void
    {
        try {
            Webhook::verify($secret, $headers, $body, $tolerance);
            $this->fail('verification passed');
        } catch (WebhookVerificationException $e) {
            $this->assertMatchesRegularExpression($pattern, $e->getMessage());
        }
    }

    public function testSvixReferenceVector(): void
    {
        $headers = ['svix-id' => self::VECTOR_ID, 'svix-timestamp' => self::VECTOR_TS, 'svix-signature' => self::VECTOR_SIG];
        $this->assertSame(['test' => 2432232314], Webhook::verify(self::VECTOR_SECRET, $headers, self::VECTOR_BODY, self::FOREVER));
    }

    public function testSvixHeaders(): void
    {
        $event = Webhook::verify(self::secret(), self::svixHeaders(), self::BODY);
        $this->assertSame('email.delivered', $event['type']);
        $this->assertSame('e1', $event['data']['email_id']);
        $this->assertSame('email.delivered', verifyWebhook(self::secret(), self::svixHeaders(), self::BODY)['type']);
    }

    public function testWebhookHeadersInEveryShape(): void
    {
        $h = self::svixHeaders();
        // getallheaders()
        $plain = ['Webhook-Id' => $h['svix-id'], 'WEBHOOK-TIMESTAMP' => $h['svix-timestamp'], 'webhook-signature' => $h['svix-signature']];
        $this->assertSame('email.delivered', Webhook::verify(self::secret(), $plain, self::BODY)['type']);
        // PSR-7 getHeaders() and Symfony headers->all(): lists of values
        $lists = array_map(static fn (string $v): array => [$v], $plain);
        $this->assertSame('email.delivered', Webhook::verify(self::secret(), $lists, self::BODY)['type']);
        // $_SERVER
        $server = ['HTTP_SVIX_ID' => $h['svix-id'], 'HTTP_SVIX_TIMESTAMP' => $h['svix-timestamp'], 'HTTP_SVIX_SIGNATURE' => $h['svix-signature'], 'REQUEST_TIME' => 1];
        $this->assertSame('email.delivered', Webhook::verify(self::secret(), $server, self::BODY)['type']);
    }

    public function testSeveralSignatures(): void
    {
        $ts = time();
        $h = self::svixHeaders($ts);
        $other = self::sign(self::secret('rotated out'), 'msg_1', $ts, self::BODY);
        $h['svix-signature'] = "v2,ignored $other " . $h['svix-signature'];
        $this->assertSame('email.delivered', Webhook::verify(self::secret(), $h, self::BODY)['type']);
    }

    public function testRefusesForgeries(): void
    {
        $h = self::svixHeaders();
        $this->assertRefused('/no webhook signature matches/', self::secret(), $h, str_replace('delivered', 'bounced', self::BODY));
        $this->assertRefused('/no webhook signature matches/', self::secret('wrong'), $h, self::BODY);
        $this->assertRefused('/no webhook signature matches/', self::secret(), ['svix-signature' => 'v1,AAAA'] + $h, self::BODY);
        $this->assertRefused('/no webhook signature matches/', self::secret(), ['svix-id' => 'msg_2'] + $h, self::BODY);
    }

    public function testTimestamps(): void
    {
        $now = time();
        $this->assertRefused('/too old/', self::secret(), self::svixHeaders($now - 301), self::BODY);
        $this->assertRefused('/future/', self::secret(), self::svixHeaders($now + 301), self::BODY);
        $this->assertNotEmpty(Webhook::verify(self::secret(), self::svixHeaders($now - 290), self::BODY));
        $this->assertRefused('/too old/', self::secret(), self::svixHeaders($now - 20), self::BODY, 10);
        $this->assertNotEmpty(Webhook::verify(self::secret(), self::svixHeaders($now - 600), self::BODY, 900));
    }

    public function testMissingHeadersBadTimestampBadSecret(): void
    {
        $this->assertRefused('/missing webhook headers/', self::secret(), [], self::BODY);
        $h = self::svixHeaders();
        unset($h['svix-signature']);
        $this->assertRefused('/missing webhook headers/', self::secret(), $h, self::BODY);
        $this->assertRefused('/timestamp/', self::secret(), ['svix-timestamp' => 'soon'] + self::svixHeaders(), self::BODY);
        $this->assertRefused('/signing secret/', 'whsec_***', self::svixHeaders(), self::BODY);
    }
}
