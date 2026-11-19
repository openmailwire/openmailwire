import base64
import os
import unittest
from datetime import datetime, timezone
from unittest import mock

from mock_server import MockServer  # also puts src/ on sys.path

from openmailwire import OpenMailWire, OpenMailWireError, WaitTimeoutError

ID = "ed01a077-1c2d-4e5f-8a9b-0c1d2e3f4a5b"
KEY = "omw_test_key"


def email(statuses):
    return {
        "object": "email",
        "id": ID,
        "from": "a@acme.test",
        "to": ["x@example.test"],
        "subject": "hi",
        "recipients": [{"email": f"r{i}@example.test", "status": s, "attempts": 0} for i, s in enumerate(statuses)],
        "attempts": [],
        "events": [],
    }


class ClientTest(unittest.TestCase):
    def serve(self, handler):
        self.mock = MockServer(handler)
        self.addCleanup(self.mock.close)
        return OpenMailWire(api_key=KEY, base_url=self.mock.url)

    # ------------------------------------------------------------ configuration

    def test_reads_environment(self):
        client = None
        self.mock = MockServer(lambda r: (200, {"object": "list", "data": []}, {}))
        self.addCleanup(self.mock.close)
        with mock.patch.dict(os.environ, {"OMW_API_URL": self.mock.url + "/", "OMW_API_KEY": "omw_from_env"}):
            client = OpenMailWire()
        self.assertEqual(client.base_url, self.mock.url)
        client.domains.list()
        self.assertEqual(self.mock.requests[0].headers["Authorization"], "Bearer omw_from_env")

    def test_defaults_and_missing_key(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            self.assertEqual(OpenMailWire(api_key=KEY).base_url, "http://localhost:8080")
            with self.assertRaisesRegex(ValueError, "OMW_API_KEY"):
                OpenMailWire()

    # ------------------------------------------------------------ emails

    def test_send(self):
        client = self.serve(lambda r: (200, {"id": ID}, {}))
        res = client.emails.send(
            {
                "from": "Acme <hello@acme.test>",
                "to": ["you@example.test"],
                "reply_to": "support@acme.test",
                "subject": "Your code",
                "text": "482913",
                "html": None,
                "attachments": [
                    {"filename": "a.txt", "content": b"hello", "content_type": "text/plain"},
                    {"filename": "logo.png", "content": "iVBORw0K", "content_id": "logo"},
                ],
                "scheduled_at": datetime(2030, 1, 2, 3, 4, 5, tzinfo=timezone.utc),
                "expires_at": datetime(2030, 1, 3),
                "retry_uncertain": False,
            },
            idempotency_key="signup-42",
        )
        self.assertEqual(res, {"id": ID})
        r = self.mock.requests[0]
        self.assertEqual((r.method, r.path), ("POST", "/emails"))
        self.assertEqual(r.headers["Authorization"], f"Bearer {KEY}")
        self.assertEqual(r.headers["Content-Type"], "application/json")
        self.assertEqual(r.headers["Idempotency-Key"], "signup-42")
        self.assertTrue(r.headers["User-Agent"].startswith("openmailwire-python/"))
        self.assertEqual(
            r.json,
            {
                "from": "Acme <hello@acme.test>",
                "to": ["you@example.test"],
                "reply_to": "support@acme.test",
                "subject": "Your code",
                "text": "482913",
                "attachments": [
                    {"filename": "a.txt", "content": "aGVsbG8=", "content_type": "text/plain"},
                    {"filename": "logo.png", "content": "iVBORw0K", "content_id": "logo"},
                ],
                "scheduled_at": "2030-01-02T03:04:05+00:00",
                "expires_at": "2030-01-03T00:00:00+00:00",
                "retry_uncertain": False,
            },
        )

    def test_send_without_idempotency_key(self):
        client = self.serve(lambda r: (200, {"id": ID}, {}))
        client.emails.send({"from": "a@acme.test", "to": "b@example.test", "subject": "s", "text": "x"})
        self.assertIsNone(self.mock.requests[0].headers.get("Idempotency-Key"))

    def test_get_list_cancel(self):
        def handler(r):
            if r.method == "GET" and r.path == f"/emails/{ID}":
                return 200, email(["delivered"]), {}
            if r.method == "GET" and r.path == "/emails":
                return 200, {"object": "list", "data": [{"id": ID}], "has_more": True, "next_before": ID}, {}
            if r.method == "POST" and r.path == f"/emails/{ID}/cancel":
                return 200, {"object": "email", "id": ID}, {}
            return None

        client = self.serve(handler)
        self.assertEqual(client.emails.get(ID)["recipients"][0]["status"], "delivered")
        page = client.emails.list(limit=10, before=ID, api_key_id="k1")
        self.assertTrue(page["has_more"])
        self.assertEqual(self.mock.requests[1].raw_query, f"limit=10&before={ID}&api_key_id=k1")
        client.emails.list()
        self.assertEqual(self.mock.requests[2].raw_query, "")
        self.assertEqual(client.emails.cancel(ID)["id"], ID)

    def test_batch(self):
        client = self.serve(lambda r: (200, {"data": [{"id": "1"}, {"id": "2"}]}, {}))
        res = client.emails.batch(
            [
                {"from": "a@acme.test", "to": "x@example.test", "subject": "1", "text": "one"},
                {"from": "a@acme.test", "to": ["y@example.test"], "subject": "2", "text": "two"},
            ],
            idempotency_key="nightly",
        )
        self.assertEqual([d["id"] for d in res["data"]], ["1", "2"])
        r = self.mock.requests[0]
        self.assertEqual(r.path, "/emails/batch")
        self.assertEqual(r.headers["Idempotency-Key"], "nightly")
        self.assertIsInstance(r.json, list)
        self.assertEqual(r.json[1]["subject"], "2")

    def test_send_raw(self):
        client = self.serve(lambda r: (200, {"id": ID}, {}))
        raw = b"From: a@acme.test\r\nTo: x@example.test\r\nSubject: hi\r\n\r\nbody\r\n"
        client.emails.send_raw(raw, mail_from="a@acme.test", rcpts=["x@example.test"], idempotency_key="r1")
        r = self.mock.requests[0]
        self.assertEqual(r.path, "/emails/raw")
        self.assertEqual(r.headers["Idempotency-Key"], "r1")
        self.assertEqual(base64.b64decode(r.json["raw"]), raw)
        self.assertEqual(r.json["mail_from"], "a@acme.test")
        self.assertEqual(r.json["rcpts"], ["x@example.test"])
        client.emails.send_raw(raw.decode())
        self.assertEqual(self.mock.requests[1].json, {"raw": base64.b64encode(raw).decode()})

    # ------------------------------------------------------------ domains, webhooks, keys

    def test_domains(self):
        dom = {"object": "domain", "id": ID, "name": "acme.test", "status": "pending", "records": []}

        def handler(r):
            if r.path == "/domains" and r.method == "POST":
                return 201, dom, {}
            if r.path == "/domains":
                return 200, {"object": "list", "data": [dom]}, {}
            if r.path == f"/domains/{ID}/verify" and r.method == "POST":
                return 200, dict(dom, status="verified"), {}
            if r.path == f"/domains/{ID}" and r.method == "DELETE":
                return 200, {"object": "domain", "id": ID, "deleted": True}, {}
            if r.path == f"/domains/{ID}":
                return 200, dom, {}
            return None

        client = self.serve(handler)
        self.assertEqual(client.domains.create("acme.test")["name"], "acme.test")
        self.assertEqual(self.mock.requests[0].json, {"name": "acme.test"})
        self.assertEqual(len(client.domains.list()["data"]), 1)
        self.assertEqual(client.domains.get(ID)["id"], ID)
        self.assertEqual(client.domains.verify(ID)["status"], "verified")
        self.assertTrue(client.domains.delete(ID)["deleted"])

    def test_webhooks(self):
        hook = {"object": "webhook", "id": ID, "endpoint": "https://h.test/x", "events": ["email.bounced"]}

        def handler(r):
            if r.path == "/webhooks" and r.method == "POST":
                return 201, dict(hook, signing_secret="whsec_abc"), {}
            if r.path == "/webhooks":
                return 200, {"object": "list", "data": [hook]}, {}
            if r.path == f"/webhooks/{ID}" and r.method == "DELETE":
                return 200, {"object": "webhook", "id": ID, "deleted": True}, {}
            if r.path == f"/webhooks/{ID}":
                return 200, dict(hook, recent_deliveries=[]), {}
            return None

        client = self.serve(handler)
        self.assertEqual(client.webhooks.create("https://h.test/x", ["email.bounced"])["signing_secret"], "whsec_abc")
        self.assertEqual(self.mock.requests[0].json, {"endpoint": "https://h.test/x", "events": ["email.bounced"]})
        client.webhooks.create("https://h.test/all")
        self.assertEqual(self.mock.requests[1].json, {"endpoint": "https://h.test/all"})
        self.assertEqual(client.webhooks.list()["data"][0]["id"], ID)
        self.assertEqual(client.webhooks.get(ID)["recent_deliveries"], [])
        self.assertTrue(client.webhooks.delete(ID)["deleted"])

    def test_api_keys(self):
        key = {"id": ID, "name": "agent", "prefix": "omw_ab", "permission": "sending_access", "policy": {}}

        def handler(r):
            if r.path == "/api-keys" and r.method == "POST":
                return 201, dict(key, token="omw_secret"), {}
            if r.path == "/api-keys":
                return 200, {"object": "list", "data": [key]}, {}
            if r.path == f"/api-keys/{ID}" and r.method == "PATCH":
                return 200, dict(key, **r.json), {}
            if r.path == f"/api-keys/{ID}" and r.method == "DELETE":
                return 200, {"object": "api_key", "id": ID, "deleted": True}, {}
            if r.path == f"/api-keys/{ID}":
                return 200, key, {}
            return None

        client = self.serve(handler)
        policy = {"max_per_hour": 10, "allowed_recipients": ["*@acme.test"]}
        created = client.api_keys.create("agent", permission="sending_access", policy=policy)
        self.assertEqual(created["token"], "omw_secret")
        self.assertEqual(self.mock.requests[0].json, {"name": "agent", "permission": "sending_access", "policy": policy})
        self.assertEqual(client.api_keys.list()["data"][0]["prefix"], "omw_ab")
        self.assertEqual(client.api_keys.get(ID)["name"], "agent")
        self.assertEqual(client.api_keys.update(ID, name="renamed")["name"], "renamed")
        self.assertEqual(self.mock.requests[3].json, {"name": "renamed"})
        client.api_keys.update(ID, policy={})
        self.assertEqual(self.mock.requests[4].json, {"policy": {}})
        self.assertTrue(client.api_keys.delete(ID)["deleted"])

    # ------------------------------------------------------------ inboxes and messages

    def test_inboxes(self):
        inbox = {"id": "i1", "address": "agent@acme.test", "created_at": "2026-01-01T00:00:00Z"}

        def handler(r):
            if r.path == "/inboxes" and r.method == "POST":
                return 201, inbox, {}
            if r.path == "/inboxes":
                return 200, {"object": "list", "data": [inbox]}, {}
            if r.path == "/inboxes/i1" and r.method == "DELETE":
                return 200, {"object": "inbox", "id": "i1", "deleted": True}, {}
            if r.path == "/inboxes/i1/messages":
                return 200, {"data": [{"id": "m1"}], "has_more": False, "next_before": None}, {}
            return None

        client = self.serve(handler)
        self.assertEqual(client.inboxes.create("agent@acme.test")["address"], "agent@acme.test")
        self.assertEqual(self.mock.requests[0].json, {"address": "agent@acme.test"})
        self.assertEqual(len(client.inboxes.list()["data"]), 1)
        page = client.inboxes.list_messages("i1", limit=5, thread_id="t1")
        self.assertEqual(page["data"][0]["id"], "m1")
        self.assertEqual(self.mock.requests[2].raw_query, "limit=5&thread_id=t1")
        self.assertTrue(client.inboxes.delete("i1")["deleted"])

    def test_messages(self):
        blob = bytes([0, 1, 2, 250, 255])

        def handler(r):
            if r.path == "/messages/m1":
                return 200, {"id": "m1", "subject": "Re: hi", "attachments": []}, {}
            if r.path == "/messages/m1/attachments/0":
                return 200, blob, {}
            if r.path == "/messages/m1/reply" and r.method == "POST":
                return 200, {"id": ID}, {}
            return None

        client = self.serve(handler)
        self.assertEqual(client.messages.get("m1")["subject"], "Re: hi")
        self.assertEqual(client.messages.get_attachment("m1", 0), blob)
        self.assertEqual(client.messages.reply("m1", text="thanks"), {"id": ID})
        self.assertEqual(self.mock.requests[2].json, {"text": "thanks"})

    # ------------------------------------------------------------ errors

    def test_api_error(self):
        client = self.serve(
            lambda r: (422, {"statusCode": 422, "name": "validation_error", "message": "subject is required"}, {})
        )
        with self.assertRaises(OpenMailWireError) as cm:
            client.emails.send({"from": "a@acme.test", "to": "b@example.test", "subject": "", "text": "x"})
        e = cm.exception
        self.assertEqual((e.status_code, e.name, e.message, e.retry_after), (422, "validation_error", "subject is required", None))
        self.assertEqual(str(e), "422 validation_error: subject is required")

    def test_rate_limit_retry_after(self):
        client = self.serve(
            lambda r: (
                429,
                {"statusCode": 429, "name": "rate_limit_exceeded", "message": "hourly cap reached"},
                {"Retry-After": "120"},
            )
        )
        with self.assertRaises(OpenMailWireError) as cm:
            client.emails.send({"from": "a@acme.test", "to": "b@example.test", "subject": "s", "text": "x"})
        self.assertEqual((cm.exception.status_code, cm.exception.name, cm.exception.retry_after), (429, "rate_limit_exceeded", 120))

    def test_non_json_error(self):
        client = self.serve(lambda r: (502, "Bad Gateway", {}))
        with self.assertRaises(OpenMailWireError) as cm:
            client.domains.list()
        self.assertEqual((cm.exception.status_code, cm.exception.name, cm.exception.message), (502, "http_error", "Bad Gateway"))

    def test_redirects_are_refused(self):
        # R4#10: the key must never follow a redirect to another address.
        other = MockServer(lambda r: (200, {"object": "list", "data": []}, {}))
        self.addCleanup(other.close)
        client = self.serve(lambda r: (307, "", {"Location": other.url + "/domains"}))
        with self.assertRaises(OpenMailWireError) as cm:
            client.domains.list()
        self.assertEqual((cm.exception.status_code, cm.exception.name), (307, "redirect_refused"))
        self.assertIn(other.url, cm.exception.message)
        self.assertEqual(other.requests, [])
        self.assertEqual(self.mock.requests[0].headers["Authorization"], "Bearer " + KEY)

        # An opener that does follow redirects still never forwards the key.
        import urllib.request

        following = OpenMailWire(api_key=KEY, base_url=self.mock.url, opener=urllib.request.build_opener())
        following.domains.list()
        self.assertEqual(len(other.requests), 1)
        self.assertIsNone(other.requests[0].headers.get("Authorization"))

    def test_connection_error(self):
        srv = MockServer(lambda r: None)
        url = srv.url
        srv.close()
        client = OpenMailWire(api_key=KEY, base_url=url, timeout=5)
        with self.assertRaises(OpenMailWireError) as cm:
            client.domains.list()
        self.assertEqual((cm.exception.status_code, cm.exception.name), (0, "connection_error"))

    # ------------------------------------------------------------ waiting

    def test_wait_for_delivery(self):
        states = [["queued", "queued"], ["sending", "deferred"], ["delivered", "bounced"]]
        calls = []

        def handler(r):
            calls.append(r.path)
            return 200, email(states[min(len(calls) - 1, len(states) - 1)]), {}

        client = self.serve(handler)
        e = client.wait_for_delivery(ID, timeout=5, interval=0.01)
        self.assertEqual([r["status"] for r in e["recipients"]], ["delivered", "bounced"])
        self.assertEqual(calls, [f"/emails/{ID}"] * 3)

    def test_wait_for_delivery_timeout(self):
        client = self.serve(lambda r: (200, email(["deferred"]), {}))
        with self.assertRaises(WaitTimeoutError) as cm:
            client.wait_for_delivery(ID, timeout=0.05, interval=0.01)
        self.assertEqual(cm.exception.email["recipients"][0]["status"], "deferred")
        self.assertIsInstance(cm.exception, TimeoutError)

    def test_wait_for_reply(self):
        polls = []

        def handler(r):
            if r.path == "/inboxes":
                return 200, {"object": "list", "data": [{"id": "i1"}, {"id": "i2"}]}, {}
            if r.path == "/inboxes/i1/messages":
                return 200, {"data": [{"id": "m0", "in_reply_to_email_id": "other"}], "has_more": False}, {}
            if r.path == "/inboxes/i2/messages":
                self.assertEqual(r.query["limit"], "50")
                polls.append(1)
                data = [] if len(polls) < 2 else [{"id": "m1", "in_reply_to_email_id": ID, "received_at": "2026-01-01T00:00:00Z"}]
                return 200, {"data": data, "has_more": False}, {}
            return None

        client = self.serve(handler)
        self.assertEqual([m["id"] for m in client.wait_for_reply(ID, timeout=5, interval=0.01)], ["m1"])

    def test_wait_for_reply_timeout_one_inbox(self):
        client = self.serve(lambda r: (200, {"data": [], "has_more": False}, {}) if r.path == "/inboxes/i9/messages" else None)
        self.assertEqual(client.wait_for_reply(ID, timeout=0.05, interval=0.01, inbox_id="i9"), [])
        self.assertTrue(all(r.path == "/inboxes/i9/messages" for r in self.mock.requests))


if __name__ == "__main__":
    unittest.main()
