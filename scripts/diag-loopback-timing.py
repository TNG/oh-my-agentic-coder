#!/usr/bin/env python3
"""Decompose the per-operation cost of scripts/probe-model_test.sh.

That test runs in ~25s on a Linux CI runner and ~700s on a macOS one, and the
test itself cannot say why: it is 69 loopback HTTP requests, 17 stub restarts
and 24 probe-model.sh invocations, any of which could carry the difference.

Each measurement below isolates one layer, so the macOS/Linux ratio per row
names the culprit:

  python_startup   interpreter spawn — the floor for every stub restart
  curl_spawn       `curl --version`, no socket — process-spawn floor
  getfqdn          socket.getfqdn("127.0.0.1"), the reverse lookup
                   HTTPServer.server_bind() performs on every bind
  server_bind      bind + listen + getfqdn, in-process
  stub_cold_start  spawn the stub and poll it with curl until it answers —
                   exactly what start_stub() does, 17 times
  curl_request     sequential curl POSTs against a live stub (client = curl)
  urllib_request   the same POSTs from python (client = python) — a gap
                   against curl_request is client-side, a match is server-side

Usage: scripts/diag-loopback-timing.py [--repeats N]
"""

import argparse
import json
import os
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

STUB = r'''
import json, os
from http.server import BaseHTTPRequestHandler, HTTPServer


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, code, payload):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self._send(200, {"data": [{"id": "diag/model"}]})

    def do_POST(self):
        n = int(self.headers.get("content-length", 0))
        self.rfile.read(n)
        self._send(200, {"choices": [{"message": {"content": "ok"}}]})


HTTPServer(("127.0.0.1", int(os.environ["STUB_PORT"])), H).serve_forever()
'''

BODY = json.dumps(
    {"model": "diag/model", "messages": [{"role": "user", "content": "ping"}], "max_tokens": 1}
)


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class Row:
    def __init__(self, name):
        self.name = name
        self.samples = []

    def add(self, seconds):
        self.samples.append(seconds)


def timed(row, fn, repeats):
    for _ in range(repeats):
        t0 = time.perf_counter()
        fn()
        row.add(time.perf_counter() - t0)
    return row


def measure_python_startup(repeats):
    row = Row("python_startup")
    return timed(row, lambda: subprocess.run([sys.executable, "-c", "pass"], check=True), repeats)


def measure_curl_spawn(repeats):
    row = Row("curl_spawn")
    return timed(
        row,
        lambda: subprocess.run(
            ["curl", "--version"], stdout=subprocess.DEVNULL, check=True
        ),
        repeats,
    )


def measure_getfqdn(repeats):
    row = Row("getfqdn")
    return timed(row, lambda: socket.getfqdn("127.0.0.1"), repeats)


def measure_server_bind(repeats):
    row = Row("server_bind")

    class H(BaseHTTPRequestHandler):
        pass

    def once():
        HTTPServer(("127.0.0.1", free_port()), H).server_close()

    return timed(row, once, repeats)


def start_stub(port):
    env = dict(os.environ, STUB_PORT=str(port))
    return subprocess.Popen([sys.executable, "-c", STUB], env=env)


def poll_ready(port, deadline=40):
    """Readiness poll copied from start_stub(): curl, no timeout, 0.1s apart."""
    url = "http://127.0.0.1:%d/models" % port
    for _ in range(deadline):
        r = subprocess.run(
            ["curl", "-s", "-o", os.devnull, url], stderr=subprocess.DEVNULL
        )
        if r.returncode == 0:
            return True
        time.sleep(0.1)
    return False


def measure_stub_cold_start(repeats):
    row = Row("stub_cold_start")
    for _ in range(repeats):
        port = free_port()
        t0 = time.perf_counter()
        proc = start_stub(port)
        ok = poll_ready(port)
        row.add(time.perf_counter() - t0)
        proc.terminate()
        proc.wait()
        if not ok:
            print("WARNING: stub never became ready", file=sys.stderr)
    return row


def measure_requests(repeats):
    port = free_port()
    proc = start_stub(port)
    try:
        if not poll_ready(port):
            print("WARNING: stub never became ready — request rows are void", file=sys.stderr)
        endpoint = "http://127.0.0.1:%d/chat/completions" % port

        curl_row = Row("curl_request")
        with tempfile.NamedTemporaryFile() as out:
            def one_curl():
                subprocess.run(
                    [
                        "curl", "-s", "-o", out.name, "-w", "%{http_code}",
                        "--max-time", "30", "-X", "POST", endpoint,
                        "-H", "content-type: application/json", "-d", BODY,
                    ],
                    stdout=subprocess.DEVNULL,
                    check=True,
                )

            timed(curl_row, one_curl, repeats)

        urllib_row = Row("urllib_request")

        def one_urllib():
            req = urllib.request.Request(
                endpoint, data=BODY.encode(), headers={"content-type": "application/json"}
            )
            with urllib.request.urlopen(req, timeout=30) as resp:
                resp.read()

        timed(urllib_row, one_urllib, repeats)
        return [curl_row, urllib_row]
    finally:
        proc.terminate()
        proc.wait()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--repeats", type=int, default=20)
    args = ap.parse_args()
    n = args.repeats

    print("platform: %s %s / python %s" % (os.uname().sysname, os.uname().machine,
                                           sys.version.split()[0]))
    rows = [
        measure_python_startup(max(3, n // 4)),
        measure_curl_spawn(n),
        measure_getfqdn(max(3, n // 4)),
        measure_server_bind(max(3, n // 4)),
        measure_stub_cold_start(max(3, n // 4)),
    ]
    rows += measure_requests(n)

    print()
    print("%-18s %4s %9s %9s %9s %9s" % ("measurement", "n", "total_s", "mean_ms", "min_ms", "max_ms"))
    for r in rows:
        s = r.samples
        print("%-18s %4d %9.3f %9.1f %9.1f %9.1f"
              % (r.name, len(s), sum(s), 1000 * sum(s) / len(s), 1000 * min(s), 1000 * max(s)))

    # The test's own shape, so the rows can be turned straight into a budget:
    # 69 requests, 17 stub restarts, 24 probe-model.sh invocations (each of
    # which spawns bash + resolve-model.sh + awk before its first request).
    by = {r.name: sum(r.samples) / len(r.samples) for r in rows}
    projected = 69 * by["curl_request"] + 17 * by["stub_cold_start"] + 20
    print()
    print("projected probe-model_test.sh wall clock: %.0fs "
          "(69 requests + 17 stub restarts + 20s of deliberate retry sleeps)" % projected)


if __name__ == "__main__":
    main()
