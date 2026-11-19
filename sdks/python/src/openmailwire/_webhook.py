import base64
import binascii
import hashlib
import hmac
import json
import re
import time
from typing import Any, Dict, Mapping, Optional, Union

from . import _ed25519
from ._errors import WebhookVerificationError

_TIMESTAMP = re.compile(r"^\d{1,15}$")


def _header(headers: Mapping[str, Any], name: str) -> Optional[str]:
    value: Any = None
    getter = getattr(headers, "get", None)
    if callable(getter):
        value = getter(name)
    if value is None:
        for k, v in headers.items():
            if k.lower() == name:
                value = v
                break
    if isinstance(value, (list, tuple)):
        value = value[0] if value else None
    if isinstance(value, bytes):
        value = value.decode("latin-1")
    return value or None


def verify_webhook(
    secret: str,
    headers: Mapping[str, Any],
    raw_body: Union[bytes, str],
    tolerance: float = 300,
) -> Dict[str, Any]:
    """Check that a webhook request was signed by OpenMailWire and is fresh,
    and return its parsed JSON body.

    ``secret`` is either the webhook's signing secret (``whsec_…``), checked
    against the ``v1`` (HMAC-SHA256) signatures, or its Ed25519 public key
    (``whpk_…``, from ``webhooks.get``), checked against the ``v1a``
    signatures of webhooks created with ``asymmetric_signing``.

    - Accepts both header families: ``svix-id``/``svix-timestamp``/``svix-signature``
      and ``webhook-id``/``webhook-timestamp``/``webhook-signature``, in any case.
    - Accepts any of several space-separated signatures, so it keeps working
      while a secret is rotated.
    - Compares HMAC signatures in constant time.
    - Refuses timestamps more than ``tolerance`` seconds (default 300) from now.

    Pass the body exactly as received, before any JSON parsing.

    :raises WebhookVerificationError: if any check fails.
    """
    msg_id = _header(headers, "svix-id")
    ts = _header(headers, "svix-timestamp")
    sigs = _header(headers, "svix-signature")
    if not (msg_id and ts and sigs):
        msg_id = _header(headers, "webhook-id")
        ts = _header(headers, "webhook-timestamp")
        sigs = _header(headers, "webhook-signature")
    if not (msg_id and ts and sigs):
        raise WebhookVerificationError(
            "missing webhook headers: expected svix-id, svix-timestamp and svix-signature, "
            "or webhook-id, webhook-timestamp and webhook-signature"
        )
    if not _TIMESTAMP.match(ts):
        raise WebhookVerificationError("invalid webhook timestamp header")

    now = int(time.time())
    timestamp = int(ts)
    if now - timestamp > tolerance:
        raise WebhookVerificationError("webhook timestamp is too old")
    if timestamp - now > tolerance:
        raise WebhookVerificationError("webhook timestamp is in the future")

    body = raw_body.encode("utf-8") if isinstance(raw_body, str) else bytes(raw_body)
    signed = f"{msg_id}.{ts}.".encode("utf-8") + body

    ok = False
    if secret.startswith("whpk_"):
        try:
            public_key = base64.b64decode(secret[len("whpk_"):], validate=True)
        except (binascii.Error, ValueError):
            raise WebhookVerificationError("the public key is not a valid whpk_… value") from None
        if len(public_key) != 32:
            raise WebhookVerificationError("the public key is not a valid whpk_… value")
        for part in sigs.split(" "):
            version, sep, sig = part.partition(",")
            if not sep or version != "v1a":
                continue
            try:
                raw_sig = base64.b64decode(sig, validate=True)
            except (binascii.Error, ValueError):
                continue
            if _ed25519.verify(public_key, signed, raw_sig):
                ok = True
                break
    else:
        encoded = secret[len("whsec_"):] if secret.startswith("whsec_") else secret
        try:
            key = base64.b64decode(encoded, validate=True)
        except (binascii.Error, ValueError):
            raise WebhookVerificationError("the signing secret is not a valid whsec_… value") from None
        if not key:
            raise WebhookVerificationError("the signing secret is empty")
        expected = base64.b64encode(hmac.new(key, signed, hashlib.sha256).digest())
        for part in sigs.split(" "):
            version, sep, sig = part.partition(",")
            if sep and version == "v1" and hmac.compare_digest(sig.encode("utf-8"), expected):
                ok = True
    if not ok:
        raise WebhookVerificationError("no webhook signature matches")

    try:
        event = json.loads(body.decode("utf-8"))
    except (UnicodeDecodeError, ValueError):
        raise WebhookVerificationError("the webhook body is not valid JSON") from None
    return event
