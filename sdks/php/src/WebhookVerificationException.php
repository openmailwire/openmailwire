<?php

declare(strict_types=1);

namespace OpenMailWire;

/** Thrown by Webhook::verify() when a request is not a genuine, fresh webhook. */
class WebhookVerificationException extends \RuntimeException
{
}
