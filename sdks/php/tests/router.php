<?php

// Router for PHP's built-in web server (php -S), used by MockServer: it
// records every request and answers with the next queued reply, so the SDK
// is tested without any real network service.
//
// A reply is {"status": int, "headers": {..}, and one of "json": any,
// "text": string or "base64": string}. A reply with "sticky": true is
// repeated once it is the last one in the queue.

declare(strict_types=1);

$dir = (string) getenv('OMW_MOCK_DIR');
$queueFile = $dir . '/queue.json';
$logFile = $dir . '/requests.jsonl';

$uri = (string) $_SERVER['REQUEST_URI'];
$record = [
    'method' => (string) $_SERVER['REQUEST_METHOD'],
    'path' => (string) parse_url($uri, PHP_URL_PATH),
    'query' => (string) (parse_url($uri, PHP_URL_QUERY) ?? ''),
    'headers' => array_change_key_case(getallheaders(), CASE_LOWER),
    'body' => (string) file_get_contents('php://input'),
];
file_put_contents($logFile, json_encode($record, JSON_UNESCAPED_SLASHES) . "\n", FILE_APPEND | LOCK_EX);

$queue = json_decode((string) file_get_contents($queueFile), true);
if (!is_array($queue)) {
    $queue = [];
}
$reply = $queue[0] ?? null;
if ($reply !== null && !(count($queue) === 1 && !empty($reply['sticky']))) {
    array_shift($queue);
    file_put_contents($queueFile, json_encode($queue), LOCK_EX);
}
if ($reply === null) {
    $reply = ['status' => 404, 'json' => ['statusCode' => 404, 'name' => 'not_found', 'message' => 'no reply queued']];
}

http_response_code((int) ($reply['status'] ?? 200));
foreach ($reply['headers'] ?? [] as $name => $value) {
    header($name . ': ' . $value);
}
if (array_key_exists('base64', $reply)) {
    header('Content-Type: application/octet-stream');
    echo base64_decode((string) $reply['base64']);
} elseif (array_key_exists('text', $reply)) {
    header('Content-Type: text/plain');
    echo (string) $reply['text'];
} else {
    header('Content-Type: application/json');
    echo json_encode($reply['json'] ?? new stdClass(), JSON_UNESCAPED_SLASHES);
}
