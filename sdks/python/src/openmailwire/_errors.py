from typing import Any, Dict, Optional


class OpenMailWireError(Exception):
    """An error answered by the OpenMailWire API, in its
    ``{statusCode, name, message}`` shape.

    ``name`` is the API's machine-readable error name, e.g.
    ``validation_error``, ``domain_not_verified`` or ``rate_limit_exceeded``.
    Network failures use ``status_code == 0`` and ``name == "connection_error"``.
    """

    def __init__(self, status_code: int, name: str, message: str, retry_after: Optional[int] = None) -> None:
        super().__init__(message)
        self.status_code = status_code
        self.name = name
        self.message = message
        #: For 429 answers: seconds to wait, from the ``Retry-After`` header.
        self.retry_after = retry_after

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
