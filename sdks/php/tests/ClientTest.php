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

    // ------------------------------------------------------------ v0.6: approvals, checks, replies

    public function testSendStreamAndHeld(): void
    {
        self::$mock->json(['id' => self::ID, 'held' => true, 'hold_reasons' => ['first_contact']]);
        $res = $this->client->emails->send(['from' => 'a@acme.test', 'to' => 'b@example.test', 'subject' => 's', 'text' => 'x', 'stream' => 'broadcast']);
        $this->assertTrue($res['held']);
        $this->assertSame(['first_contact'], $res['hold_reasons']);
        $this->assertSame('broadcast', self::$mock->last()['json']['stream']);
    }

    public function testCheck(): void
    {
        $result = [
            'object' => 'email_check', 'ok' => false, 'would' => 'hold', 'stream' => 'transactional', 'size' => 812,
            'hold_reasons' => ['has_links'], 'refusals' => [], 'warnings' => [['code' => 'no_text_part', 'message' => 'add text']],
        ];
        self::$mock->json($result);
        $res = $this->client->emails->check([
            'from' => 'a@acme.test', 'to' => 'b@example.test', 'subject' => 's', 'html' => '<a href="https://x.test">x</a>', 'text' => null,
            'scheduled_at' => new \DateTimeImmutable('2030-01-02T03:04:05Z'),
        ]);
        $this->assertSame($result, $res);
        $last = self::$mock->last();
        $this->assertSame('POST /emails/check', $last['method'] . ' ' . $last['path']);
        $this->assertSame([
            'from' => 'a@acme.test', 'to' => 'b@example.test', 'subject' => 's', 'html' => '<a href="https://x.test">x</a>',
            'scheduled_at' => '2030-01-02T03:04:05+00:00',
        ], $last['json']);
    }

    public function testHeldApproveRejectReplies(): void
    {
        $decision = static fn (string $d): array => ['object' => 'email', 'id' => self::ID, 'decision' => $d, 'recipients' => ['b@example.test'], 'suppressed' => []];
        self::$mock->json(['object' => 'list', 'data' => [['object' => 'held_email', 'id' => self::ID]], 'has_more' => false, 'next_before' => null]);
        self::$mock->json(['object' => 'list', 'data' => [], 'has_more' => false, 'next_before' => null]);
        self::$mock->json($decision('approved'));
        self::$mock->json($decision('approved'));
        self::$mock->json($decision('rejected'));
        self::$mock->json(['object' => 'list', 'email_id' => self::ID, 'thread_id' => 't1', 'data' => [['id' => 'm1', 'trust' => 'authenticated']]]);

        $this->assertSame('held_email', $this->client->emails->listHeld(5, 'e0')['data'][0]['object']);
        $this->assertSame('GET /emails/held', self::$mock->last()['method'] . ' ' . self::$mock->last()['path']);
        $this->assertSame('limit=5&before=e0', self::$mock->last()['query']);
        $this->client->emails->listHeld();
        $this->assertSame('', self::$mock->last()['query']);

        $this->assertSame('approved', $this->client->emails->approve(self::ID, 'looks fine')['decision']);
        $last = self::$mock->last();
        $this->assertSame('POST /emails/' . self::ID . '/approve', $last['method'] . ' ' . $last['path']);
        $this->assertSame('{"note":"looks fine"}', $last['body']);
        $this->client->emails->approve(self::ID);
        $this->assertSame('{}', self::$mock->last()['body']);
        $this->assertSame('rejected', $this->client->emails->reject(self::ID, note: 'wrong customer')['decision']);
        $last = self::$mock->last();
        $this->assertSame('POST /emails/' . self::ID . '/reject', $last['method'] . ' ' . $last['path']);
        $this->assertSame('{"note":"wrong customer"}', $last['body']);

        $replies = $this->client->emails->replies(self::ID);
        $this->assertSame('t1', $replies['thread_id']);
        $this->assertSame('authenticated', $replies['data'][0]['trust']);
        $this->assertSame('GET /emails/' . self::ID . '/replies', self::$mock->last()['method'] . ' ' . self::$mock->last()['path']);
    }

    // ------------------------------------------------------------ v0.6: suppressions, pauses, project

    public function testSuppressions(): void
    {
        $row = ['address' => 'x@example.test', 'stream' => 'all', 'reason' => 'manual', 'detail' => '', 'created_at' => '2026-01-01T00:00:00Z'];
        $csv = "address,stream,reason,detail,created_at\nx@example.test,all,manual,,2026-01-01T00:00:00Z\n";
        self::$mock->json(['object' => 'list', 'data' => [$row], 'has_more' => true, 'next_before' => 'c1']);
        self::$mock->json(['object' => 'suppression', 'created' => true] + $row, 201);
        self::$mock->json(['object' => 'suppression', 'created' => false] + $row);
        self::$mock->json(['object' => 'suppression', 'address' => 'x@example.test', 'deleted' => true, 'removed' => [$row]]);
        self::$mock->json(['object' => 'suppression', 'address' => 'x@example.test', 'deleted' => true, 'removed' => [$row]]);
        self::$mock->enqueue(['status' => 200, 'text' => $csv, 'headers' => ['Content-Type' => 'text/csv']]);
        self::$mock->enqueue(['status' => 200, 'text' => $csv, 'headers' => ['Content-Type' => 'text/csv']]);

        $page = $this->client->suppressions->list(2, 'c0', 'hard_bounce', 'broadcast', 'x@example.test', 'example.test');
        $this->assertSame('c1', $page['next_before']);
        $this->assertSame('GET /suppressions', self::$mock->last()['method'] . ' ' . self::$mock->last()['path']);
        $this->assertSame('limit=2&before=c0&reason=hard_bounce&stream=broadcast&address=x%40example.test&domain=example.test', self::$mock->last()['query']);

        $this->assertTrue($this->client->suppressions->create('x@example.test', 'customer_request', 'by phone', 'all')['created']);
        $last = self::$mock->last();
        $this->assertSame('POST /suppressions', $last['method'] . ' ' . $last['path']);
        $this->assertSame(['address' => 'x@example.test', 'reason' => 'customer_request', 'detail' => 'by phone', 'stream' => 'all'], $last['json']);
        $this->assertFalse($this->client->suppressions->create('x@example.test')['created']);
        $this->assertSame(['address' => 'x@example.test'], self::$mock->last()['json']);

        $this->assertCount(1, $this->client->suppressions->delete('x@example.test', 'broadcast')['removed']);
        $last = self::$mock->last();
        $this->assertSame('DELETE /suppressions/x%40example.test', $last['method'] . ' ' . $last['path']);
        $this->assertSame('stream=broadcast', $last['query']);
        $this->client->suppressions->delete('x@example.test');
        $this->assertSame('', self::$mock->last()['query']);

        $this->assertSame($csv, $this->client->suppressions->export(reason: 'complaint'));
        $last = self::$mock->last();
        $this->assertSame('GET /suppressions/export', $last['method'] . ' ' . $last['path']);
        $this->assertSame('reason=complaint', $last['query']);
        $this->assertStringContainsString('text/csv', $last['headers']['accept']);
        $this->assertSame($csv, $this->client->suppressions->export());
        $this->assertSame('', self::$mock->last()['query']);
    }

    public function testPauses(): void
    {
        $pause = ['object' => 'pause', 'id' => 'p1', 'scope' => 'domain', 'scope_id' => 'd1', 'reason' => 'bounce_rate', 'rate' => 0.09, 'threshold' => 0.05, 'active' => true];
        self::$mock->json(['object' => 'list', 'data' => [$pause]]);
        self::$mock->json(['object' => 'list', 'data' => []]);
        self::$mock->json(['active' => false, 'resumed_by' => 'k1'] + $pause);

        $this->assertSame('domain', $this->client->pauses->list(true, 10)['data'][0]['scope']);
        $this->assertSame('GET /pauses', self::$mock->last()['method'] . ' ' . self::$mock->last()['path']);
        $this->assertSame('active=true&limit=10', self::$mock->last()['query']);
        $this->client->pauses->list(active: false);
        $this->assertSame('active=false', self::$mock->last()['query']);
        $this->assertFalse($this->client->pauses->resume('p1')['active']);
        $last = self::$mock->last();
        $this->assertSame('POST /pauses/p1/resume', $last['method'] . ' ' . $last['path']);
        $this->assertSame('', $last['body']);
    }

    public function testProject(): void
    {
        $project = ['object' => 'project', 'id' => 'pr1', 'name' => 'acme', 'max_per_recipient_per_day' => 5, 'circuit_breaker' => ['enabled' => true]];
        self::$mock->json($project);
        self::$mock->json(['max_per_recipient_per_day' => 3] + $project);
        self::$mock->json(['max_per_recipient_per_day' => null] + $project);
        self::$mock->json($project);

        $this->assertSame(5, $this->client->project->get()['max_per_recipient_per_day']);
        $this->assertSame('GET /project', self::$mock->last()['method'] . ' ' . self::$mock->last()['path']);

        $this->assertSame(3, $this->client->project->update(3, ['bounce_rate' => 0.02, 'min_recipients' => 100])['max_per_recipient_per_day']);
        $last = self::$mock->last();
        $this->assertSame('PATCH /project', $last['method'] . ' ' . $last['path']);
        $this->assertSame(['max_per_recipient_per_day' => 3, 'circuit_breaker' => ['bounce_rate' => 0.02, 'min_recipients' => 100]], $last['json']);

        $this->assertNull($this->client->project->update(clearMaxPerRecipientPerDay: true)['max_per_recipient_per_day']);
        $this->assertSame('{"max_per_recipient_per_day":null}', self::$mock->last()['body']);
        $this->client->project->update(circuitBreaker: ['enabled' => false]);
        $this->assertSame('{"circuit_breaker":{"enabled":false}}', self::$mock->last()['body']);
    }

    // ------------------------------------------------------------ v0.6: audit log, insights

    public function testAuditLog(): void
    {
        self::$mock->json(['object' => 'list', 'data' => [['object' => 'audit_entry', 'id' => 7, 'action' => 'api_key.created']], 'has_more' => true, 'next_before' => '7']);
        self::$mock->json(['object' => 'audit_verification', 'valid' => true, 'entries' => 7, 'head_hash' => 'ab', 'first_invalid_id' => null]);

        $this->assertSame(7, $this->client->auditLog->list(1, 9, 'api_key.created')['data'][0]['id']);
        $this->assertSame('GET /audit-log', self::$mock->last()['method'] . ' ' . self::$mock->last()['path']);
        $this->assertSame('limit=1&before=9&action=api_key.created', self::$mock->last()['query']);
        $verification = $this->client->auditLog->verify();
        $this->assertTrue($verification['valid']);
        $this->assertSame('ab', $verification['head_hash']);
        $this->assertSame('GET /audit-log/verify', self::$mock->last()['method'] . ' ' . self::$mock->last()['path']);
    }

    public function testInsights(): void
    {
        $window = static fn (string $name): array => ['window' => $name, 'total' => ['attempts' => 1], 'by_provider' => [], 'by_sending_ip' => []];
        $body = ['object' => 'insights', 'emails_considered' => 1, 'truncated' => false, 'windows' => [$window('24h'), $window('7d'), $window('30d')]];
        self::$mock->json($body);
        self::$mock->json($body);

        $this->assertSame(['24h', '7d', '30d'], array_column($this->client->insights->get()['windows'], 'window'));
        $this->assertSame('GET /insights', self::$mock->last()['method'] . ' ' . self::$mock->last()['path']);
        $this->assertSame(['7d'], array_column($this->client->insights->get('7d')['windows'], 'window'));
        $this->assertSame('', self::$mock->last()['query']);
    }

    // ------------------------------------------------------------ v0.6: domains, webhooks, keys, inboxes

    public function testDomainDkimAndReports(): void
    {
        $state = static fn (string $object, ?string $status = null): array => [
            'object' => $object, 'status' => $status, 'keys' => [['selector' => 'omw2', 'algorithm' => 'rsa', 'state' => 'pending', 'signs' => false, 'record' => []]],
        ];
        self::$mock->json($state('dkim'));
        self::$mock->json($state('dkim_rotation', 'pending'), 202);
        self::$mock->json($state('dkim_ed25519', 'active'));
        self::$mock->json($state('dkim_ed25519', 'disabled'));
        self::$mock->json(['object' => 'domain_reports', 'domain_id' => self::ID, 'domain' => 'acme.test', 'days' => 7, 'dmarc' => ['pass' => 3], 'tls' => [], 'reports' => []]);
        self::$mock->json(['object' => 'domain_reports', 'domain_id' => self::ID, 'domain' => 'acme.test', 'days' => 30, 'dmarc' => [], 'tls' => [], 'reports' => []]);
        self::$mock->json(['object' => 'domain', 'id' => self::ID, 'claim_expires_at' => null, 'report_addresses' => ['dmarc_rua' => 'mailto:d@rp.acme.test']]);

        $this->assertSame('omw2', $this->client->domains->dkim(self::ID)['keys'][0]['selector']);
        $this->assertSame('GET /domains/' . self::ID . '/dkim', self::$mock->last()['method'] . ' ' . self::$mock->last()['path']);
        $this->assertSame('pending', $this->client->domains->rotateDkim(self::ID)['status']);
        $last = self::$mock->last();
        $this->assertSame('POST /domains/' . self::ID . '/dkim/rotate', $last['method'] . ' ' . $last['path']);
        $this->assertSame('', $last['body']);
        $this->assertSame('active', $this->client->domains->setEd25519(self::ID, true)['status']);
        $last = self::$mock->last();
        $this->assertSame('POST /domains/' . self::ID . '/dkim/ed25519', $last['method'] . ' ' . $last['path']);
        $this->assertSame('{"enabled":true}', $last['body']);
        $this->assertSame('disabled', $this->client->domains->setEd25519(self::ID, false)['status']);
        $this->assertSame('{"enabled":false}', self::$mock->last()['body']);
        $this->assertSame(3, $this->client->domains->reports(self::ID, 7)['dmarc']['pass']);
        $this->assertSame('GET /domains/' . self::ID . '/reports', self::$mock->last()['method'] . ' ' . self::$mock->last()['path']);
        $this->assertSame('days=7', self::$mock->last()['query']);
        $this->client->domains->reports(self::ID);
        $this->assertSame('', self::$mock->last()['query']);
        $domain = $this->client->domains->get(self::ID);
        $this->assertNull($domain['claim_expires_at']);
        $this->assertSame('mailto:d@rp.acme.test', $domain['report_addresses']['dmarc_rua']);
    }

    public function testWebhookEd25519AndRotation(): void
    {
        $hook = ['object' => 'webhook', 'id' => self::ID, 'endpoint' => 'https://h.test/x', 'asymmetric_signing' => true, 'public_key' => 'whpk_abc'];
        self::$mock->json(['signing_secret' => 'whsec_1'] + $hook, 201);
        self::$mock->json($hook);
        self::$mock->json(['signing_secret' => 'whsec_2', 'previous_public_key' => 'whpk_abc'] + $hook);
        self::$mock->json(['signing_secret' => 'whsec_3'] + $hook);

        $this->client->webhooks->create('https://h.test/x', ['email.held', 'sending.paused'], true);
        $this->assertSame(['endpoint' => 'https://h.test/x', 'events' => ['email.held', 'sending.paused'], 'asymmetric_signing' => true], self::$mock->last()['json']);
        $this->assertSame('whpk_abc', $this->client->webhooks->get(self::ID)['public_key']);
        $this->assertSame('whsec_2', $this->client->webhooks->rotateSecret(self::ID, 0)['signing_secret']);
        $last = self::$mock->last();
        $this->assertSame('POST /webhooks/' . self::ID . '/rotate-secret', $last['method'] . ' ' . $last['path']);
        $this->assertSame('{"overlap_seconds":0}', $last['body']);
        $this->client->webhooks->rotateSecret(self::ID);
        $this->assertSame('{}', self::$mock->last()['body']);
        $this->assertContains('sending.paused', \OpenMailWire\Webhook::EVENTS);
        $this->assertContains('email.held', \OpenMailWire\Webhook::EVENTS);
    }

    public function testApiKeyIpsExpiryAndApprovalPolicy(): void
    {
        $key = ['id' => self::ID, 'name' => 'agent', 'allowed_ips' => ['10.0.0.0/8'], 'expires_at' => '2031-01-01T00:00:00Z'];
        self::$mock->json(['token' => 'omw_secret'] + $key, 201);
        self::$mock->json($key);
        self::$mock->json($key);
        self::$mock->json($key);

        $policy = ['require_approval' => ['first_contact' => true, 'recipients_over' => 5], 'can_approve' => false, 'inboxes' => ['support@acme.test']];
        $created = $this->client->apiKeys->create('agent', 'sending_access', $policy, ['10.0.0.0/8'], new \DateTimeImmutable('2031-01-01T00:00:00Z'));
        $this->assertSame('omw_secret', $created['token']);
        $this->assertSame([
            'name' => 'agent',
            'permission' => 'sending_access',
            'policy' => $policy,
            'allowed_ips' => ['10.0.0.0/8'],
            'expires_at' => '2031-01-01T00:00:00+00:00',
        ], self::$mock->last()['json']);

        $this->client->apiKeys->update(self::ID, clearAllowedIps: true, clearExpiresAt: true);
        $last = self::$mock->last();
        $this->assertSame('PATCH /api-keys/' . self::ID, $last['method'] . ' ' . $last['path']);
        $this->assertSame('{"allowed_ips":null,"expires_at":null}', $last['body']);
        $this->client->apiKeys->update(self::ID, allowedIps: ['203.0.113.7'], expiresAt: '2032-01-01T00:00:00Z');
        $this->assertSame('{"allowed_ips":["203.0.113.7"],"expires_at":"2032-01-01T00:00:00Z"}', self::$mock->last()['body']);
        $this->client->apiKeys->update(self::ID, name: 'renamed');
        $this->assertSame('{"name":"renamed"}', self::$mock->last()['body']);
    }

    public function testInboxGetUpdateAndHold(): void
    {
        $inbox = ['object' => 'inbox', 'id' => 'i1', 'address' => 'agent@acme.test', 'hold_unauthenticated' => true];
        self::$mock->json(['hold_unauthenticated' => false] + $inbox, 201);
        self::$mock->json($inbox);
        self::$mock->json(['hold_unauthenticated' => false] + $inbox);

        $this->assertFalse($this->client->inboxes->create('agent@acme.test', false)['hold_unauthenticated']);
        $this->assertSame(['address' => 'agent@acme.test', 'hold_unauthenticated' => false], self::$mock->last()['json']);
        $this->assertTrue($this->client->inboxes->get('i1')['hold_unauthenticated']);
        $this->assertSame('GET /inboxes/i1', self::$mock->last()['method'] . ' ' . self::$mock->last()['path']);
        $this->assertFalse($this->client->inboxes->update('i1', false)['hold_unauthenticated']);
        $last = self::$mock->last();
        $this->assertSame('PATCH /inboxes/i1', $last['method'] . ' ' . $last['path']);
        $this->assertSame('{"hold_unauthenticated":false}', $last['body']);
    }

    public function testMessageTrustReleaseAndReplyTo(): void
    {
        $message = ['id' => 'm1', 'trust' => 'unauthenticated', 'held' => true, 'suspicious_instructions' => true, 'authentication_results' => ['dmarc' => ['result' => 'none']]];
        self::$mock->json($message);
        self::$mock->json(['held' => false] + $message);
        self::$mock->json(['id' => self::ID, 'thread_id' => 't1', 'to' => ['a@example.test', 'b@example.test'], 'warnings' => ['other domain']]);

        $m = $this->client->messages->get('m1');
        $this->assertSame(['unauthenticated', true, true], [$m['trust'], $m['held'], $m['suspicious_instructions']]);
        $this->assertFalse($this->client->messages->release('m1')['held']);
        $last = self::$mock->last();
        $this->assertSame('POST /messages/m1/release', $last['method'] . ' ' . $last['path']);
        $this->assertSame('', $last['body']);
        $sent = $this->client->messages->reply('m1', text: 'thanks', to: ['a@example.test', 'b@example.test']);
        $this->assertSame(['a@example.test', 'b@example.test'], $sent['to']);
        $this->assertSame(['other domain'], $sent['warnings']);
        $this->assertSame(['text' => 'thanks', 'to' => ['a@example.test', 'b@example.test']], self::$mock->last()['json']);
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

    public function testRetryAfterOnRecipientCapAndServerBusy(): void
    {
        foreach ([[429, OpenMailWireException::RECIPIENT_RATE_LIMITED], [503, OpenMailWireException::SERVER_BUSY]] as [$status, $name]) {
            self::$mock->reset();
            self::$mock->enqueue(['status' => $status, 'headers' => ['Retry-After' => '10'], 'json' => ['statusCode' => $status, 'name' => $name, 'message' => 'wait']]);
            try {
                $this->client->emails->send(['from' => 'a@acme.test', 'to' => 'b@example.test', 'subject' => 's', 'text' => 'x']);
                $this->fail('no exception');
            } catch (OpenMailWireException $e) {
                $this->assertSame($status, $e->statusCode);
                $this->assertSame($name, $e->name);
                $this->assertSame(10, $e->retryAfter);
                $this->assertTrue($e->isRetryable());
            }
            $this->assertCount(1, self::$mock->requests()); // no retry by default
        }
        $this->assertFalse((new OpenMailWireException(422, 'validation_error', 'x'))->isRetryable());
    }

    public function testErrorNames(): void
    {
        $this->assertSame('sending_paused', OpenMailWireException::SENDING_PAUSED);
        $this->assertSame('insufficient_permission', OpenMailWireException::INSUFFICIENT_PERMISSION);
        $this->assertSame('cannot_approve_own_email', OpenMailWireException::CANNOT_APPROVE_OWN_EMAIL);
        $this->assertSame('server_busy', OpenMailWireException::SERVER_BUSY);
        $this->assertSame('domain_verified_elsewhere', OpenMailWireException::DOMAIN_VERIFIED_ELSEWHERE);
        $this->assertSame('ip_not_allowed', OpenMailWireException::IP_NOT_ALLOWED);
        $this->assertSame('api_key_expired', OpenMailWireException::API_KEY_EXPIRED);
        $this->assertSame('webhook_limit_reached', OpenMailWireException::WEBHOOK_LIMIT_REACHED);
        $this->assertSame('recipient_rate_limited', OpenMailWireException::RECIPIENT_RATE_LIMITED);
    }

    public function testMaxRetries(): void
    {
        self::$mock->enqueue(['status' => 503, 'headers' => ['Retry-After' => '0'], 'json' => ['statusCode' => 503, 'name' => 'server_busy', 'message' => 'busy']]);
        self::$mock->enqueue(['status' => 429, 'headers' => ['Retry-After' => '0'], 'json' => ['statusCode' => 429, 'name' => 'rate_limit_exceeded', 'message' => 'slow']]);
        self::$mock->json(['id' => self::ID]);
        $client = new Client(self::KEY, self::$mock->url, maxRetries: 2);
        $res = $client->emails->send(['from' => 'a@acme.test', 'to' => 'b@example.test', 'subject' => 's', 'text' => 'x'], 'k1');
        $this->assertSame(['id' => self::ID], $res);
        $requests = self::$mock->requests();
        $this->assertCount(3, $requests);
        foreach ($requests as $r) {
            $this->assertSame('k1', $r['headers']['idempotency-key']);
            $this->assertSame('s', $r['json']['subject']);
        }
    }

    public function testMaxRetriesGivesUp(): void
    {
        $busy = ['statusCode' => 503, 'name' => 'server_busy', 'message' => 'busy'];
        self::$mock->enqueue(['status' => 503, 'headers' => ['Retry-After' => '0'], 'json' => $busy, 'sticky' => true]);
        try {
            (new Client(self::KEY, self::$mock->url, maxRetries: 1))->domains->list();
            $this->fail('no exception');
        } catch (OpenMailWireException $e) {
            $this->assertSame('server_busy', $e->name);
        }
        $this->assertCount(2, self::$mock->requests());

        self::$mock->reset();
        self::$mock->enqueue(['status' => 503, 'headers' => ['Retry-After' => '3600'], 'json' => $busy, 'sticky' => true]);
        try {
            (new Client(self::KEY, self::$mock->url, maxRetries: 3, maxRetryWait: 5.0))->domains->list();
            $this->fail('no exception');
        } catch (OpenMailWireException $e) {
            $this->assertSame(3600, $e->retryAfter);
        }
        $this->assertCount(1, self::$mock->requests());
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

    public function testRedirectsAreRefused(): void
    {
        // R4#10: the Python SDK forwarded Authorization on redirects; this one
        // never follows a redirect at all.
        self::$mock->enqueue(['status' => 307, 'headers' => ['Location' => self::$mock->url . '/elsewhere'], 'text' => '']);
        try {
            $this->client->domains->list();
            $this->fail('no exception');
        } catch (OpenMailWireException $e) {
            $this->assertSame(307, $e->statusCode);
            $this->assertSame('redirect_refused', $e->name);
            $this->assertStringContainsString('/elsewhere', $e->getMessage());
        }
        $this->assertCount(1, self::$mock->requests());
        $this->assertSame('/domains', self::$mock->last()['path']);
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

    public function testWaitForDeliveryWaitsWhileHeld(): void
    {
        self::$mock->json(self::email(['held']));
        self::$mock->json(self::email(['held']));
        self::$mock->json(self::email(['rejected']));
        $email = $this->client->waitForDelivery(self::ID, 5.0, 0.01);
        $this->assertSame(['rejected'], array_column($email['recipients'], 'status'));
        $this->assertCount(3, self::$mock->requests());
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
