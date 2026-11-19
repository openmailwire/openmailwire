from typing import Any, Dict, Optional


class ErrorNames:
    """Error names the API answers with (``OpenMailWireError.name``). Branch
    on these rather than on the message. Newer servers may add names."""

    MISSING_API_KEY = "missing_api_key"
    INVALID_API_KEY = "invalid_api_key"
    #: 401: the key's ``expires_at`` has passed.
    API_KEY_EXPIRED = "api_key_expired"
    #: 403: the key may be used only from its ``allowed_ips``.
    IP_NOT_ALLOWED = "ip_not_allowed"
    #: 403: this needs a ``full_access`` key (or ``can_approve``, for held emails).
    INSUFFICIENT_PERMISSION = "insufficient_permission"
    DOMAIN_NOT_FOUND = "domain_not_found"
    DOMAIN_NOT_VERIFIED = "domain_not_verified"
    SENDER_NOT_ALLOWED = "sender_not_allowed"
    RECIPIENT_NOT_ALLOWED = "recipient_not_allowed"
    #: 403: a circuit breaker paused the key, domain or project (``pauses.resume``).
    SENDING_PAUSED = "sending_paused"
    #: 403: no key approves or rejects its own email.
    CANNOT_APPROVE_OWN_EMAIL = "cannot_approve_own_email"
    NOT_FOUND = "not_found"
    IDEMPOTENCY_CONFLICT = "idempotency_conflict"
    CANNOT_CANCEL = "cannot_cancel"
    #: 409: approve or reject on an email that is not held any more.
    NOT_HELD = "not_held"
    DOMAIN_EXISTS = "domain_exists"
    #: 409: another project verified this domain first.
    DOMAIN_VERIFIED_ELSEWHERE = "domain_verified_elsewhere"
    #: 409: a new DKIM key's TXT record is not published yet.
    DKIM_RECORD_NOT_FOUND = "dkim_record_not_found"
    #: 409: the project has as many webhooks as it may (10 by default).
    WEBHOOK_LIMIT_REACHED = "webhook_limit_reached"
    PAYLOAD_TOO_LARGE = "payload_too_large"
    VALIDATION_ERROR = "validation_error"
    #: 429, with ``retry_after``: a key's cap, or too many requests per second.
    RATE_LIMIT_EXCEEDED = "rate_limit_exceeded"
    #: 429, with ``retry_after``: the project's ``max_per_recipient_per_day``.
    RECIPIENT_RATE_LIMITED = "recipient_rate_limited"
    INTERNAL_ERROR = "internal_error"
    #: 503, with ``retry_after``: too many large requests at once.
    SERVER_BUSY = "server_busy"
    #: Not from the API: the request got no answer (``status_code`` 0).
    CONNECTION_ERROR = "connection_error"
    #: Not from the API: a 3xx answer, which the SDK never follows.
    REDIRECT_REFUSED = "redirect_refused"


class OpenMailWireError(Exception):
    """An error answered by the OpenMailWire API, in its
    ``{statusCode, name, message}`` shape.

    ``name`` is the API's machine-readable error name, e.g.
    ``validation_error``, ``domain_not_verified`` or ``rate_limit_exceeded``;
    :class:`ErrorNames` lists them.
    Network failures use ``status_code == 0`` and ``name == "connection_error"``.
    """

    def __init__(self, status_code: int, name: str, message: str, retry_after: Optional[int] = None) -> None:
        super().__init__(message)
        self.status_code = status_code
        self.name = name
        self.message = message
        #: Seconds to wait, from the ``Retry-After`` header, or None. Set on
        #: 429 answers (``rate_limit_exceeded``, ``recipient_rate_limited``)
        #: and on 503 ``server_busy``.
        self.retry_after = retry_after

    @property
    def retryable(self) -> bool:
        """True for a 429 or 503 answer: the request was refused before any
        work, so it may be sent again (with the same idempotency key) after
        ``retry_after`` seconds."""
        return self.status_code in (429, 503)

    def __str__(self) -> str:
        return f"{self.status_code} {self.name}: {self.message}"

    def __repr__(self) -> str:
        return f"OpenMailWireError(status_code={self.status_code!r}, name={self.name!r}, message={self.message!r})"


class WebhookVerificationError(Exception):
    """Raised by :func:`verify_webhook` when a request is not a genuine, fresh webhook."""


class WaitTimeoutError(TimeoutError):
    """Raised by ``wait_for_delivery`` when the timeout passes while a
    recipient is still queued, sending or deferred. ``email`` is the last
    state seen."""

    def __init__(self, message: str, email: Dict[str, Any]) -> None:
        super().__init__(message)
        self.email = email
