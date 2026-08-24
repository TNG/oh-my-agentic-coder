#!/usr/bin/env python3
"""Classify e2e-smoke compatibility failures from per-leg .ctx logs.

The e2e-smoke workflow writes one ``<slug>.ctx`` file per failing leg,
each starting with a header line::

    === <harness> / <os> / omac@<omac> — failing stage(s): <stages> ===

followed by log excerpts. This script reads every ``.ctx`` in a directory,
names the concrete failure reason for each leg from those excerpts, and
reports whether the whole run failed only for infrastructure reasons
(model backend down, auth, rate-limit, timeout) rather than a real
harness-contract regression.

It is deliberately independent of the SKAINET AI summariser: that
summariser calls the same model backend that is unavailable in the most
common infra failure, so it cannot explain precisely that case. This
classifier reads the logs directly and always produces detail.

Usage::

    classify-compat-failure.py --ctx-dir compat --out failure-reason.txt

Writes the human-readable reason lines to ``--out`` and prints two
``key=value`` lines to stdout for the workflow to append to
``$GITHUB_OUTPUT``::

    infra_only=true|false
    reason_count=<n>
"""

from __future__ import annotations

import argparse
import glob
import os
import re
import sys

HEADER_RE = re.compile(
    r"^=== (?P<harness>.+?) / (?P<os>.+?) / omac@(?P<omac>.+?) "
    r"— failing stage\(s\):(?P<stages>.*?) ===\s*$",
    re.MULTILINE,
)

# Each category: (code, friendly label, is_infra, detect regex, [clean-excerpt regexes]).
# Ordering rationale:
#   - CONTRACT_DRIFT first: a real CLI regression that also triggered a
#     downstream model error must be reported as the contract break, not
#     downplayed as infra.
#   - All infra categories (AUTH, RATE_LIMIT, LLM_UNAVAILABLE, MODEL_NOT_FOUND,
#     TIMEOUT) before LAUNCH_FAIL. Rationale: when the model backend fails
#     (401/403/429/5xx/404/timeout), the agent process exits non-zero and the
#     Go test prints the generic "omac start failed: exit status 1" symptom
#     always classified as LAUNCH_FAIL, instead of the actual infra failure.
#     A genuine sandbox-launch failure (bwrap missing, profile corrupt) fails
#     fast and produces NO model-call logs, so no infra category matches and
#     LAUNCH_FAIL still wins. The launch probe (TestHarnessLaunchProbe) is a
#     separate stage — if it failed, its own error string is in the .ctx and
#     no model-error strings are present, so the fallthrough to LAUNCH_FAIL is
#     correct there too.
#   - MODEL_NOT_FOUND (404) is placed below LLM_UNAVAILABLE so a 5xx with a
#     trailing 404 on retry is labeled as the 5xx, not the 404. Its is_infra
#     is True (a 404 from the model endpoint is almost always gateway
#     overload rejecting the model, as observed co-occurring with codex 5xx
#     in the same run), but the label carries the ambiguity so a human can
#     verify whether it's actually a config bug (wrong baseURL).
#   - LAUNCH_FAIL drops to last: it is the residual "agent exited non-zero
#     for an unknown reason" bucket, only reached when no model-error pattern
#     matched.
CATEGORIES = [
    (
        "CONTRACT_DRIFT",
        "harness CLI contract changed (real regression)",
        False,
        re.compile(r"CLI contract drift|no longer exposes", re.IGNORECASE),
        [re.compile(r"(?:no longer exposes|CLI contract drift)[^\n]*", re.IGNORECASE)],
    ),
    (
        "AUTH",
        "model auth rejected (infra)",
        True,
        re.compile(r"\b401\b|\b403\b|unauthorized|forbidden|invalid[_ ]?(?:api[_ ]?key|token)|authentication failed", re.IGNORECASE),
        [re.compile(r"(?:401|403|[Uu]nauthorized|[Ff]orbidden)[^\n]*")],
    ),
    (
        "RATE_LIMIT",
        "model rate-limited (infra)",
        True,
        re.compile(r"\b429\b|rate[ _-]?limit|too many requests", re.IGNORECASE),
        [re.compile(r"(?:429|[Rr]ate[ _-]?limit)[^\n]*")],
    ),
    (
        "LLM_UNAVAILABLE",
        "LLM backend unavailable (infra)",
        True,
        re.compile(
            r"not available|Supported model names are:\s*set\(\)|AI_APICallError"
            r"|stream error|no supported model|model[^\n]*unavailable|does not exist"
            # Terminal errors only to avoid error classification on recovery through internal retry
            r"|high demand"                              # codex: "We're currently experiencing high demand"
            r"|Failed to get response from the AI model" # copilot: "Failed to get response from the AI model; retried N times"
            r"|Last error: 5\d\d"                        # generic 5xx: "Last error: 500 ..."
            r"|model inference should recover"           # copilot: "model inference should recover automatically"
            , re.IGNORECASE,
        ),
        [
            re.compile(r"Requested model name '[^']+' is currently not available"),
            re.compile(r"Supported model names are:\s*set\(\)"),
            re.compile(r"AI_APICallError:[^\n\"]+"),
            re.compile(r"stream error[^\n]*"),
            re.compile(r"We're currently experiencing high demand[^\n]*"),
            re.compile(r"Failed to get response from the AI model[^\n]*"),
            re.compile(r"Last error: 5\d\d [^\n]*"),
        ],
    ),
    (
        "MODEL_NOT_FOUND",
        "model endpoint not found (404 — likely infra overload, possibly config)",
        True,
        re.compile(r"Invalid request \(404\)|exec turn failed", re.IGNORECASE),
        [
            re.compile(r"error: Invalid request \(404\)[^\n]*"),
            re.compile(r"exec turn failed[^\n]*")
        ],
    ),
    (
        "TIMEOUT",
        "agent/model timed out (infra)",
        True,
        re.compile(r"did not exit within|timed out|context deadline exceeded", re.IGNORECASE),
        [re.compile(r"[^\n]*(?:did not exit within|timed out|context deadline exceeded)[^\n]*", re.IGNORECASE)],
    ),
    (
        "LAUNCH_FAIL",
        "sandbox launch failed (real regression)",
        False,
        re.compile(r"omac start .*failed|failed to start|launch probe failed", re.IGNORECASE),
        [re.compile(r"omac start [^\n]*failed[^\n]*", re.IGNORECASE)],
    ),
]

ANSI_RE = re.compile(r"\x1b\[[0-9;]*m")
MAX_EXCERPT = 180


def clean(line: str) -> str:
    line = ANSI_RE.sub("", line).strip()
    line = re.sub(r"\s+", " ", line)
    if len(line) > MAX_EXCERPT:
        line = line[: MAX_EXCERPT - 1] + "…"
    return line


def classify_block(body: str) -> tuple[str, str, bool]:
    """Return (label, excerpt, is_infra) for one leg's log body."""
    for _code, label, is_infra, detect, cleans in CATEGORIES:
        if not detect.search(body):
            continue
        excerpt = ""
        for pat in cleans:
            m = pat.search(body)
            if m:
                excerpt = clean(m.group(0))
                break
        if not excerpt:
            for raw in body.splitlines():
                if detect.search(raw):
                    excerpt = clean(raw)
                    break
        return label, excerpt, is_infra
    return "unrecognized failure — see run log", "", False


def split_blocks(text: str):
    headers = list(HEADER_RE.finditer(text))
    for i, h in enumerate(headers):
        start = h.end()
        end = headers[i + 1].start() if i + 1 < len(headers) else len(text)
        yield h, text[start:end]


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--ctx-dir", default="compat")
    ap.add_argument("--out", default="failure-reason.txt")
    args = ap.parse_args()

    text = ""
    for f in sorted(glob.glob(os.path.join(args.ctx_dir, "*.ctx"))):
        with open(f, encoding="utf-8", errors="replace") as fh:
            text += fh.read() + "\n"

    lines: list[str] = []
    infra_flags: list[bool] = []
    for h, body in split_blocks(text):
        label, excerpt, is_infra = classify_block(body)
        infra_flags.append(is_infra)
        loc = f"{h['harness'].strip()} / {h['os'].strip()} (omac@{h['omac'].strip()})"
        line = f"• {loc} — {label}"
        if excerpt:
            line += f": {excerpt}"
        lines.append(line)

    with open(args.out, "w", encoding="utf-8") as fh:
        if lines:
            fh.write("\n".join(lines) + "\n")

    infra_only = bool(infra_flags) and all(infra_flags)
    print(f"infra_only={'true' if infra_only else 'false'}")
    print(f"reason_count={len(lines)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
