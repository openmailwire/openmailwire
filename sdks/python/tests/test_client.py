import base64
import os
import unittest
from datetime import datetime, timezone
from unittest import mock

from mock_server import MockServer  # also puts src/ on sys.path

from openmailwire import ErrorNames, OpenMailWire, OpenMailWireError, WaitTimeoutError

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

    # ------------------------------------------------------------ v0.6: approvals, checks, replies

    def test_send_stream_and_held(self):
        client = self.serve(lambda r: (200, {"id": ID, "held": True, "hold_reasons": ["first_contact"]}, {}))
        res = client.emails.send({"from": "a@acme.test", "to": "b@example.test", "subject": "s", "text": "x", "stream": "broadcast"})
        self.assertEqual(res, {"id": ID, "held": True, "hold_reasons": ["first_contact"]})
        self.assertEqual(self.mock.requests[0].json["stream"], "broadcast")

    def test_check(self):
        result = {
            "object": "email_check", "ok": False, "would": "refuse", "stream": "transactional", "size": 900,
            "hold_reasons": [], "refusals": [{"statusCode": 403, "name": "domain_not_verified", "message": "m"}], "warnings": [],
        }
        client = self.serve(lambda r: (200, result, {}) if r.path == "/emails/check" else None)
        res = client.emails.check(
            {"from": "a@acme.test", "to": "b@example.test", "subject": "s", "text": "x", "scheduled_at": datetime(2030, 1, 2)}
        )
        self.assertEqual(res["would"], "refuse")
        r = self.mock.requests[0]
        self.assertEqual(r.method, "POST")
        self.assertEqual(
            r.json,
            {"from": "a@acme.test", "to": "b@example.test", "subject": "s", "text": "x", "scheduled_at": "2030-01-02T00:00:00+00:00"},
        )

    def test_held_approve_reject_replies(self):
        def decision(d):
            return {"object": "email", "id": ID, "decision": d, "recipients": ["b@example.test"], "suppressed": []}

        def handler(r):
            if r.method == "GET" and r.path == "/emails/held":
                return 200, {"object": "list", "data": [{"object": "held_email", "id": ID}], "has_more": False, "next_before": None}, {}
            if r.method == "POST" and r.path == f"/emails/{ID}/approve":
                return 200, decision("approved"), {}
            if r.method == "POST" and r.path == f"/emails/{ID}/reject":
                return 200, decision("rejected"), {}
            if r.method == "GET" and r.path == f"/emails/{ID}/replies":
                return 200, {"object": "list", "email_id": ID, "thread_id": "t1", "data": [{"id": "m1", "trust": "failed"}]}, {}
            return None

        client = self.serve(handler)
        self.assertEqual(client.emails.list_held(limit=5, before="e0")["data"][0]["object"], "held_email")
        self.assertEqual(self.mock.requests[0].raw_query, "limit=5&before=e0")
        client.emails.list_held()
        self.assertEqual(self.mock.requests[1].raw_query, "")
        self.assertEqual(client.emails.approve(ID, note="looks fine")["decision"], "approved")
        self.assertEqual(self.mock.requests[2].json, {"note": "looks fine"})
        client.emails.approve(ID)
        self.assertEqual(self.mock.requests[3].json, {})
        self.assertEqual(client.emails.reject(ID, note="wrong customer")["decision"], "rejected")
        self.assertEqual(self.mock.requests[4].json, {"note": "wrong customer"})
        replies = client.emails.replies(ID)
        self.assertEqual((replies["thread_id"], replies["data"][0]["trust"]), ("t1", "failed"))

    # ------------------------------------------------------------ v0.6: suppressions, pauses, project

    def test_suppressions(self):
        row = {"address": "x@example.test", "stream": "all", "reason": "manual", "detail": "", "created_at": "2026-01-01T00:00:00Z"}
        csv = "address,stream,reason,detail,created_at\nx@example.test,all,manual,,2026-01-01T00:00:00Z\n"

        def handler(r):
            if r.method == "GET" and r.path == "/suppressions":
                return 200, {"object": "list", "data": [row], "has_more": True, "next_before": "c1"}, {}
            if r.method == "POST" and r.path == "/suppressions":
                return 201, dict(row, object="suppression", created=True), {}
            if r.method == "DELETE" and r.path == "/suppressions/x%40example.test":
                return 200, {"object": "suppression", "address": row["address"], "deleted": True, "removed": [row]}, {}
            if r.method == "GET" and r.path == "/suppressions/export":
                return 200, csv, {"Content-Type": "text/csv"}
            return None

        client = self.serve(handler)
        page = client.suppressions.list(
            limit=2, before="c0", reason="hard_bounce", stream="broadcast", address="x@example.test", domain="example.test"
        )
        self.assertEqual(page["next_before"], "c1")
        self.assertEqual(
            self.mock.requests[0].raw_query,
            "limit=2&before=c0&reason=hard_bounce&stream=broadcast&address=x%40example.test&domain=example.test",
        )
        created = client.suppressions.create("x@example.test", reason="customer_request", detail="by phone", stream="all")
        self.assertTrue(created["created"])
        self.assertEqual(
            self.mock.requests[1].json,
            {"address": "x@example.test", "reason": "customer_request", "detail": "by phone", "stream": "all"},
        )
        client.suppressions.create("x@example.test")
        self.assertEqual(self.mock.requests[2].json, {"address": "x@example.test"})
        self.assertEqual(len(client.suppressions.delete("x@example.test", stream="broadcast")["removed"]), 1)
        self.assertEqual(self.mock.requests[3].raw_query, "stream=broadcast")
        client.suppressions.delete("x@example.test")
        self.assertEqual(self.mock.requests[4].raw_query, "")
        self.assertEqual(client.suppressions.export(reason="complaint"), csv)
        self.assertEqual(self.mock.requests[5].raw_query, "reason=complaint")
        self.assertIn("text/csv", self.mock.requests[5].headers["Accept"])
        self.assertEqual(client.suppressions.export(), csv)

    def test_pauses(self):
        pause = {"object": "pause", "id": "p1", "scope": "domain", "scope_id": "d1", "reason": "bounce_rate", "active": True}

        def handler(r):
            if r.method == "GET" and r.path == "/pauses":
                return 200, {"object": "list", "data": [pause]}, {}
            if r.method == "POST" and r.path == "/pauses/p1/resume":
                return 200, dict(pause, active=False, resumed_by="k1"), {}
            return None

        client = self.serve(handler)
        self.assertEqual(client.pauses.list(active=True, limit=10)["data"][0]["scope"], "domain")
        self.assertEqual(self.mock.requests[0].raw_query, "active=true&limit=10")
        client.pauses.list(active=False)
        self.assertEqual(self.mock.requests[1].raw_query, "active=false")
        self.assertFalse(client.pauses.resume("p1")["active"])
        self.assertEqual(self.mock.requests[2].body, b"")

    def test_project(self):
        project = {"object": "project", "id": "pr1", "name": "acme", "max_per_recipient_per_day": 5, "circuit_breaker": {"enabled": True}}

        def handler(r):
            if r.method == "GET" and r.path == "/project":
                return 200, project, {}
            if r.method == "PATCH" and r.path == "/project":
                return 200, dict(project, **r.json), {}
            return None

        client = self.serve(handler)
        self.assertEqual(client.project.get()["max_per_recipient_per_day"], 5)
        updated = client.project.update(max_per_recipient_per_day=3, circuit_breaker={"bounce_rate": 0.02})
        self.assertEqual(updated["max_per_recipient_per_day"], 3)
        self.assertEqual(self.mock.requests[1].json, {"max_per_recipient_per_day": 3, "circuit_breaker": {"bounce_rate": 0.02}})
        client.project.update(max_per_recipient_per_day=None)
        self.assertEqual(self.mock.requests[2].body, b'{"max_per_recipient_per_day":null}')
        client.project.update(circuit_breaker={"enabled": False})
        self.assertEqual(self.mock.requests[3].json, {"circuit_breaker": {"enabled": False}})

    # ------------------------------------------------------------ v0.6: audit log, insights

    def test_audit_log(self):
        def handler(r):
            if r.path == "/audit-log":
                return 200, {"object": "list", "data": [{"object": "audit_entry", "id": 7}], "has_more": True, "next_before": "7"}, {}
            if r.path == "/audit-log/verify":
                return 200, {"object": "audit_verification", "valid": True, "entries": 7, "head_hash": "ab", "first_invalid_id": None}, {}
            return None

        client = self.serve(handler)
        self.assertEqual(client.audit_log.list(limit=1, before=9, action="api_key.created")["data"][0]["id"], 7)
        self.assertEqual(self.mock.requests[0].raw_query, "limit=1&before=9&action=api_key.created")
        self.assertTrue(client.audit_log.verify()["valid"])

    def test_insights(self):
        def window(name):
            return {"window": name, "total": {"attempts": 1}, "by_provider": [], "by_sending_ip": []}

        body = {"object": "insights", "emails_considered": 1, "truncated": False, "windows": [window("24h"), window("7d"), window("30d")]}
        client = self.serve(lambda r: (200, body, {}) if r.path == "/insights" else None)
        self.assertEqual([w["window"] for w in client.insights.get()["windows"]], ["24h", "7d", "30d"])
        self.assertEqual([w["window"] for w in client.insights.get(window="30d")["windows"]], ["30d"])
        self.assertEqual(self.mock.requests[1].raw_query, "")

    # ------------------------------------------------------------ v0.6: domains, webhooks, keys, inboxes

    def test_domain_dkim_and_reports(self):
        def state(obj, status=None):
            return {"object": obj, "status": status, "keys": [{"selector": "omw2", "algorithm": "rsa", "state": "pending", "signs": False}]}

        def handler(r):
            if r.method == "GET" and r.path == f"/domains/{ID}/dkim":
                return 200, state("dkim"), {}
            if r.method == "POST" and r.path == f"/domains/{ID}/dkim/rotate":
                return 202, state("dkim_rotation", "pending"), {}
            if r.method == "POST" and r.path == f"/domains/{ID}/dkim/ed25519":
                return 200, state("dkim_ed25519", "active" if r.json["enabled"] else "disabled"), {}
            if r.method == "GET" and r.path == f"/domains/{ID}/reports":
                return 200, {"object": "domain_reports", "domain_id": ID, "days": 7, "dmarc": {"pass": 3}, "tls": {}, "reports": []}, {}
            if r.method == "GET" and r.path == f"/domains/{ID}":
                return 200, {"object": "domain", "id": ID, "claim_expires_at": None, "report_addresses": {"dmarc_rua": "mailto:d@x"}}, {}
            return None

        client = self.serve(handler)
        self.assertEqual(client.domains.dkim(ID)["keys"][0]["selector"], "omw2")
        self.assertEqual(client.domains.rotate_dkim(ID)["status"], "pending")
        self.assertEqual(self.mock.requests[1].method, "POST")
        self.assertEqual(client.domains.set_ed25519(ID, True)["status"], "active")
        self.assertEqual(self.mock.requests[2].json, {"enabled": True})
        self.assertEqual(client.domains.set_ed25519(ID, False)["status"], "disabled")
        self.assertEqual(client.domains.reports(ID, days=7)["dmarc"]["pass"], 3)
        self.assertEqual(self.mock.requests[4].raw_query, "days=7")
        client.domains.reports(ID)
        self.assertEqual(self.mock.requests[5].raw_query, "")
        dom = client.domains.get(ID)
        self.assertIsNone(dom["claim_expires_at"])
        self.assertEqual(dom["report_addresses"]["dmarc_rua"], "mailto:d@x")

    def test_webhook_rotation_and_ed25519(self):
        hook = {"object": "webhook", "id": ID, "endpoint": "https://h.test/x", "asymmetric_signing": True, "public_key": "whpk_abc"}

        def handler(r):
            if r.method == "POST" and r.path == "/webhooks":
                return 201, dict(hook, signing_secret="whsec_1"), {}
            if r.method == "GET" and r.path == f"/webhooks/{ID}":
                return 200, hook, {}
            if r.method == "POST" and r.path == f"/webhooks/{ID}/rotate-secret":
                return 200, dict(hook, signing_secret="whsec_2", previous_public_key="whpk_abc"), {}
            return None

        client = self.serve(handler)
        client.webhooks.create("https://h.test/x", ["email.held", "sending.paused"], asymmetric_signing=True)
        self.assertEqual(
            self.mock.requests[0].json,
            {"endpoint": "https://h.test/x", "events": ["email.held", "sending.paused"], "asymmetric_signing": True},
        )
        self.assertEqual(client.webhooks.get(ID)["public_key"], "whpk_abc")
        self.assertEqual(client.webhooks.rotate_secret(ID, overlap_seconds=0)["signing_secret"], "whsec_2")
        self.assertEqual(self.mock.requests[2].json, {"overlap_seconds": 0})
        client.webhooks.rotate_secret(ID)
        self.assertEqual(self.mock.requests[3].json, {})

    def test_api_key_ips_expiry_and_approval_policy(self):
        client = self.serve(lambda r: (201 if r.method == "POST" else 200, dict(r.json or {}, id=ID), {}))
        policy = {"require_approval": {"first_contact": True, "recipients_over": 5}, "can_approve": False, "inboxes": ["s@acme.test"]}
        client.api_keys.create(
            "agent",
            permission="sending_access",
            policy=policy,
            allowed_ips=["10.0.0.0/8"],
            expires_at=datetime(2031, 1, 1, tzinfo=timezone.utc),
        )
        self.assertEqual(
            self.mock.requests[0].json,
            {
                "name": "agent",
                "permission": "sending_access",
                "policy": policy,
                "allowed_ips": ["10.0.0.0/8"],
                "expires_at": "2031-01-01T00:00:00+00:00",
            },
        )
        client.api_keys.update(ID, allowed_ips=None, expires_at=None)
        self.assertEqual(self.mock.requests[1].body, b'{"allowed_ips":null,"expires_at":null}')
        client.api_keys.update(ID, name="renamed")
        self.assertEqual(self.mock.requests[2].json, {"name": "renamed"})
        client.api_keys.update(ID, expires_at="2032-01-01T00:00:00Z", allowed_ips=["203.0.113.7"])
        self.assertEqual(self.mock.requests[3].json, {"allowed_ips": ["203.0.113.7"], "expires_at": "2032-01-01T00:00:00Z"})

    def test_inbox_get_update_and_hold(self):
        inbox = {"object": "inbox", "id": "i1", "address": "agent@acme.test", "hold_unauthenticated": True}

        def handler(r):
            if r.method == "POST" and r.path == "/inboxes":
                return 201, dict(inbox, **r.json), {}
            if r.method == "GET" and r.path == "/inboxes/i1":
                return 200, inbox, {}
            if r.method == "PATCH" and r.path == "/inboxes/i1":
                return 200, dict(inbox, **r.json), {}
            return None

        client = self.serve(handler)
        client.inboxes.create("agent@acme.test", hold_unauthenticated=False)
        self.assertEqual(self.mock.requests[0].json, {"address": "agent@acme.test", "hold_unauthenticated": False})
        self.assertTrue(client.inboxes.get("i1")["hold_unauthenticated"])
        self.assertFalse(client.inboxes.update("i1", hold_unauthenticated=False)["hold_unauthenticated"])
        self.assertEqual(self.mock.requests[2].json, {"hold_unauthenticated": False})

    def test_message_trust_release_and_reply_to(self):
        msg = {"id": "m1", "trust": "unauthenticated", "held": True, "suspicious_instructions": True, "authentication_results": {}}

        def handler(r):
            if r.method == "GET" and r.path == "/messages/m1":
                return 200, msg, {}
            if r.method == "POST" and r.path == "/messages/m1/release":
                return 200, dict(msg, held=False), {}
            if r.method == "POST" and r.path == "/messages/m1/reply":
                return 200, {"id": ID, "thread_id": "t1", "to": r.json["to"], "warnings": ["other domain"]}, {}
            return None

        client = self.serve(handler)
        m = client.messages.get("m1")
        self.assertEqual((m["trust"], m["held"], m["suspicious_instructions"]), ("unauthenticated", True, True))
        self.assertFalse(client.messages.release("m1")["held"])
        self.assertEqual(self.mock.requests[1].method, "POST")
        sent = client.messages.reply("m1", text="thanks", to=["a@example.test", "b@example.test"])
        self.assertEqual((sent["to"], sent["warnings"]), (["a@example.test", "b@example.test"], ["other domain"]))
        self.assertEqual(self.mock.requests[2].json, {"text": "thanks", "to": ["a@example.test", "b@example.test"]})

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

    def test_retry_after_for_recipient_cap_and_server_busy(self):
        for status, name in ((429, "recipient_rate_limited"), (503, "server_busy")):
            client = self.serve(lambda r, s=status, n=name: (s, {"statusCode": s, "name": n, "message": "wait"}, {"Retry-After": "10"}))
            with self.assertRaises(OpenMailWireError) as cm:
                client.emails.send({"from": "a@acme.test", "to": "b@example.test", "subject": "s", "text": "x"})
            e = cm.exception
            self.assertEqual((e.status_code, e.name, e.retry_after, e.retryable), (status, name, 10, True))
            self.assertEqual(len(self.mock.requests), 1)  # no retry by default

    def test_error_names(self):
        self.assertEqual(ErrorNames.SENDING_PAUSED, "sending_paused")
        self.assertEqual(ErrorNames.INSUFFICIENT_PERMISSION, "insufficient_permission")
        self.assertEqual(ErrorNames.CANNOT_APPROVE_OWN_EMAIL, "cannot_approve_own_email")
        self.assertEqual(ErrorNames.SERVER_BUSY, "server_busy")
        self.assertEqual(ErrorNames.DOMAIN_VERIFIED_ELSEWHERE, "domain_verified_elsewhere")
        self.assertEqual(ErrorNames.IP_NOT_ALLOWED, "ip_not_allowed")
        self.assertEqual(ErrorNames.API_KEY_EXPIRED, "api_key_expired")
        self.assertEqual(ErrorNames.WEBHOOK_LIMIT_REACHED, "webhook_limit_reached")
        self.assertEqual(ErrorNames.RECIPIENT_RATE_LIMITED, "recipient_rate_limited")
        self.assertFalse(OpenMailWireError(422, "validation_error", "x").retryable)

    def test_max_retries(self):
        answers = [
            (503, {"statusCode": 503, "name": "server_busy", "message": "busy"}, {"Retry-After": "0"}),
            (429, {"statusCode": 429, "name": "rate_limit_exceeded", "message": "slow"}, {"Retry-After": "0"}),
            (200, {"id": ID}, {}),
        ]
        self.mock = MockServer(lambda r: answers[len(self.mock.requests) - 1])
        self.addCleanup(self.mock.close)
        client = OpenMailWire(api_key=KEY, base_url=self.mock.url, max_retries=2)
        res = client.emails.send({"from": "a@acme.test", "to": "b@example.test", "subject": "s", "text": "x"}, idempotency_key="k1")
        self.assertEqual(res, {"id": ID})
        self.assertEqual(len(self.mock.requests), 3)
        self.assertTrue(all(r.headers["Idempotency-Key"] == "k1" and r.json["subject"] == "s" for r in self.mock.requests))

    def test_max_retries_gives_up(self):
        busy = {"statusCode": 503, "name": "server_busy", "message": "busy"}
        self.mock = MockServer(lambda r: (503, busy, {"Retry-After": "0"}))
        self.addCleanup(self.mock.close)
        with self.assertRaises(OpenMailWireError):
            OpenMailWire(api_key=KEY, base_url=self.mock.url, max_retries=1).domains.list()
        self.assertEqual(len(self.mock.requests), 2)

        long_wait = MockServer(lambda r: (503, busy, {"Retry-After": "3600"}))
        self.addCleanup(long_wait.close)
        with self.assertRaises(OpenMailWireError) as cm:
            OpenMailWire(api_key=KEY, base_url=long_wait.url, max_retries=3, max_retry_wait=5).domains.list()
        self.assertEqual(cm.exception.retry_after, 3600)
        self.assertEqual(len(long_wait.requests), 1)

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
