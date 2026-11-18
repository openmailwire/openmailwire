<?php

declare(strict_types=1);

namespace OpenMailWire;

if (!\function_exists('OpenMailWire\verifyWebhook')) {
    /**
     * Shorthand for Webhook::verify(): checks a webhook request's signature and
     * timestamp, and returns its decoded JSON body.
     *
     * @param array<string, string|list<string>> $headers
     * @return array<string, mixed>
     *
     * @throws WebhookVerificationException
     */
    function verifyWebhook(string $secret, array $headers, string $rawBody, int $tolerance = Webhook::DEFAULT_TOLERANCE): array
    {
        return Webhook::verify($secret, $headers, $rawBody, $tolerance);
    }
}
