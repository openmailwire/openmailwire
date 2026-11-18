"""Typed shapes of requests and answers.

Requests and answers use the API's own snake_case field names. Answers are
plain ``dict`` objects; these TypedDicts only document them for type checkers.
"""

from datetime import datetime
from typing import Any, Dict, List, Literal, Optional, TypedDict, Union

# ------------------------------------------------------------------ requests


class Tag(TypedDict):
    name: str
    value: str


class _AttachmentRequired(TypedDict):
    filename: str
    #: bytes are base64-encoded for you; a str must already be base64.
    content: Union[bytes, str]


class Attachment(_AttachmentRequired, total=False):
    content_type: str
    #: Makes the attachment inline: reference it from the HTML as ``cid:<content_id>``.
    content_id: str


# "from" is a Python keyword, so this TypedDict uses the functional syntax.
SendParams = TypedDict(
    "SendParams",
    {
        "from": str,
        "to": Union[str, List[str]],
        "cc": Union[str, List[str]],
        "bcc": Union[str, List[str]],
        "reply_to": Union[str, List[str]],
        "subject": str,
        "html": str,
        "text": str,
        "headers": Dict[str, str],
        "tags": List[Tag],
        "attachments": List[Attachment],
        "scheduled_at": Union[datetime, str],
        "expires_at": Union[datetime, str],
        "retry_uncertain": bool,
    },
    total=False,
)


class ApiKeyPolicy(TypedDict, total=False):
    max_per_hour: int
    max_per_day: int
    allowed_recipients: List[str]
    allowed_from: List[str]


# ------------------------------------------------------------------ answers


class SendResponse(TypedDict):
    id: str


class BatchResponse(TypedDict):
    data: List[SendResponse]


class Recipient(TypedDict, total=False):
    email: str
    #: queued, sending, deferred, delivered, bounced, expired, suppressed, unknown or canceled
    status: str
    attempts: int
    next_attempt_at: Optional[str]
    last_code: Optional[int]
    last_response: str


class Attempt(TypedDict, total=False):
    id: str
    recipient: str
    worker: str
    source_ip: str
    mx_host: str
    outcome: str
    smtp_code: Optional[int]
    enhanced_code: str
    response: str
    tls: str
    started_at: str
    finished_at: Optional[str]


class EmailEvent(TypedDict, total=False):
    type: str
    recipient: str
    data: Any
    created_at: str


Email = TypedDict(
    "Email",
    {
        "object": Literal["email"],
        "id": str,
        "from": str,
        "to": List[str],
        "cc": Optional[List[str]],
        "bcc": Optional[List[str]],
        "subject": str,
        "message_id": str,
        "created_at": str,
        "expires_at": str,
        "scheduled_at": Optional[str],
        "last_event": str,
        "tags": Optional[Dict[str, str]],
        "body_purged": bool,
        "recipients": List[Recipient],
        "attempts": List[Attempt],
        "events": List[EmailEvent],
    },
    total=False,
)


EmailSummary = TypedDict(
    "EmailSummary",
    {
        "id": str,
        "from": str,
        "to": List[str],
        "subject": str,
        "created_at": str,
        "scheduled_at": Optional[str],
        "last_event": str,
        "api_key_id": Optional[str],
    },
    total=False,
)


class EmailPage(TypedDict, total=False):
    object: str
    data: List[EmailSummary]
    has_more: bool
    next_before: Optional[str]


class DnsRecord(TypedDict, total=False):
    record: str
    type: str
    name: str
    value: str
    priority: int
    required: bool
    status: str
    detail: str


class Domain(TypedDict, total=False):
    object: Literal["domain"]
    id: str
    name: str
    status: str
    created_at: str
    verified_at: Optional[str]
    checked_at: Optional[str]
    records: List[DnsRecord]


class DomainList(TypedDict):
    object: str
    data: List[Domain]


class Deleted(TypedDict):
    object: str
    id: str
    deleted: bool


class WebhookDelivery(TypedDict, total=False):
    id: str
    type: str
    status: str
    attempts: int
    last_status: Optional[int]
    last_error: str
    created_at: str
    delivered_at: Optional[str]


class Webhook(TypedDict, total=False):
    object: Literal["webhook"]
    id: str
    endpoint: str
    events: Optional[List[str]]
    created_at: str
    #: ``whsec_…``, returned once, by ``create``.
    signing_secret: str
    recent_deliveries: List[WebhookDelivery]


class WebhookList(TypedDict):
    object: str
    data: List[Webhook]


class ApiKey(TypedDict, total=False):
    id: str
    name: str
    prefix: str
    #: full_access or sending_access
    permission: str
    policy: ApiKeyPolicy
    created_at: str
    last_used_at: Optional[str]
    #: The secret ``omw_…`` token, returned once, by ``create``.
    token: str


class ApiKeyList(TypedDict):
    object: str
    data: List[ApiKey]


class Inbox(TypedDict, total=False):
    id: str
    address: str
    created_at: str


class InboxList(TypedDict):
    object: str
    data: List[Inbox]


MessageSummary = TypedDict(
    "MessageSummary",
    {
        "id": str,
        "from": str,
        "to": Union[List[str], str],
        "subject": str,
        "received_at": str,
        "thread_id": Optional[str],
        "in_reply_to_email_id": Optional[str],
        "snippet": str,
    },
    total=False,
)


class MessagePage(TypedDict, total=False):
    object: str
    data: List[MessageSummary]
    has_more: bool
    next_before: Optional[str]


class MessageAttachment(TypedDict, total=False):
    index: int
    filename: str
    content_type: str
    size: int


Message = TypedDict(
    "Message",
    {
        "id": str,
        "from": str,
        "inbox_id": str,
        "to": Union[List[str], str],
        "cc": Union[List[str], str, None],
        "subject": str,
        "text": Optional[str],
        "html": Optional[str],
        # message-id, in-reply-to, references, date
        "headers": Dict[str, str],
        "attachments": List[MessageAttachment],
        "thread_id": Optional[str],
        "in_reply_to_email_id": Optional[str],
        "received_at": str,
    },
    total=False,
)
