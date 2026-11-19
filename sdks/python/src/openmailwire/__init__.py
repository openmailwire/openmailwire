"""Python client for OpenMailWire, open-source email for AI agents.

Standard library only. Python 3.9+.

    from openmailwire import OpenMailWire

    client = OpenMailWire()  # reads OMW_API_URL and OMW_API_KEY
    sent = client.emails.send(
        {"from": "Acme <hello@acme.com>", "to": "you@example.org", "subject": "Hi", "text": "Hello"},
        idempotency_key="welcome-42",
    )
    email = client.wait_for_delivery(sent["id"])
"""

from ._client import (
    DEFAULT_BASE_URL,
    ApiKeys,
    AuditLog,
    DeliveryInsights,
    Domains,
    Emails,
    Inboxes,
    Messages,
    OpenMailWire,
    Pauses,
    ProjectSettings,
    Suppressions,
    Webhooks,
    __version__,
)
from ._errors import ErrorNames, OpenMailWireError, WaitTimeoutError, WebhookVerificationError
from ._webhook import verify_webhook

__all__ = [
    "DEFAULT_BASE_URL",
    "ApiKeys",
    "AuditLog",
    "DeliveryInsights",
    "Domains",
    "Emails",
    "ErrorNames",
    "Inboxes",
    "Messages",
    "OpenMailWire",
    "OpenMailWireError",
    "Pauses",
    "ProjectSettings",
    "Suppressions",
    "WaitTimeoutError",
    "Webhooks",
    "WebhookVerificationError",
    "__version__",
    "verify_webhook",
]
