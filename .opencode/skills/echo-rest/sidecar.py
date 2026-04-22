#!/usr/bin/env python3
"""echo-rest sidecar.

This is a trivial HTTP server used as a proof-of-concept for omac's
Unix-socket facade. It:

  - binds to 127.0.0.1 on the port given in $SIDECAR_PORT (set by omac),
  - exposes GET /status (health probe — facade waits on this),
  - exposes GET /whoami (returns skill name + a fingerprint of the secret,
    proving the secret was injected without leaking its value),
  - exposes POST /echo (returns the JSON body verbatim plus the same
    fingerprint),
  - exposes GET /tick?n=N (Server-Sent Events stream of N ticks, used to
    prove streaming works through the Unix-socket facade).

Secrets live in this process's env only. They are NEVER returned in full and
are NEVER forwarded into the sandbox; only the sandbox talks to us via the
Unix socket.
"""

from __future__ import annotations

import hashlib
import json
import os
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs


SKILL = os.environ.get("SIDECAR_SKILL", "echo-rest")
PORT = int(os.environ.get("SIDECAR_PORT", "0"))
SECRET = os.environ.get("ECHO_API_KEY", "")


def fingerprint(s: str) -> str:
    """Short, non-reversible identifier for a secret, suitable for logs."""
    if not s:
        return "<absent>"
    return "sha256:" + hashlib.sha256(s.encode()).hexdigest()[:12]


class Handler(BaseHTTPRequestHandler):
    # Quiet the default access log; omac writes its own access log.
    def log_message(self, fmt: str, *args) -> None:  # noqa: D401
        sys.stderr.write("[echo-rest] " + (fmt % args) + "\n")

    def _json(self, code: int, body: dict) -> None:
        raw = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self) -> None:  # noqa: N802
        url = urlparse(self.path)
        if url.path == "/status":
            self._json(200, {"ok": True, "skill": SKILL})
            return
        if url.path == "/whoami":
            self._json(
                200,
                {
                    "skill": SKILL,
                    "secret_present": bool(SECRET),
                    "secret_fingerprint": fingerprint(SECRET),
                    "pid": os.getpid(),
                },
            )
            return
        if url.path == "/tick":
            self._sse_stream(url)
            return
        self._json(404, {"error": "not found", "path": self.path})

    def _sse_stream(self, url) -> None:
        """Server-Sent Events endpoint.

        Query params:
          n        number of events to emit (default 3, max 100)
          gap_ms   delay between events in ms (default 50)

        Each event carries a small JSON payload containing the 1-based index,
        the sidecar's monotonic time, and the secret fingerprint (so the
        caller can confirm the secret was injected into the sidecar's env).
        """
        params = parse_qs(url.query)
        try:
            n = min(max(int(params.get("n", ["3"])[0]), 1), 100)
        except ValueError:
            n = 3
        try:
            gap = max(float(params.get("gap_ms", ["50"])[0]) / 1000.0, 0.0)
        except ValueError:
            gap = 0.05

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "keep-alive")
        # No Content-Length: we stream.
        self.end_headers()
        try:
            for i in range(1, n + 1):
                frame = (
                    f"event: tick\n"
                    f"id: {i}\n"
                    f"data: "
                    + json.dumps(
                        {
                            "n": i,
                            "of": n,
                            "mono": time.monotonic(),
                            "secret_fingerprint": fingerprint(SECRET),
                        }
                    )
                    + "\n\n"
                )
                self.wfile.write(frame.encode())
                self.wfile.flush()
                if i < n and gap > 0:
                    time.sleep(gap)
            # Final "done" sentinel so clients can stop reading cleanly.
            self.wfile.write(b"event: done\ndata: {}\n\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            # Client went away; that's fine for SSE.
            pass

    def do_POST(self) -> None:  # noqa: N802
        if self.path != "/echo":
            self._json(404, {"error": "not found", "path": self.path})
            return
        length = int(self.headers.get("Content-Length", "0") or "0")
        raw = self.rfile.read(length) if length > 0 else b""
        try:
            payload = json.loads(raw.decode() or "{}")
        except json.JSONDecodeError as exc:
            self._json(400, {"error": f"bad json: {exc}"})
            return
        self._json(
            200,
            {
                "skill": SKILL,
                "secret_fingerprint": fingerprint(SECRET),
                "you_sent": payload,
            },
        )


def main() -> int:
    if PORT == 0:
        print("echo-rest: $SIDECAR_PORT not set", file=sys.stderr)
        return 2
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(
        f"[echo-rest] listening on 127.0.0.1:{PORT} skill={SKILL} "
        f"secret={fingerprint(SECRET)}",
        file=sys.stderr,
    )
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
