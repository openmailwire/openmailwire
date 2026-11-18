"""A local HTTP server that records every request and answers from a handler,
so the SDK is tested without any real network service."""

import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable, Dict, List, Optional, Tuple
from urllib.parse import parse_qs, urlsplit

# Import the SDK from src/ without installing it.
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "src"))


class Request:
    def __init__(self, method: str, target: str, headers: Any, body: bytes) -> None:
        parts = urlsplit(target)
        self.method = method
        self.path = parts.path
        self.raw_query = parts.query
        self.query = {k: v[0] for k, v in parse_qs(parts.query).items()}
        self.headers = headers
        self.body = body
        try:
            self.json = json.loads(body) if body else None
        except ValueError:
            self.json = None


# A handler returns (status, body, headers) or None for 404. A dict or list
# body is sent as JSON; bytes or str are sent as they are.
Handler = Callable[[Request], Optional[Tuple[int, Any, Dict[str, str]]]]


class MockServer:
    def __init__(self, handler: Handler) -> None:
        self.handler = handler
        self.requests: List[Request] = []
        mock = self

        class _H(BaseHTTPRequestHandler):
            def log_message(self, *args: Any) -> None:
                pass

            def _serve(self) -> None:
                n = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(n) if n else b""
                req = Request(self.command, self.path, self.headers, body)
                mock.requests.append(req)
                out = mock.handler(req) or (404, {"statusCode": 404, "name": "not_found", "message": "no such endpoint"}, {})
                status, payload, headers = out
                if isinstance(payload, (bytes, str)):
                    data = payload.encode("utf-8") if isinstance(payload, str) else payload
                    ctype = "application/octet-stream"
                else:
                    data = json.dumps(payload).encode("utf-8")
                    ctype = "application/json"
                self.send_response(status)
                self.send_header("Content-Type", ctype)
                self.send_header("Content-Length", str(len(data)))
                for k, v in headers.items():
                    self.send_header(k, v)
                self.end_headers()
                self.wfile.write(data)

            do_GET = do_POST = do_PATCH = do_DELETE = do_PUT = _serve

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), _H)
        self.server.daemon_threads = True
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True)
        self.thread.start()

    def close(self) -> None:
        self.server.shutdown()
        self.server.server_close()
