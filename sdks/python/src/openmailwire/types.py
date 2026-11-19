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
        #: transactional (the default) or broadcast, which adds a one-click unsubscribe.
        "stream": str,
    },
    total=False,
)


class RequireApproval(TypedDict, total=False):
    """Hold the key's emails for approval when any rule matches."""

    always: bool
    first_contact: bool
    has_attachments: bool
    has_links: bool
    recipients_over: int


class ApiKeyPolicy(TypedDict, total=False):
    max_per_hour: int
    max_per_day: int
    allowed_recipients: List[str]
    allowed_from: List[str]
    require_approval: RequireApproval
    #: This key may approve and reject held emails of other keys.
    can_approve: bool
    #: Inbox address patterns a ``sending_access`` key may read and reply from.
    inboxes: List[str]


class CircuitBreaker(TypedDict, total=False):
    enabled: bool
    #: Pause when bounces over 24 hours are above this fraction of recipients.
    bounce_rate: float
    #: Pause when complaints are above this fraction.
    complaint_rate: float
    #: Rates count only once a scope queued this many recipients in 24 hours.
    min_recipients: int


# ------------------------------------------------------------------ answers


class _SendResponseRequired(TypedDict):
    id: str


class SendResponse(_SendResponseRequired, total=False):
    #: Present (and True) when the email waits for approval.
    held: bool
    #: With ``held``: the ``require_approval`` rules that matched.
    hold_reasons: List[str]


class BatchResponse(TypedDict):
    data: List[SendResponse]


class CheckWarning(TypedDict):
    code: str
    message: str


class ErrorBody(TypedDict):
    statusCode: int
    name: str
    message: str


class CheckResult(TypedDict, total=False):
    object: Literal["email_check"]
    #: No refusal and no warning.
    ok: bool
    #: send, hold or refuse
    would: str
    stream: str
    size: int
    hold_reasons: List[str]
    refusals: List[ErrorBody]
    warnings: List[CheckWarning]


HeldEmail = TypedDict(
    "HeldEmail",
    {
        "object": Literal["held_email"],
        "id": str,
        "api_key_id": Optional[str],
        "from": str,
        "to": List[str],
        "cc": List[str],
        "bcc": List[str],
        "subject": str,
        "stream": str,
        "hold_reasons": List[str],
        "recipients": List[str],
        "created_at": str,
        "expires_at": str,
        "scheduled_at": Optional[str],
        "text": str,
        "text_truncated": bool,
    },
    total=False,
)


class HeldEmailPage(TypedDict, total=False):
    object: str
    data: List[HeldEmail]
    has_more: bool
    next_before: Optional[str]


class Decision(TypedDict, total=False):
    object: Literal["email"]
    id: str
    #: approved or rejected
    decision: str
    recipients: List[str]
    #: Approved, but suppressed since the email was held, so not sent.
    suppressed: List[str]


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


class ReportAddresses(TypedDict, total=False):
    dmarc_rua: str
    tls_rpt_rua: str
    dmarc_record_example: str
    tls_rpt_record: Dict[str, str]


class Domain(TypedDict, total=False):
    object: Literal["domain"]
    id: str
    name: str
    status: str
    created_at: str
    verified_at: Optional[str]
    checked_at: Optional[str]
    records: List[DnsRecord]
    #: An unverified domain is removed at this time; None once verified.
    claim_expires_at: Optional[str]
    report_addresses: ReportAddresses


class DkimKey(TypedDict, total=False):
    selector: str
    #: rsa or ed25519
    algorithm: str
    #: active, pending or retiring
    state: str
    signs: bool
    record: DnsRecord
    created_at: str
    retire_at: Optional[str]


class DkimState(TypedDict, total=False):
    #: dkim, dkim_rotation or dkim_ed25519
    object: str
    status: str
    message: str
    keys: List[DkimKey]
    auth_issues: List[Dict[str, Any]]


class DomainReports(TypedDict, total=False):
    object: Literal["domain_reports"]
    domain_id: str
    domain: str
    days: int
    addresses: Dict[str, Any]
    dmarc: Dict[str, Any]
    tls: Dict[str, Any]
    reports: List[Dict[str, Any]]


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
    #: ``whsec_…``, returned once, by ``create`` and ``rotate_secret``.
    signing_secret: str
    asymmetric_signing: bool
    #: ``whpk_…``: the Ed25519 public key that verifies ``v1a`` signatures.
    public_key: str
    previous_public_key: str
    previous_secret_expires_at: str
    secret_rotated_at: str
    consecutive_failures: int
    paused_until: str
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
    allowed_ips: Optional[List[str]]
    expires_at: Optional[str]
    #: The secret ``omw_…`` token, returned once, by ``create``.
    token: str


class ApiKeyList(TypedDict):
    object: str
    data: List[ApiKey]


class Inbox(TypedDict, total=False):
    object: Literal["inbox"]
    id: str
    address: str
    created_at: str
    domain_id: str
    hold_unauthenticated: bool


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
        #: authenticated, unauthenticated or failed
        "trust": str,
        "held": bool,
        "suspicious_instructions": bool,
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
        #: authenticated, unauthenticated or failed
        "trust": str,
        "authentication_results": Optional[Dict[str, Any]],
        "held": bool,
        "released_at": Optional[str],
        "suspicious_instructions": bool,
        "suspicious_rules": List[str],
        "reply_to": List[str],
        "mail_from": str,
        "reply_to_mismatch": bool,
        "message_id_conflict": bool,
        "size": int,
        "tls": bool,
        "body_purged": bool,
    },
    total=False,
)


class ReplyResponse(TypedDict, total=False):
    id: str
    thread_id: str
    #: Who the reply was sent to.
    to: List[str]
    warnings: List[str]
    held: bool
    hold_reasons: List[str]


class EmailReplies(TypedDict, total=False):
    object: str
    email_id: str
    thread_id: str
    data: List[MessageSummary]


# ------------------------------------------------------------------ suppressions, pauses, project


class Suppression(TypedDict, total=False):
    address: str
    #: all or broadcast
    stream: str
    reason: str
    detail: str
    created_at: str


class SuppressionPage(TypedDict, total=False):
    object: str
    data: List[Suppression]
    has_more: bool
    next_before: Optional[str]


class SuppressionCreated(Suppression, total=False):
    object: Literal["suppression"]
    #: False when the address was already suppressed for that stream.
    created: bool


class SuppressionDeleted(TypedDict, total=False):
    object: Literal["suppression"]
    address: str
    deleted: bool
    removed: List[Suppression]


class Pause(TypedDict, total=False):
    object: Literal["pause"]
    id: str
    #: api_key, domain or project
    scope: str
    scope_id: str
    scope_name: str
    #: bounce_rate or complaint_rate
    reason: str
    rate: float
    threshold: float
    recipients: int
    bounces: int
    complaints: int
    created_at: str
    resumed_at: Optional[str]
    resumed_by: Optional[str]
    active: bool


class PauseList(TypedDict):
    object: str
    data: List[Pause]


class Project(TypedDict, total=False):
    object: Literal["project"]
    id: str
    name: str
    max_per_recipient_per_day: Optional[int]
    circuit_breaker: CircuitBreaker
    updated_at: Optional[str]


# ------------------------------------------------------------------ audit log, insights


class AuditEntry(TypedDict, total=False):
    object: Literal["audit_entry"]
    id: int
    project_id: Optional[str]
    created_at: str
    #: api_key, cli or system
    actor: str
    actor_key_id: Optional[str]
    action: str
    target_type: str
    target_id: str
    data: Dict[str, Any]
    #: The exact JSON text that was hashed.
    entry: str
    prev_hash: str
    hash: str


class AuditEntryPage(TypedDict, total=False):
    object: str
    data: List[AuditEntry]
    has_more: bool
    next_before: Optional[str]


class AuditVerification(TypedDict, total=False):
    object: Literal["audit_verification"]
    valid: bool
    entries: int
    head_hash: str
    first_invalid_id: Optional[int]
    problem: str


class InsightStats(TypedDict, total=False):
    attempts: int
    accepted: int
    deferred: int
    bounced: int
    unknown: int
    acceptance_rate: Optional[float]
    deferral_rate: Optional[float]
    bounce_rate: Optional[float]
    time_to_acceptance_p50_seconds: Optional[float]
    time_to_acceptance_p95_seconds: Optional[float]


class InsightGroup(InsightStats, total=False):
    provider: str
    sending_ip: str


class InsightWindow(TypedDict, total=False):
    #: 24h, 7d or 30d
    window: str
    total: InsightStats
    by_provider: List[InsightGroup]
    by_sending_ip: List[InsightGroup]


class Insights(TypedDict, total=False):
    object: Literal["insights"]
    generated_at: str
    window_basis: str
    provider_grouping: str
    emails_considered: int
    truncated: bool
    windows: List[InsightWindow]


# ------------------------------------------------------------------ webhook events

WebhookEventType = Literal[
    "email.sent",
    "email.delivered",
    "email.delivery_delayed",
    "email.bounced",
    "email.failed",
    "email.suppressed",
    "email.delivery_unknown",
    "email.canceled",
    "email.complained",
    "email.received",
    "email.held",
    "email.approved",
    "email.rejected",
    "sending.paused",
]
"""The event types a webhook can receive. Newer servers may add more."""


WebhookEventData = TypedDict(
    "WebhookEventData",
    {
        "email_id": str,
        "created_at": str,
        "from": str,
        "to": List[str],
        "subject": str,
        "tags": Dict[str, str],
        "bounce": Dict[str, str],
        "delivery": Dict[str, Any],
        # email.complained: feedback_type, user_agent
        "complaint": Dict[str, str],
        # email.held: reasons; email.approved: approved_by, note; email.rejected: rejected_by, note
        "approval": Dict[str, Any],
        # sending.paused: the pause (see Pause)
        "pause": Dict[str, Any],
        # email.received: email_id is the received message, to the inbox address
        "inbox_id": str,
        "inbox": str,
        "thread_id": str,
        "in_reply_to_email_id": str,
        "message_id": str,
        "cc": List[str],
        "header_to": List[str],
        "trust": str,
        "authentication": Dict[str, str],
        "held": bool,
        "suspicious_instructions": bool,
    },
    total=False,
)


class WebhookEvent(TypedDict):
    """The JSON body of a webhook request, as ``verify_webhook`` returns it."""

    type: str
    created_at: str
    data: WebhookEventData
