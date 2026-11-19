import base64
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from email.utils import parsedate_to_datetime
from typing import Any, Dict, Iterable, List, Mapping, Optional, Union, cast

from ._errors import OpenMailWireError, WaitTimeoutError
from .types import (
    ApiKey,
    ApiKeyList,
    ApiKeyPolicy,
    AuditEntryPage,
    AuditVerification,
    BatchResponse,
    CheckResult,
    CircuitBreaker,
    Decision,
    Deleted,
    DkimState,
    Domain,
    DomainList,
    DomainReports,
    Email,
    EmailPage,
    EmailReplies,
    HeldEmailPage,
    Inbox,
    InboxList,
    Insights,
    Message,
    MessagePage,
    MessageSummary,
    Pause,
    PauseList,
    Project,
    ReplyResponse,
    SendParams,
    SendResponse,
    SuppressionCreated,
    SuppressionDeleted,
    SuppressionPage,
    Webhook,
    WebhookList,
)

__version__ = "0.1.0"
DEFAULT_BASE_URL = "http://localhost:8080"
USER_AGENT = f"openmailwire-python/{__version__}"

_PENDING = frozenset(("queued", "sending", "deferred"))

# Marks an argument that was not given, where None means "send null".
_UNSET: Any = object()


def _seg(value: Union[str, int]) -> str:
    return urllib.parse.quote(str(value), safe="")


def _compact(d: Mapping[str, Any]) -> Dict[str, Any]:
    return {k: v for k, v in d.items() if v is not None}


def _given(d: Mapping[str, Any]) -> Dict[str, Any]:
    """Leaves out arguments that were not given; keeps None, sent as null."""
    return {k: v for k, v in d.items() if v is not _UNSET}


def _rfc3339(value: Union[datetime, str]) -> str:
    if isinstance(value, datetime):
        if value.tzinfo is None:
            value = value.replace(tzinfo=timezone.utc)
        return value.isoformat()
    return value


def _b64(value: Union[bytes, bytearray, memoryview, str]) -> str:
    raw = value.encode("utf-8") if isinstance(value, str) else bytes(value)
    return base64.b64encode(raw).decode("ascii")


def _send_body(params: Mapping[str, Any]) -> Dict[str, Any]:
    body = _compact(params)
    if body.get("attachments") is not None:
        out = []
        for a in body["attachments"]:
            a = _compact(a)
            if not isinstance(a.get("content"), str) and a.get("content") is not None:
                a["content"] = _b64(a["content"])
            out.append(a)
        body["attachments"] = out
    for k in ("scheduled_at", "expires_at"):
        if k in body:
            body[k] = _rfc3339(body[k])
    return body


def _retry_after(value: Optional[str]) -> Optional[int]:
    if not value:
        return None
    value = value.strip()
    if value.isdigit():
        return int(value)
    try:
        at = parsedate_to_datetime(value)
    except (TypeError, ValueError):
        return None
    if at.tzinfo is None:
        at = at.replace(tzinfo=timezone.utc)
    return max(0, int((at - datetime.now(timezone.utc)).total_seconds() + 0.999))


def _to_error(status: int, headers: Any, raw: bytes) -> OpenMailWireError:
    retry_after = _retry_after(headers.get("Retry-After") if headers is not None else None)
    text = raw.decode("utf-8", "replace")
    try:
        j = json.loads(text)
        if isinstance(j, dict) and isinstance(j.get("name"), str):
            message = j.get("message")
            return OpenMailWireError(status, j["name"], message if isinstance(message, str) else "", retry_after)
    except ValueError:
        pass
    return OpenMailWireError(status, "http_error", text[:500] or f"HTTP {status}", retry_after)


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """Refuses redirects: the request then fails with the 3xx answer, so the
    API key is never sent to another address."""

    def redirect_request(self, req: Any, fp: Any, code: int, msg: str, headers: Any, newurl: str) -> None:
        return None


class _Resource:
    def __init__(self, client: "OpenMailWire") -> None:
        self._client = client

    def _request(self, method: str, path: str, **kwargs: Any) -> Any:
        return self._client._request(method, path, **kwargs)


class Emails(_Resource):
    def send(self, params: SendParams, *, idempotency_key: Optional[str] = None) -> SendResponse:
        """Queue an email and return ``{"id": ...}``.

        ``params`` uses the API's field names (``from``, ``to``, ``subject``,
        ``html``, ``text``, ``reply_to``, ``attachments``, ``scheduled_at`` …).
        Attachment ``content`` given as bytes is base64-encoded for you;
        ``scheduled_at`` and ``expires_at`` may be datetimes (naive means UTC).
        """
        return self._request("POST", "/emails", body=_send_body(params), idempotency_key=idempotency_key)

    def get(self, email_id: str) -> Email:
        """One email with each recipient's status, every attempt and the event timeline."""
        return self._request("GET", f"/emails/{_seg(email_id)}")

    def list(
        self, *, limit: Optional[int] = None, before: Optional[str] = None, api_key_id: Optional[str] = None
    ) -> EmailPage:
        """Emails, newest first. Pass ``next_before`` as ``before`` for the next page."""
        return self._request("GET", "/emails", query={"limit": limit, "before": before, "api_key_id": api_key_id})

    def cancel(self, email_id: str) -> Dict[str, Any]:
        """Cancel every recipient not tried yet. 409 ``cannot_cancel`` if there is none."""
        return self._request("POST", f"/emails/{_seg(email_id)}/cancel")

    def batch(self, emails: Iterable[SendParams], *, idempotency_key: Optional[str] = None) -> BatchResponse:
        """Queue 1 to 100 emails (no attachments) in one transaction."""
        return self._request(
            "POST", "/emails/batch", body=[_send_body(e) for e in emails], idempotency_key=idempotency_key
        )

    def send_raw(
        self,
        raw: Union[bytes, str],
        *,
        mail_from: Optional[str] = None,
        rcpts: Optional[List[str]] = None,
        idempotency_key: Optional[str] = None,
    ) -> SendResponse:
        """Queue a complete MIME message (base64-encoded for you). Its From domain must be verified."""
        body = _compact({"raw": _b64(raw), "mail_from": mail_from, "rcpts": rcpts})
        return self._request("POST", "/emails/raw", body=body, idempotency_key=idempotency_key)

    def check(self, params: SendParams) -> CheckResult:
        """Report what :meth:`send` would do with ``params`` now (``would``:
        send, hold or refuse, with every refusal and warning), without
        sending anything."""
        return self._request("POST", "/emails/check", body=_send_body(params))

    def list_held(self, *, limit: Optional[int] = None, before: Optional[str] = None) -> HeldEmailPage:
        """Emails waiting for approval, newest first."""
        return self._request("GET", "/emails/held", query={"limit": limit, "before": before})

    def approve(self, email_id: str, *, note: Optional[str] = None) -> Decision:
        """Approve a held email: its recipients are queued. Needs a
        ``full_access`` key or one with ``can_approve``; no key approves its
        own email."""
        return self._request("POST", f"/emails/{_seg(email_id)}/approve", body=_compact({"note": note}))

    def reject(self, email_id: str, *, note: Optional[str] = None) -> Decision:
        """Reject a held email: it is never sent."""
        return self._request("POST", f"/emails/{_seg(email_id)}/reject", body=_compact({"note": note}))

    def replies(self, email_id: str) -> EmailReplies:
        """The received messages that answer a sent email, oldest first."""
        return self._request("GET", f"/emails/{_seg(email_id)}/replies")


class Domains(_Resource):
    def create(self, name: str) -> Domain:
        """Add a domain and return the DNS records to publish."""
        return self._request("POST", "/domains", body={"name": name})

    def list(self) -> DomainList:
        return self._request("GET", "/domains")

    def get(self, domain_id: str) -> Domain:
        return self._request("GET", f"/domains/{_seg(domain_id)}")

    def verify(self, domain_id: str) -> Domain:
        """Check the DNS records now."""
        return self._request("POST", f"/domains/{_seg(domain_id)}/verify")

    def delete(self, domain_id: str) -> Deleted:
        return self._request("DELETE", f"/domains/{_seg(domain_id)}")

    def dkim(self, domain_id: str) -> DkimState:
        """The domain's DKIM keys, with the records to publish."""
        return self._request("GET", f"/domains/{_seg(domain_id)}/dkim")

    def rotate_dkim(self, domain_id: str) -> DkimState:
        """Rotate the DKIM key in two steps: the first call creates a key to
        publish (``status`` ``pending``), a later call switches signing to it
        once its record is in DNS (``rotated``). 409 ``dkim_record_not_found``
        while the record is missing."""
        return self._request("POST", f"/domains/{_seg(domain_id)}/dkim/rotate")

    def set_ed25519(self, domain_id: str, enabled: bool) -> DkimState:
        """Turn the extra Ed25519 DKIM signature on or off. Turning it on
        creates a key to publish first; call again once its record is in DNS."""
        return self._request("POST", f"/domains/{_seg(domain_id)}/dkim/ed25519", body={"enabled": bool(enabled)})

    def reports(self, domain_id: str, *, days: Optional[int] = None) -> DomainReports:
        """DMARC aggregate and TLS-RPT reports received for the domain
        (default: the last 30 days)."""
        return self._request("GET", f"/domains/{_seg(domain_id)}/reports", query={"days": days})


class Webhooks(_Resource):
    def create(
        self, endpoint: str, events: Optional[List[str]] = None, *, asymmetric_signing: Optional[bool] = None
    ) -> Webhook:
        """Create a webhook. The answer carries ``signing_secret``, shown once.
        Leave out ``events`` to receive every type. ``asymmetric_signing``
        also signs every request with Ed25519 (``v1a``); the answer then has
        the ``public_key`` (``whpk_…``) that verifies them."""
        body = _compact({"endpoint": endpoint, "events": events, "asymmetric_signing": asymmetric_signing})
        return self._request("POST", "/webhooks", body=body)

    def rotate_secret(self, webhook_id: str, *, overlap_seconds: Optional[int] = None) -> Webhook:
        """Replace the signing secret (and Ed25519 key). The previous ones keep
        signing for ``overlap_seconds`` (default one day; 0 stops them at
        once). The answer carries the new ``signing_secret``, shown once."""
        body = _compact({"overlap_seconds": overlap_seconds})
        return self._request("POST", f"/webhooks/{_seg(webhook_id)}/rotate-secret", body=body)

    def list(self) -> WebhookList:
        return self._request("GET", "/webhooks")

    def get(self, webhook_id: str) -> Webhook:
        """One webhook with its 20 most recent deliveries."""
        return self._request("GET", f"/webhooks/{_seg(webhook_id)}")

    def delete(self, webhook_id: str) -> Deleted:
        return self._request("DELETE", f"/webhooks/{_seg(webhook_id)}")


class ApiKeys(_Resource):
    def create(
        self,
        name: str,
        *,
        permission: Optional[str] = None,
        policy: Optional[ApiKeyPolicy] = None,
        allowed_ips: Optional[List[str]] = None,
        expires_at: Optional[Union[datetime, str]] = None,
    ) -> ApiKey:
        """Create a key (``full_access`` or ``sending_access``). The answer
        carries ``token``, shown once. Needs a full-access key.

        ``allowed_ips`` takes addresses and CIDR prefixes the key may be used
        from; ``expires_at`` (a datetime, naive means UTC, or RFC 3339) when it
        stops working."""
        body = _compact(
            {
                "name": name,
                "permission": permission,
                "policy": policy,
                "allowed_ips": allowed_ips,
                "expires_at": None if expires_at is None else _rfc3339(expires_at),
            }
        )
        return self._request("POST", "/api-keys", body=body)

    def list(self) -> ApiKeyList:
        return self._request("GET", "/api-keys")

    def get(self, key_id: str) -> ApiKey:
        return self._request("GET", f"/api-keys/{_seg(key_id)}")

    def update(
        self,
        key_id: str,
        *,
        name: Optional[str] = None,
        policy: Optional[ApiKeyPolicy] = None,
        allowed_ips: Optional[List[str]] = _UNSET,
        expires_at: Optional[Union[datetime, str]] = _UNSET,
    ) -> ApiKey:
        """Rename a key, or replace its policy, allowed networks or expiry.
        Arguments left out are unchanged; ``allowed_ips=None`` removes the
        restriction and ``expires_at=None`` makes the key never expire."""
        body = _compact({"name": name, "policy": policy})
        body.update(_given({"allowed_ips": allowed_ips}))
        if expires_at is not _UNSET:
            body["expires_at"] = None if expires_at is None else _rfc3339(expires_at)
        return self._request("PATCH", f"/api-keys/{_seg(key_id)}", body=body)

    def delete(self, key_id: str) -> Deleted:
        """Revoke a key."""
        return self._request("DELETE", f"/api-keys/{_seg(key_id)}")


class Inboxes(_Resource):
    def create(self, address: str, *, hold_unauthenticated: Optional[bool] = None) -> Inbox:
        """Create an inbox for an address on one of the project's domains.
        ``hold_unauthenticated`` (default True) holds mail whose sender is not
        authenticated from ``sending_access`` keys until it is released."""
        body = _compact({"address": address, "hold_unauthenticated": hold_unauthenticated})
        return self._request("POST", "/inboxes", body=body)

    def list(self) -> InboxList:
        return self._request("GET", "/inboxes")

    def get(self, inbox_id: str) -> Inbox:
        return self._request("GET", f"/inboxes/{_seg(inbox_id)}")

    def update(self, inbox_id: str, *, hold_unauthenticated: bool) -> Inbox:
        """Change whether mail from unauthenticated senders is held. Needs a full-access key."""
        return self._request(
            "PATCH", f"/inboxes/{_seg(inbox_id)}", body={"hold_unauthenticated": bool(hold_unauthenticated)}
        )

    def delete(self, inbox_id: str) -> Deleted:
        return self._request("DELETE", f"/inboxes/{_seg(inbox_id)}")

    def list_messages(
        self,
        inbox_id: str,
        *,
        limit: Optional[int] = None,
        before: Optional[str] = None,
        thread_id: Optional[str] = None,
    ) -> MessagePage:
        """Messages received by an inbox, newest first."""
        return self._request(
            "GET",
            f"/inboxes/{_seg(inbox_id)}/messages",
            query={"limit": limit, "before": before, "thread_id": thread_id},
        )


class Messages(_Resource):
    def get(self, message_id: str) -> Message:
        """One received message, with its text, HTML, headers and attachment list."""
        return self._request("GET", f"/messages/{_seg(message_id)}")

    def get_attachment(self, message_id: str, index: int) -> bytes:
        """The raw bytes of one attachment (``index`` from ``attachments[].index``)."""
        return self._request("GET", f"/messages/{_seg(message_id)}/attachments/{_seg(index)}", binary=True)

    def reply(
        self,
        message_id: str,
        *,
        text: Optional[str] = None,
        html: Optional[str] = None,
        to: Optional[List[str]] = None,
    ) -> ReplyResponse:
        """Reply from the inbox address, threaded with In-Reply-To and
        References. ``to`` defaults to the first Reply-To address, or From.
        The answer has ``id``, ``thread_id``, ``to`` and, when addresses were
        left out or Reply-To is on another domain, ``warnings``."""
        body = _compact({"text": text, "html": html, "to": to})
        return self._request("POST", f"/messages/{_seg(message_id)}/reply", body=body)

    def release(self, message_id: str) -> Message:
        """Release a held message, so keys with access to its inbox see it.
        Needs a full-access key. Answers the message without its content."""
        return self._request("POST", f"/messages/{_seg(message_id)}/release")


class Suppressions(_Resource):
    def list(
        self,
        *,
        limit: Optional[int] = None,
        before: Optional[str] = None,
        reason: Optional[str] = None,
        stream: Optional[str] = None,
        address: Optional[str] = None,
        domain: Optional[str] = None,
    ) -> SuppressionPage:
        """Suppressed addresses, newest first. Pass ``next_before`` as ``before`` for the next page."""
        query = {"limit": limit, "before": before, "reason": reason, "stream": stream}
        query.update(address=address, domain=domain)
        return self._request("GET", "/suppressions", query=query)

    def create(
        self, address: str, *, reason: Optional[str] = None, detail: Optional[str] = None, stream: Optional[str] = None
    ) -> SuppressionCreated:
        """Stop sending to an address (``stream``: ``all``, the default, or
        ``broadcast``). ``created`` is False if it was already suppressed.
        Needs a full-access key."""
        body = _compact({"address": address, "reason": reason, "detail": detail, "stream": stream})
        return self._request("POST", "/suppressions", body=body)

    def delete(self, address: str, *, stream: Optional[str] = None) -> SuppressionDeleted:
        """Send to an address again: remove its suppressions, or only ``stream``'s.
        Needs a full-access key."""
        return self._request("DELETE", f"/suppressions/{_seg(address)}", query={"stream": stream})

    def export(
        self,
        *,
        reason: Optional[str] = None,
        stream: Optional[str] = None,
        address: Optional[str] = None,
        domain: Optional[str] = None,
    ) -> str:
        """Every matching suppression, as CSV text."""
        query = {"reason": reason, "stream": stream, "address": address, "domain": domain}
        return self._request("GET", "/suppressions/export", query=query, text=True)


class Pauses(_Resource):
    def list(self, *, active: Optional[bool] = None, limit: Optional[int] = None) -> PauseList:
        """Circuit-breaker pauses, newest first; ``active=True`` lists only those still in force."""
        return self._request("GET", "/pauses", query={"active": active, "limit": limit})

    def resume(self, pause_id: str) -> Pause:
        """Let the paused key, domain or project send again. Needs a full-access key."""
        return self._request("POST", f"/pauses/{_seg(pause_id)}/resume")


class ProjectSettings(_Resource):
    def get(self) -> Project:
        """The project's per-recipient cap and circuit-breaker settings."""
        return self._request("GET", "/project")

    def update(
        self,
        *,
        max_per_recipient_per_day: Optional[int] = _UNSET,
        circuit_breaker: Optional[CircuitBreaker] = None,
    ) -> Project:
        """Change them. Arguments left out keep their value;
        ``max_per_recipient_per_day=None`` turns the cap off. Needs a
        full-access key."""
        body = _given({"max_per_recipient_per_day": max_per_recipient_per_day})
        if circuit_breaker is not None:
            body["circuit_breaker"] = circuit_breaker
        return self._request("PATCH", "/project", body=body)


class AuditLog(_Resource):
    def list(
        self, *, limit: Optional[int] = None, before: Optional[Union[str, int]] = None, action: Optional[str] = None
    ) -> AuditEntryPage:
        """Audit log entries, newest first. Needs a full-access key."""
        return self._request("GET", "/audit-log", query={"limit": limit, "before": before, "action": action})

    def verify(self) -> AuditVerification:
        """Check the whole hash chain on the server. Keep ``head_hash`` to compare later."""
        return self._request("GET", "/audit-log/verify")


class DeliveryInsights(_Resource):
    def get(self, *, window: Optional[str] = None) -> Insights:
        """Acceptance, deferral and bounce rates and time to acceptance, per
        mailbox provider and sending IP, for the windows ``24h``, ``7d`` and
        ``30d``. The server always computes all three; with ``window``, only
        that one is kept in ``windows``. Needs a full-access key."""
        insights = self._request("GET", "/insights")
        if window and isinstance(insights, dict) and isinstance(insights.get("windows"), list):
            insights["windows"] = [w for w in insights["windows"] if w.get("window") == window]
        return cast(Insights, insights)


class OpenMailWire:
    """A client for the OpenMailWire HTTP API.

    :param api_key: an ``omw_…`` key. Default: the ``OMW_API_KEY`` environment variable.
    :param base_url: default: ``OMW_API_URL``, then ``http://localhost:8080``.
    :param timeout: per-request timeout in seconds.
    :param max_retries: how many times to send a request again when it is
        answered 429 or 503, after waiting its ``Retry-After`` seconds (1 s if
        there is none). Default 0: never retry. The same body and
        idempotency key are sent again.
    :param max_retry_wait: the longest ``Retry-After`` to wait for, in
        seconds; a longer one is raised at once.
    """

    def __init__(
        self,
        api_key: Optional[str] = None,
        base_url: Optional[str] = None,
        *,
        timeout: float = 30.0,
        opener: Optional[urllib.request.OpenerDirector] = None,
        max_retries: int = 0,
        max_retry_wait: float = 60.0,
    ) -> None:
        key = api_key or os.environ.get("OMW_API_KEY")
        if not key:
            raise ValueError("OpenMailWire: no API key. Pass api_key='omw_…' or set OMW_API_KEY.")
        self._api_key = key
        self.base_url = (base_url or os.environ.get("OMW_API_URL") or DEFAULT_BASE_URL).rstrip("/")
        self.timeout = timeout
        self.max_retries = max(0, int(max_retries))
        self.max_retry_wait = max_retry_wait
        self._opener = opener or urllib.request.build_opener(_NoRedirect)

        self.emails = Emails(self)
        self.domains = Domains(self)
        self.webhooks = Webhooks(self)
        self.api_keys = ApiKeys(self)
        self.inboxes = Inboxes(self)
        self.messages = Messages(self)
        self.suppressions = Suppressions(self)
        self.pauses = Pauses(self)
        self.project = ProjectSettings(self)
        self.audit_log = AuditLog(self)
        self.insights = DeliveryInsights(self)

    def __repr__(self) -> str:
        return f"OpenMailWire(base_url={self.base_url!r})"

    def _request(self, method: str, path: str, **kwargs: Any) -> Any:
        """Send a request, and again after ``Retry-After`` on 429 and 503 when ``max_retries`` allows."""
        attempt = 0
        while True:
            try:
                return self._send(method, path, **kwargs)
            except OpenMailWireError as e:
                if not e.retryable or attempt >= self.max_retries:
                    raise
                wait = float(e.retry_after if e.retry_after is not None else 1)
                if wait > self.max_retry_wait:
                    raise
                attempt += 1
                time.sleep(wait)

    def _send(
        self,
        method: str,
        path: str,
        *,
        body: Any = None,
        query: Optional[Mapping[str, Any]] = None,
        idempotency_key: Optional[str] = None,
        binary: bool = False,
        text: bool = False,
    ) -> Any:
        url = self.base_url + path
        if query:
            q = {
                k: ("true" if v else "false") if isinstance(v, bool) else v
                for k, v in query.items()
                if v is not None and v != ""
            }
            if q:
                url += "?" + urllib.parse.urlencode(q)
        headers = {
            "Accept": "*/*" if binary else "text/csv, text/plain, */*" if text else "application/json",
            "User-Agent": USER_AGENT,
        }
        data = None
        if body is not None:
            data = json.dumps(body, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
            headers["Content-Type"] = "application/json"
        if idempotency_key:
            headers["Idempotency-Key"] = idempotency_key

        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        # Never forwarded on a redirect, even by an opener that follows them.
        req.add_unredirected_header("Authorization", f"Bearer {self._api_key}")
        try:
            with self._opener.open(req, timeout=self.timeout) as resp:
                status = resp.status
                raw = resp.read()
        except urllib.error.HTTPError as e:
            try:
                err_body = e.read()
            except OSError:
                err_body = b""
            finally:
                e.close()
            if 300 <= e.code < 400:
                location = e.headers.get("Location", "") if e.headers is not None else ""
                raise OpenMailWireError(
                    e.code,
                    "redirect_refused",
                    f"{method} {self.base_url}{path} answered {e.code}, a redirect to {location or 'another address'}. "
                    "Redirects are not followed, so the API key never goes elsewhere: "
                    "set base_url to the address the API answers at.",
                ) from None
            raise _to_error(e.code, e.headers, err_body) from None
        except (urllib.error.URLError, OSError) as e:
            reason = getattr(e, "reason", None) or e
            raise OpenMailWireError(0, "connection_error", f"{method} {self.base_url}{path}: {reason}") from e

        if binary:
            return raw
        if text:
            return raw.decode("utf-8", "replace")
        if not raw:
            return None
        try:
            return json.loads(raw.decode("utf-8"))
        except ValueError:
            snippet = raw[:200].decode("utf-8", "replace")
            raise OpenMailWireError(
                status, "invalid_response", f"{method} {self.base_url}{path} answered {status} with a body that is not JSON: {snippet}"
            ) from None

    def wait_for_delivery(self, email_id: str, timeout: float = 60.0, interval: float = 1.0) -> Email:
        """Poll ``emails.get`` until no recipient is queued, sending or
        deferred, and return that email. Every recipient then has a final
        status (delivered, bounced, expired, suppressed, unknown or canceled):
        check each one.

        :raises WaitTimeoutError: after ``timeout`` seconds, with ``.email`` set to the last state seen.
        """
        deadline = time.monotonic() + timeout
        while True:
            email = self.emails.get(email_id)
            pending = [r for r in (email.get("recipients") or []) if r.get("status") in _PENDING]
            if not pending:
                return email
            left = deadline - time.monotonic()
            if left <= 0:
                statuses = ", ".join(str(r.get("status")) for r in pending)
                raise WaitTimeoutError(
                    f"email {email_id}: {len(pending)} recipient(s) still {statuses} after {timeout} s",
                    cast(Dict[str, Any], email),
                )
            time.sleep(min(interval, left))

    def wait_for_reply(
        self, email_id: str, timeout: float = 60.0, interval: float = 2.0, *, inbox_id: Optional[str] = None
    ) -> List[MessageSummary]:
        """Poll the inboxes until received messages answer the sent email
        ``email_id`` (their ``in_reply_to_email_id``), and return them, newest
        first. Returns ``[]`` if none arrived within ``timeout`` seconds.
        Looks at the 50 newest messages of each inbox, or only of ``inbox_id``.
        """
        deadline = time.monotonic() + timeout
        while True:
            ids = [inbox_id] if inbox_id else [i["id"] for i in self.inboxes.list()["data"]]
            found: List[MessageSummary] = []
            for iid in ids:
                page = self.inboxes.list_messages(iid, limit=50)
                found.extend(m for m in page.get("data") or [] if m.get("in_reply_to_email_id") == email_id)
            if found:
                return sorted(found, key=lambda m: m.get("received_at") or "", reverse=True)
            left = deadline - time.monotonic()
            if left <= 0:
                return []
            time.sleep(min(interval, left))
