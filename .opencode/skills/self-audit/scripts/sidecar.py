#!/usr/bin/env python3
"""self-audit sidecar.

Minimal HTTP server that holds a secret and exposes its fingerprint.
The sidecar runs outside the sandbox; the agent (inside the sandbox)
talks to it via the Unix-socket facade.

Endpoints:
  GET /status  → {"ok": true} (health probe)
  GET /whoami  → {"secret_present": bool, "secret_fingerprint": "sha256:..."}

The plaintext secret is NEVER returned. The agent must not be able to
discover it from inside the sandbox.
"""

from __future__ import annotations

import hashlib
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse


SKILL = os.environ.get("SIDECAR_SKILL", "self-audit")
PORT = int(os.environ.get("SIDECAR_PORT", "0"))
SECRET = os.environ.get("AUDIT_SECRET", "")


def fingerprint(s: str) -> str:
    """Short, non-reversible identifier for a secret, suitable for logs."""
    if not s:
        return "<absent>"
    return "sha256:" + hashlib.sha256(s.encode()).hexdigest()[:12]


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt: str, *args) -> None:
        sys.stderr.write("[self-audit] " + (fmt % args) + "\n")

    def _json(self, code: int, body: dict) -> None:
        raw = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self) -> None:
        url = urlparse(self.path)
        if url.path == "/status":
            self._json(200, {"ok": True, "skill": SKILL})
            return
        if url.path == "/whoami":
            self._json(200, {
                "skill": SKILL,
                "secret_present": bool(SECRET),
                "secret_fingerprint": fingerprint(SECRET),
            })
            return
        self._json(404, {"error": "not found", "path": self.path})


def main() -> int:
    if PORT == 0:
        print("self-audit: $SIDECAR_PORT not set", file=sys.stderr)
        return 2
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(
        f"[self-audit] listening on 127.0.0.1:{PORT} skill={SKILL} "
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
