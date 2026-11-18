<?php

declare(strict_types=1);

namespace OpenMailWire\Tests;

/**
 * Runs PHP's built-in web server on 127.0.0.1 with tests/router.php: it
 * records every request and answers with replies queued by the test.
 */
final class MockServer
{
    public readonly string $url;

    /** @var resource */
    private $process;

    private readonly string $dir;

    public function __construct()
    {
        $this->dir = sys_get_temp_dir() . '/omw-mock-' . bin2hex(random_bytes(6));
        if (!mkdir($this->dir, 0700) && !is_dir($this->dir)) {
            throw new \RuntimeException('cannot create ' . $this->dir);
        }
        $this->reset();

        $port = self::freePort();
        $env = getenv();
        $env['OMW_MOCK_DIR'] = $this->dir;
        $null = \DIRECTORY_SEPARATOR === '\\' ? 'NUL' : '/dev/null';
        $process = proc_open(
            [\PHP_BINARY, '-S', '127.0.0.1:' . $port, __DIR__ . '/router.php'],
            [0 => ['file', $null, 'r'], 1 => ['file', $null, 'w'], 2 => ['file', $null, 'w']],
            $pipes,
            null,
            $env,
        );
        if (!is_resource($process)) {
            throw new \RuntimeException('cannot start php -S');
        }
        $this->process = $process;
        $this->url = 'http://127.0.0.1:' . $port;

        $deadline = microtime(true) + 10;
        while (true) {
            $conn = @fsockopen('127.0.0.1', $port, $errno, $errstr, 0.2);
            if (is_resource($conn)) {
                fclose($conn);
                break;
            }
            if (microtime(true) > $deadline) {
                $this->stop();
                throw new \RuntimeException('the mock server did not start');
            }
            usleep(50_000);
        }
    }

    /** A port that nothing listens on right now. */
    public static function freePort(): int
    {
        $socket = stream_socket_server('tcp://127.0.0.1:0', $errno, $errstr);
        if ($socket === false) {
            throw new \RuntimeException("cannot find a free port: $errstr");
        }
        $name = (string) stream_socket_get_name($socket, false);
        fclose($socket);
        return (int) substr($name, (int) strrpos($name, ':') + 1);
    }

    public function reset(): void
    {
        file_put_contents($this->dir . '/queue.json', '[]');
        file_put_contents($this->dir . '/requests.jsonl', '');
    }

    /**
     * Queues replies: ['status' => 200, 'json' => [...]], or 'text' / 'base64'
     * instead of 'json', plus optional 'headers' and 'sticky'.
     *
     * @param array<string, mixed> ...$replies
     */
    public function enqueue(array ...$replies): void
    {
        $queue = json_decode((string) file_get_contents($this->dir . '/queue.json'), true);
        foreach ($replies as $reply) {
            $queue[] = $reply;
        }
        file_put_contents($this->dir . '/queue.json', json_encode($queue, JSON_UNESCAPED_SLASHES));
    }

    /** Shorthand for a 200 JSON reply. */
    public function json(mixed $body, int $status = 200): void
    {
        $this->enqueue(['status' => $status, 'json' => $body]);
    }

    /**
     * Every request received since the last reset, oldest first. Each has
     * method, path, query, headers (lowercase names), body and json.
     *
     * @return list<array<string, mixed>>
     */
    public function requests(): array
    {
        $out = [];
        $lines = file($this->dir . '/requests.jsonl', FILE_IGNORE_NEW_LINES | FILE_SKIP_EMPTY_LINES);
        foreach ($lines === false ? [] : $lines as $line) {
            $r = json_decode($line, true);
            $r['json'] = $r['body'] === '' ? null : json_decode($r['body'], true);
            $out[] = $r;
        }
        return $out;
    }

    /** @return array<string, mixed> */
    public function last(): array
    {
        $all = $this->requests();
        if ($all === []) {
            throw new \RuntimeException('no request received');
        }
        return $all[count($all) - 1];
    }

    public function stop(): void
    {
        if (is_resource($this->process)) {
            proc_terminate($this->process);
            proc_close($this->process);
        }
        foreach (['queue.json', 'requests.jsonl'] as $file) {
            @unlink($this->dir . '/' . $file);
        }
        @rmdir($this->dir);
    }
}
