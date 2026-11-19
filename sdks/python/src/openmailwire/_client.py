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
    BatchResponse,
    Deleted,
    Domain,
    DomainList,
    Email,
    EmailPage,
    Inbox,
    InboxList,
    Message,
    MessagePage,
    MessageSummary,
    SendParams,
    SendResponse,
    Webhook,
    WebhookList,
)

__version__ = "0.1.0"
DEFAULT_BASE_URL = "http://localhost:8080"
USER_AGENT = f"openmailwire-python/{__version__}"

_PENDING = frozenset(("queued", "sending", "deferred"))


def _seg(value: Union[str, int]) -> str:
    return urllib.parse.quote(str(value), safe="")


def _compact(d: Mapping[str, Any]) -> Dict[str, Any]:
    return {k: v for k, v in d.items() if v is not None}


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


class Webhooks(_Resource):
    def create(self, endpoint: str, events: Optional[List[str]] = None) -> Webhook:
        """Create a webhook. The answer carries ``signing_secret``, shown once.
        Leave out ``events`` to receive every type."""
        return self._request("POST", "/webhooks", body=_compact({"endpoint": endpoint, "events": events}))

    def list(self) -> WebhookList:
        return self._request("GET", "/webhooks")

    def get(self, webhook_id: str) -> Webhook:
        """One webhook with its 20 most recent deliveries."""
        return self._request("GET", f"/webhooks/{_seg(webhook_id)}")

    def delete(self, webhook_id: str) -> Deleted:
        return self._request("DELETE", f"/webhooks/{_seg(webhook_id)}")


class ApiKeys(_Resource):
    def create(
        self, name: str, *, permission: Optional[str] = None, policy: Optional[ApiKeyPolicy] = None
    ) -> ApiKey:
        """Create a key (``full_access`` or ``sending_access``). The answer
        carries ``token``, shown once. Needs a full-access key."""
        return self._request(
            "POST", "/api-keys", body=_compact({"name": name, "permission": permission, "policy": policy})
        )

    def list(self) -> ApiKeyList:
        return self._request("GET", "/api-keys")

    def get(self, key_id: str) -> ApiKey:
        return self._request("GET", f"/api-keys/{_seg(key_id)}")

    def update(self, key_id: str, *, name: Optional[str] = None, policy: Optional[ApiKeyPolicy] = None) -> ApiKey:
        """Rename a key or replace its policy."""
        return self._request("PATCH", f"/api-keys/{_seg(key_id)}", body=_compact({"name": name, "policy": policy}))

    def delete(self, key_id: str) -> Deleted:
        """Revoke a key."""
        return self._request("DELETE", f"/api-keys/{_seg(key_id)}")


class Inboxes(_Resource):
    def create(self, address: str) -> Inbox:
        """Create an inbox for an address on one of the project's domains."""
        return self._request("POST", "/inboxes", body={"address": address})

    def list(self) -> InboxList:
        return self._request("GET", "/inboxes")

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

    def reply(self, message_id: str, *, text: Optional[str] = None, html: Optional[str] = None) -> SendResponse:
        """Reply from the inbox address, threaded with In-Reply-To and References."""
        return self._request("POST", f"/messages/{_seg(message_id)}/reply", body=_compact({"text": text, "html": html}))


class OpenMailWire:
    """A client for the OpenMailWire HTTP API.

    :param api_key: an ``omw_…`` key. Default: the ``OMW_API_KEY`` environment variable.
    :param base_url: default: ``OMW_API_URL``, then ``http://localhost:8080``.
    :param timeout: per-request timeout in seconds.
    """

    def __init__(
        self,
        api_key: Optional[str] = None,
        base_url: Optional[str] = None,
        *,
        timeout: float = 30.0,
        opener: Optional[urllib.request.OpenerDirector] = None,
    ) -> None:
        key = api_key or os.environ.get("OMW_API_KEY")
        if not key:
            raise ValueError("OpenMailWire: no API key. Pass api_key='omw_…' or set OMW_API_KEY.")
        self._api_key = key
        self.base_url = (base_url or os.environ.get("OMW_API_URL") or DEFAULT_BASE_URL).rstrip("/")
        self.timeout = timeout
        self._opener = opener or urllib.request.build_opener(_NoRedirect)

        self.emails = Emails(self)
        self.domains = Domains(self)
        self.webhooks = Webhooks(self)
        self.api_keys = ApiKeys(self)
        self.inboxes = Inboxes(self)
        self.messages = Messages(self)

    def __repr__(self) -> str:
        return f"OpenMailWire(base_url={self.base_url!r})"

    def _request(
        self,
        method: str,
        path: str,
        *,
        body: Any = None,
        query: Optional[Mapping[str, Any]] = None,
        idempotency_key: Optional[str] = None,
        binary: bool = False,
    ) -> Any:
        url = self.base_url + path
        if query:
            q = {k: v for k, v in query.items() if v is not None and v != ""}
            if q:
                url += "?" + urllib.parse.urlencode(q)
        headers = {
            "Accept": "*/*" if binary else "application/json",
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
