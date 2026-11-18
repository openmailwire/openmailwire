import base64
import hashlib
import hmac
import json
import time
import unittest

import mock_server  # noqa: F401  (puts src/ on sys.path)

from openmailwire import WebhookVerificationError, verify_webhook

# The reference vector from Svix's own libraries; the server's signer
# (internal/webhook.Sign) produces the same signature.
VECTOR = {
    "secret": "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw",
    "id": "msg_p5jXN8AQM9LWM0D4loKWxJek",
    "ts": "1614265330",
    "body": '{"test": 2432232314}',
    "sig": "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=",
}
FOREVER = 100 * 365 * 24 * 3600

SECRET = "whsec_" + base64.b64encode(b"0123456789abcdef0123456789abcdef").decode()
BODY = json.dumps({"type": "email.delivered", "created_at": "2026-11-18T10:00:00Z", "data": {"email_id": "e1"}})


def sign(secret, msg_id, ts, body):
    key = base64.b64decode(secret[len("whsec_"):])
    mac = hmac.new(key, f"{msg_id}.{ts}.{body}".encode(), hashlib.sha256).digest()
    return "v1," + base64.b64encode(mac).decode()


def svix_headers(msg_id="msg_1", ts=None, body=BODY, secret=SECRET):
    ts = int(time.time()) if ts is None else ts
    return {"svix-id": msg_id, "svix-timestamp": str(ts), "svix-signature": sign(secret, msg_id, ts, body)}


class CaseInsensitive(dict):
    """Like the header objects of Flask, Django or Starlette."""

    def get(self, name, default=None):
        for k, v in self.items():
            if k.lower() == name.lower():
                return v
        return default


class VerifyWebhookTest(unittest.TestCase):
    def refused(self, pattern, *args, **kwargs):
        with self.assertRaisesRegex(WebhookVerificationError, pattern):
            verify_webhook(*args, **kwargs)

    def test_svix_reference_vector(self):
        headers = {"svix-id": VECTOR["id"], "svix-timestamp": VECTOR["ts"], "svix-signature": VECTOR["sig"]}
        self.assertEqual(verify_webhook(VECTOR["secret"], headers, VECTOR["body"], FOREVER), {"test": 2432232314})

    def test_svix_headers(self):
        event = verify_webhook(SECRET, svix_headers(), BODY)
        self.assertEqual(event["type"], "email.delivered")
        self.assertEqual(event["data"]["email_id"], "e1")

    def test_webhook_headers_any_case(self):
        h = svix_headers()
        plain = {"Webhook-Id": h["svix-id"], "WEBHOOK-TIMESTAMP": h["svix-timestamp"], "webhook-signature": h["svix-signature"]}
        self.assertEqual(verify_webhook(SECRET, plain, BODY)["type"], "email.delivered")
        self.assertEqual(verify_webhook(SECRET, CaseInsensitive(plain), BODY.encode())["type"], "email.delivered")

    def test_several_signatures(self):
        h = svix_headers()
        other = sign("whsec_" + base64.b64encode(b"rotated out").decode(), "msg_1", h["svix-timestamp"], BODY)
        h["svix-signature"] = f"v2,ignored {other} {h['svix-signature']}"
        self.assertEqual(verify_webhook(SECRET, h, BODY)["type"], "email.delivered")

    def test_wrong_signature_body_or_secret(self):
        h = svix_headers()
        self.refused("no webhook signature matches", SECRET, h, BODY.replace("delivered", "bounced"))
        self.refused("no webhook signature matches", "whsec_" + base64.b64encode(b"wrong").decode(), h, BODY)
        self.refused("no webhook signature matches", SECRET, dict(h, **{"svix-signature": "v1,AAAA"}), BODY)
        self.refused("no webhook signature matches", SECRET, dict(h, **{"svix-id": "msg_2"}), BODY)

    def test_timestamps(self):
        now = int(time.time())
        self.refused("too old", SECRET, svix_headers(ts=now - 301), BODY)
        self.refused("future", SECRET, svix_headers(ts=now + 301), BODY)
        self.assertTrue(verify_webhook(SECRET, svix_headers(ts=now - 290), BODY))
        self.refused("too old", SECRET, svix_headers(ts=now - 20), BODY, tolerance=10)
        self.assertTrue(verify_webhook(SECRET, svix_headers(ts=now - 600), BODY, tolerance=900))

    def test_missing_headers_bad_timestamp_bad_secret(self):
        self.refused("missing webhook headers", SECRET, {}, BODY)
        h = svix_headers()
        del h["svix-signature"]
        self.refused("missing webhook headers", SECRET, h, BODY)
        self.refused("timestamp", SECRET, dict(svix_headers(), **{"svix-timestamp": "soon"}), BODY)
        self.refused("signing secret", "whsec_***", svix_headers(), BODY)


if __name__ == "__main__":
    unittest.main()
