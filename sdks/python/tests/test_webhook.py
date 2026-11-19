import base64
import hashlib
import hmac
import json
import time
import unittest

import mock_server  # noqa: F401  (puts src/ on sys.path)

from openmailwire import WebhookVerificationError, verify_webhook
from openmailwire import _ed25519

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


# Ed25519 ("v1a"). The request vector is pinned on the server side by
# internal/webhook TestSDKVector (seed 00 01 … 1f); every SDK checks it.
V1A = {
    "public_key": "whpk_A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg=",
    "id": "msg_2Lh9KQHvQbLvjGF3s3hQqZ1u",
    "ts": "1700000000",
    "body": '{"type":"email.delivered","data":{"email_id":"e1"}}',
    "sig": "v1a,De0zbXw3fa+WDUWMYmuFVJsBTTtbEkQJi1dS49Vm48H2mguOCtQ8embnNXP20I+H5cLV4SkJSc/+h7BGhrm2Dw==",
}


def v1a_headers(sig=V1A["sig"]):
    return {"webhook-id": V1A["id"], "webhook-timestamp": V1A["ts"], "webhook-signature": sig}


class VerifyV1aTest(unittest.TestCase):
    def test_rfc8032_vectors(self):
        pk = bytes.fromhex("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")
        sig = bytes.fromhex(
            "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b"
        )
        self.assertTrue(_ed25519.verify(pk, b"", sig))
        self.assertFalse(_ed25519.verify(pk, b"x", sig))
        pk2 = bytes.fromhex("3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c")
        sig2 = bytes.fromhex(
            "92a009a9f0d4cab8720e820b5f642540a2b27b5416503f8fb3762223ebdb69da085ac1e43e15996e458f3613d0f11d8c387b2eaeb4302aeeb00d291612bb0c00"
        )
        self.assertTrue(_ed25519.verify(pk2, b"\x72", sig2))
        self.assertFalse(_ed25519.verify(pk, b"\x72", sig2))
        bad = bytearray(sig2)
        bad[63] |= 0x80  # S >= L
        self.assertFalse(_ed25519.verify(pk2, b"\x72", bytes(bad)))
        self.assertFalse(_ed25519.verify(pk2[:31], b"\x72", sig2))

    def test_server_vector(self):
        event = verify_webhook(V1A["public_key"], v1a_headers(), V1A["body"], FOREVER)
        self.assertEqual(event["data"]["email_id"], "e1")
        # Next to v1 signatures and during a rotation (two v1a signatures).
        other = "v1a," + base64.b64encode(bytes(64)).decode()
        mixed = v1a_headers(f"v1,AAAA {other} {V1A['sig']}")
        self.assertTrue(verify_webhook(V1A["public_key"], mixed, V1A["body"], FOREVER))

    def test_v1a_refusals(self):
        with self.assertRaisesRegex(WebhookVerificationError, "no webhook signature matches"):
            verify_webhook(V1A["public_key"], v1a_headers(), V1A["body"].replace("e1", "e2"), FOREVER)
        with self.assertRaisesRegex(WebhookVerificationError, "no webhook signature matches"):
            # A v1a signature never satisfies an HMAC secret, nor the reverse.
            verify_webhook(SECRET, v1a_headers(), V1A["body"], FOREVER)
        with self.assertRaisesRegex(WebhookVerificationError, "no webhook signature matches"):
            verify_webhook(V1A["public_key"], v1a_headers(V1A["sig"].replace("v1a,", "v1,")), V1A["body"], FOREVER)
        with self.assertRaisesRegex(WebhookVerificationError, "public key"):
            verify_webhook("whpk_AAAA", v1a_headers(), V1A["body"], FOREVER)
        with self.assertRaisesRegex(WebhookVerificationError, "too old"):
            verify_webhook(V1A["public_key"], v1a_headers(), V1A["body"])


if __name__ == "__main__":
    unittest.main()
