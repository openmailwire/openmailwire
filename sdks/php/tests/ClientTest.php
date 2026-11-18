<?php

declare(strict_types=1);

namespace OpenMailWire\Tests;

use OpenMailWire\Client;
use OpenMailWire\OpenMailWireException;
use OpenMailWire\WaitTimeoutException;
use PHPUnit\Framework\TestCase;

final class ClientTest extends TestCase
{
    private const ID = 'ed01a077-1c2d-4e5f-8a9b-0c1d2e3f4a5b';
    private const KEY = 'omw_test_key';

    private static MockServer $mock;
    private Client $client;

    public static function setUpBeforeClass(): void
    {
        self::$mock = new MockServer();
    }

    public static function tearDownAfterClass(): void
    {
        self::$mock->stop();
    }

    protected function setUp(): void
    {
        self::$mock->reset();
        $this->client = new Client(self::KEY, self::$mock->url . '/');
    }

    /**
     * @param list<string> $statuses
     * @return array<string, mixed>
     */
    private static function email(array $statuses): array
    {
        $recipients = [];
        foreach ($statuses as $i => $status) {
            $recipients[] = ['email' => "r$i@example.test", 'status' => $status, 'attempts' => 0];
        }
        return [
            'object' => 'email', 'id' => self::ID, 'from' => 'a@acme.test', 'to' => ['x@example.test'],
            'subject' => 'hi', 'recipients' => $recipients, 'attempts' => [], 'events' => [],
        ];
    }

    // ------------------------------------------------------------ configuration

    public function testReadsEnvironment(): void
    {
        putenv('OMW_API_URL=' . self::$mock->url . '/');
        putenv('OMW_API_KEY=omw_from_env');
        try {
            $client = new Client();
            $this->assertSame(self::$mock->url, $client->baseUrl);
            self::$mock->json(['object' => 'list', 'data' => []]);
            $client->domains->list();
            $this->assertSame('Bearer omw_from_env', self::$mock->last()['headers']['authorization']);
        } finally {
            putenv('OMW_API_URL');
            putenv('OMW_API_KEY');
        }
    }

    public function testDefaultsAndMissingKey(): void
    {
        putenv('OMW_API_URL');
        putenv('OMW_API_KEY');
        unset($_ENV['OMW_API_URL'], $_ENV['OMW_API_KEY'], $_SERVER['OMW_API_URL'], $_SERVER['OMW_API_KEY']);
        $this->assertSame('http://localhost:8080', (new Client(self::KEY))->baseUrl);
        $this->expectException(\InvalidArgumentException::class);
        $this->expectExceptionMessage('OMW_API_KEY');
        new Client();
    }

    // ------------------------------------------------------------ emails

    public function testSend(): void
    {
        self::$mock->json(['id' => self::ID]);
        $res = $this->client->emails->send([
            'from' => 'Acme <hello@acme.test>',
            'to' => ['you@example.test'],
            'reply_to' => 'support@acme.test',
            'subject' => 'Your code',
            'text' => '482913',
            'html' => null,
            'headers' => [],
            'tags' => [['name' => 'kind', 'value' => 'otp']],
            'attachments' => [
                ['filename' => 'a.txt', 'content' => base64_encode('hello'), 'content_type' => 'text/plain'],
                ['filename' => 'logo.png', 'content' => 'iVBORw0K', 'content_id' => 'logo', 'content_type' => null],
            ],
            'scheduled_at' => new \DateTimeImmutable('2030-01-02T03:04:05Z'),
            'retry_uncertain' => false,
        ], idempotencyKey: 'signup-42');
        $this->assertSame(['id' => self::ID], $res);

        $r = self::$mock->last();
        $this->assertSame('POST', $r['method']);
        $this->assertSame('/emails', $r['path']);
        $this->assertSame('Bearer ' . self::KEY, $r['headers']['authorization']);
        $this->assertSame('application/json', $r['headers']['content-type']);
        $this->assertSame('signup-42', $r['headers']['idempotency-key']);
        $this->assertStringStartsWith('openmailwire-php/', $r['headers']['user-agent']);
        $this->assertSame([
            'from' => 'Acme <hello@acme.test>',
            'to' => ['you@example.test'],
            'reply_to' => 'support@acme.test',
            'subject' => 'Your code',
            'text' => '482913',
            'tags' => [['name' => 'kind', 'value' => 'otp']],
            'attachments' => [
                ['filename' => 'a.txt', 'content' => 'aGVsbG8=', 'content_type' => 'text/plain'],
                ['filename' => 'logo.png', 'content' => 'iVBORw0K', 'content_id' => 'logo'],
            ],
            'scheduled_at' => '2030-01-02T03:04:05+00:00',
            'retry_uncertain' => false,
        ], $r['json']);
    }

    public function testSendWithoutIdempotencyKey(): void
    {
        self::$mock->json(['id' => self::ID]);
        $this->client->emails->send(['from' => 'a@acme.test', 'to' => 'b@example.test', 'subject' => 's', 'text' => 'x']);
        $this->assertArrayNotHasKey('idempotency-key', self::$mock->last()['headers']);
    }

    public function testGetListCancel(): void
    {
        self::$mock->json(self::email(['delivered']));
        self::$mock->json(['object' => 'list', 'data' => [['id' => self::ID]], 'has_more' => true, 'next_before' => self::ID]);
        self::$mock->json(['object' => 'list', 'data' => [], 'has_more' => false, 'next_before' => null]);
        self::$mock->json(['object' => 'email', 'id' => self::ID]);

        $this->assertSame('delivered', $this->client->emails->get(self::ID)['recipients'][0]['status']);
        $this->assertSame('GET /emails/' . self::ID, self::$mock->last()['method'] . ' ' . self::$mock->last()['path']);

        $page = $this->client->emails->list(limit: 10, before: self::ID, apiKeyId: 'k1');
        $this->assertTrue($page['has_more']);
        $this->assertSame('limit=10&before=' . self::ID . '&api_key_id=k1', self::$mock->last()['query']);

        $this->client->emails->list();
        $this->assertSame('', self::$mock->last()['query']);

        $this->assertSame(self::ID, $this->client->emails->cancel(self::ID)['id']);
        $last = self::$mock->last();
        $this->assertSame('POST', $last['method']);
        $this->assertSame('/emails/' . self::ID . '/cancel', $last['path']);
        $this->assertSame('', $last['body']);
    }

    public function testBatch(): void
    {
        self::$mock->json(['data' => [['id' => '1'], ['id' => '2']]]);
        $res = $this->client->emails->batch([
            ['from' => 'a@acme.test', 'to' => 'x@example.test', 'subject' => '1', 'text' => 'one'],
            ['from' => 'a@acme.test', 'to' => ['y@example.test'], 'subject' => '2', 'text' => 'two'],
        ], 'nightly');
        $this->assertSame(['1', '2'], array_column($res['data'], 'id'));
        $r = self::$mock->last();
        $this->assertSame('/emails/batch', $r['path']);
        $this->assertSame('nightly', $r['headers']['idempotency-key']);
        $this->assertTrue(array_is_list($r['json']));
        $this->assertSame('2', $r['json'][1]['subject']);
    }

    public function testSendRaw(): void
    {
        self::$mock->json(['id' => self::ID]);
        self::$mock->json(['id' => self::ID]);
        $raw = "From: a@acme.test\r\nTo: x@example.test\r\nSubject: hi\r\n\r\nbody\r\n";
        $this->client->emails->sendRaw($raw, 'a@acme.test', ['x@example.test'], 'r1');
        $r = self::$mock->last();
        $this->assertSame('/emails/raw', $r['path']);
        $this->assertSame('r1', $r['headers']['idempotency-key']);
        $this->assertSame(['raw' => base64_encode($raw), 'mail_from' => 'a@acme.test', 'rcpts' => ['x@example.test']], $r['json']);

        $this->client->emails->sendRaw($raw);
        $this->assertSame(['raw' => base64_encode($raw)], self::$mock->last()['json']);
    }

    // ------------------------------------------------------------ domains, webhooks, keys

    public function testDomains(): void
    {
        $dom = ['object' => 'domain', 'id' => self::ID, 'name' => 'acme.test', 'status' => 'pending', 'records' => []];
        self::$mock->json($dom, 201);
        self::$mock->json(['object' => 'list', 'data' => [$dom]]);
        self::$mock->json($dom);
        self::$mock->json(['status' => 'verified'] + $dom);
        self::$mock->json(['object' => 'domain', 'id' => self::ID, 'deleted' => true]);

        $this->assertSame('acme.test', $this->client->domains->create('acme.test')['name']);
        $this->assertSame(['name' => 'acme.test'], self::$mock->last()['json']);
        $this->assertCount(1, $this->client->domains->list()['data']);
        $this->assertSame('GET /domains', self::$mock->last()['method'] . ' ' . self::$mock->last()['path']);
        $this->assertSame(self::ID, $this->client->domains->get(self::ID)['id']);
        $this->assertSame('verified', $this->client->domains->verify(self::ID)['status']);
        $this->assertSame('POST /domains/' . self::ID . '/verify', self::$mock->last()['method'] . ' ' . self::$mock->last()['path']);
        $this->assertTrue($this->client->domains->delete(self::ID)['deleted']);
        $this->assertSame('DELETE /domains/' . self::ID, self::$mock->last()['method'] . ' ' . self::$mock->last()['path']);
    }

    public function testWebhooks(): void
    {
        $hook = ['object' => 'webhook', 'id' => self::ID, 'endpoint' => 'https://h.test/x', 'events' => ['email.bounced']];
        self::$mock->json(['signing_secret' => 'whsec_abc'] + $hook, 201);
        self::$mock->json($hook, 201);
        self::$mock->json(['object' => 'list', 'data' => [$hook]]);
        self::$mock->json(['recent_deliveries' => []] + $hook);
        self::$mock->json(['object' => 'webhook', 'id' => self::ID, 'deleted' => true]);

        $this->assertSame('whsec_abc', $this->client->webhooks->create('https://h.test/x', ['email.bounced'])['signing_secret']);
        $this->assertSame(['endpoint' => 'https://h.test/x', 'events' => ['email.bounced']], self::$mock->last()['json']);
        $this->client->webhooks->create('https://h.test/all');
        $this->assertSame(['endpoint' => 'https://h.test/all'], self::$mock->last()['json']);
        $this->assertSame(self::ID, $this->client->webhooks->list()['data'][0]['id']);
        $this->assertSame([], $this->client->webhooks->get(self::ID)['recent_deliveries']);
        $this->assertSame('/webhooks/' . self::ID, self::$mock->last()['path']);
        $this->assertTrue($this->client->webhooks->delete(self::ID)['deleted']);
        $this->assertSame('DELETE', self::$mock->last()['method']);
    }

    public function testApiKeys(): void
    {
        $key = ['id' => self::ID, 'name' => 'agent', 'prefix' => 'omw_ab', 'permission' => 'sending_access', 'policy' => ['max_per_hour' => 10]];
        self::$mock->json(['token' => 'omw_secret'] + $key, 201);
        self::$mock->json(['object' => 'list', 'data' => [$key]]);
        self::$mock->json($key);
        self::$mock->json(['name' => 'renamed'] + $key);
        self::$mock->json($key);
        self::$mock->json(['object' => 'api_key', 'id' => self::ID, 'deleted' => true]);

        $policy = ['max_per_hour' => 10, 'allowed_recipients' => ['*@acme.test']];
        $created = $this->client->apiKeys->create('agent', 'sending_access', $policy);
        $this->assertSame('omw_secret', $created['token']);
        $this->assertSame(['name' => 'agent', 'permission' => 'sending_access', 'policy' => $policy], self::$mock->last()['json']);
        $this->assertSame('omw_ab', $this->client->apiKeys->list()['data'][0]['prefix']);
        $this->assertSame('agent', $this->client->apiKeys->get(self::ID)['name']);
        $this->assertSame('renamed', $this->client->apiKeys->update(self::ID, name: 'renamed')['name']);
        $last = self::$mock->last();
        $this->assertSame('PATCH /api-keys/' . self::ID, $last['method'] . ' ' . $last['path']);
        $this->assertSame('{"name":"renamed"}', $last['body']);
        $this->client->apiKeys->update(self::ID, policy: []);
        $this->assertSame('{"policy":{}}', self::$mock->last()['body']);
        $this->assertTrue($this->client->apiKeys->delete(self::ID)['deleted']);
    }

    // ------------------------------------------------------------ inboxes and messages

    public function testInboxes(): void
    {
        $inbox = ['id' => 'i1', 'address' => 'agent@acme.test', 'created_at' => '2026-01-01T00:00:00Z'];
        self::$mock->json($inbox, 201);
        self::$mock->json(['object' => 'list', 'data' => [$inbox]]);
        self::$mock->json(['data' => [['id' => 'm1']], 'has_more' => false, 'next_before' => null]);
        self::$mock->json(['object' => 'inbox', 'id' => 'i1', 'deleted' => true]);

        $this->assertSame('agent@acme.test', $this->client->inboxes->create('agent@acme.test')['address']);
        $this->assertSame(['address' => 'agent@acme.test'], self::$mock->last()['json']);
        $this->assertCount(1, $this->client->inboxes->list()['data']);
        $page = $this->client->inboxes->listMessages('i1', limit: 5, threadId: 't1');
        $this->assertSame('m1', $page['data'][0]['id']);
        $this->assertSame('/inboxes/i1/messages', self::$mock->last()['path']);
        $this->assertSame('limit=5&thread_id=t1', self::$mock->last()['query']);
        $this->assertTrue($this->client->inboxes->delete('i1')['deleted']);
        $this->assertSame('DELETE /inboxes/i1', self::$mock->last()['method'] . ' ' . self::$mock->last()['path']);
    }

    public function testMessages(): void
    {
        $blob = "\x00\x01\x02\xfa\xff";
        self::$mock->json(['id' => 'm1', 'subject' => 'Re: hi', 'attachments' => []]);
        self::$mock->enqueue(['status' => 200, 'base64' => base64_encode($blob)]);
        self::$mock->json(['id' => self::ID]);

        $this->assertSame('Re: hi', $this->client->messages->get('m1')['subject']);
        $this->assertSame($blob, $this->client->messages->getAttachment('m1', 0));
        $this->assertSame('/messages/m1/attachments/0', self::$mock->last()['path']);
        $this->assertSame(['id' => self::ID], $this->client->messages->reply('m1', text: 'thanks'));
        $last = self::$mock->last();
        $this->assertSame('POST /messages/m1/reply', $last['method'] . ' ' . $last['path']);
        $this->assertSame(['text' => 'thanks'], $last['json']);
    }

    // ------------------------------------------------------------ errors

    public function testApiError(): void
    {
        self::$mock->json(['statusCode' => 422, 'name' => 'validation_error', 'message' => 'subject is required'], 422);
        try {
            $this->client->emails->send(['from' => 'a@acme.test', 'to' => 'b@example.test', 'subject' => '', 'text' => 'x']);
            $this->fail('no exception');
        } catch (OpenMailWireException $e) {
            $this->assertSame(422, $e->statusCode);
            $this->assertSame('validation_error', $e->name);
            $this->assertSame('subject is required', $e->getMessage());
            $this->assertNull($e->retryAfter);
        }
    }

    public function testRateLimitRetryAfter(): void
    {
        self::$mock->enqueue([
            'status' => 429,
            'headers' => ['Retry-After' => '120'],
            'json' => ['statusCode' => 429, 'name' => 'rate_limit_exceeded', 'message' => 'hourly cap reached'],
        ]);
        try {
            $this->client->emails->send(['from' => 'a@acme.test', 'to' => 'b@example.test', 'subject' => 's', 'text' => 'x']);
            $this->fail('no exception');
        } catch (OpenMailWireException $e) {
            $this->assertSame(429, $e->statusCode);
            $this->assertSame('rate_limit_exceeded', $e->name);
            $this->assertSame(120, $e->retryAfter);
        }
    }

    public function testNonJsonError(): void
    {
        self::$mock->enqueue(['status' => 502, 'text' => 'Bad Gateway']);
        try {
            $this->client->domains->list();
            $this->fail('no exception');
        } catch (OpenMailWireException $e) {
            $this->assertSame(502, $e->statusCode);
            $this->assertSame('http_error', $e->name);
            $this->assertSame('Bad Gateway', $e->getMessage());
        }
    }

    public function testConnectionError(): void
    {
        $client = new Client(self::KEY, 'http://127.0.0.1:' . MockServer::freePort(), 5.0);
        try {
            $client->domains->list();
            $this->fail('no exception');
        } catch (OpenMailWireException $e) {
            $this->assertSame(0, $e->statusCode);
            $this->assertSame('connection_error', $e->name);
        }
    }

    // ------------------------------------------------------------ waiting

    public function testWaitForDelivery(): void
    {
        self::$mock->json(self::email(['queued', 'queued']));
        self::$mock->json(self::email(['sending', 'deferred']));
        self::$mock->json(self::email(['delivered', 'bounced']));
        $email = $this->client->waitForDelivery(self::ID, 5.0, 0.01);
        $this->assertSame(['delivered', 'bounced'], array_column($email['recipients'], 'status'));
        $requests = self::$mock->requests();
        $this->assertCount(3, $requests);
        foreach ($requests as $r) {
            $this->assertSame('/emails/' . self::ID, $r['path']);
        }
    }

    public function testWaitForDeliveryTimeout(): void
    {
        self::$mock->enqueue(['status' => 200, 'json' => self::email(['deferred']), 'sticky' => true]);
        try {
            $this->client->waitForDelivery(self::ID, 0.05, 0.01);
            $this->fail('no exception');
        } catch (WaitTimeoutException $e) {
            $this->assertSame('deferred', $e->email['recipients'][0]['status']);
        }
    }

    public function testWaitForReply(): void
    {
        $inboxes = ['object' => 'list', 'data' => [['id' => 'i1'], ['id' => 'i2']]];
        $other = ['data' => [['id' => 'm0', 'in_reply_to_email_id' => 'other']], 'has_more' => false];
        // First poll: no reply yet. Second poll: two replies.
        self::$mock->json($inboxes);
        self::$mock->json($other);
        self::$mock->json(['data' => [], 'has_more' => false]);
        self::$mock->json($inboxes);
        self::$mock->json($other);
        self::$mock->json(['data' => [
            ['id' => 'm1', 'in_reply_to_email_id' => self::ID, 'received_at' => '2026-01-01T00:00:00Z'],
            ['id' => 'm2', 'in_reply_to_email_id' => self::ID, 'received_at' => '2026-01-01T01:00:00Z'],
        ], 'has_more' => false]);

        $replies = $this->client->waitForReply(self::ID, 5.0, 0.01);
        $this->assertSame(['m2', 'm1'], array_column($replies, 'id'));
        $requests = self::$mock->requests();
        $this->assertSame('/inboxes/i2/messages', $requests[2]['path']);
        $this->assertSame('limit=50', $requests[2]['query']);
    }

    public function testWaitForReplyTimeoutOneInbox(): void
    {
        self::$mock->enqueue(['status' => 200, 'json' => ['data' => [], 'has_more' => false], 'sticky' => true]);
        $this->assertSame([], $this->client->waitForReply(self::ID, 0.05, 0.01, 'i9'));
        foreach (self::$mock->requests() as $r) {
            $this->assertSame('/inboxes/i9/messages', $r['path']);
        }
    }
}
