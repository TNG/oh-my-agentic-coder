"""Local reverse proxy that compacts the strix scan's context in flight.

strix (via the openai-agents SDK) resends the entire conversation on every
request and never compacts it, so a long whole-repo scan grows past the gateway
model's context window (GLM-5.2: 202752 tokens) and then every request 422s with
``context_length_exceeded`` -- silently truncating the scan (strix crashes but
leaves a partial run dir, so the workflow can mistake it for a finished scan).

strix is a third-party CLI we don't own the call site of, so this proxy fixes it
at the HTTP layer: strix points ``LLM_API_BASE`` here, and every chat-completions
request is compacted before being forwarded (see :mod:`context_compaction`) --
oldest exchanges are summarised by the model itself and replaced with a briefing,
keeping the request under the window while preserving the system prefix, recent
turns, and tool-call pairing.

Unlike the sibling asml proxy this one does no token minting: strix already
sends a working bearer, so the proxy passes ``Authorization`` straight through
and reuses that same bearer (and the request's own ``model``) for the summariser
call it originates. Only the upstream host needs configuring (``SKAINET_UPSTREAM``).

Dependency-free beyond what ``pip install strix-agent`` already pulls in (httpx,
starlette, uvicorn all land transitively via openai-agents/mcp).
"""

from __future__ import annotations

import json
import logging
import os
import sys
from pathlib import Path
from typing import Any, Callable

import httpx
import uvicorn
from starlette.applications import Starlette
from starlette.concurrency import run_in_threadpool
from starlette.requests import Request
from starlette.responses import JSONResponse, Response, StreamingResponse
from starlette.routing import Route

sys.path.insert(0, str(Path(__file__).resolve().parent))
from context_compaction import compact_messages  # noqa: E402

logger = logging.getLogger("skainet_proxy")

UPSTREAM_BASE = os.environ["SKAINET_UPSTREAM"]  # bare host, e.g. https://external.model.tngtech.com
CHAT_PATH = os.environ.get("SKAINET_CHAT_PATH", "/v1/chat/completions")

_HOP_BY_HOP = {"connection", "content-encoding", "content-length", "transfer-encoding", "host"}


def _int_env(name: str, default: int) -> int:
    try:
        return int(os.environ[name])
    except (KeyError, ValueError):
        return default


def _float_env(name: str, default: float) -> float:
    try:
        return float(os.environ[name])
    except (KeyError, ValueError):
        return default


_COMPACT_ENABLED = os.environ.get("STRIX_COMPACT_ENABLED", "1") != "0"
_MODEL_CONTEXT = _int_env("STRIX_MODEL_CONTEXT_TOKENS", 202752)
_COMPACT_TRIGGER = _int_env("STRIX_COMPACT_TRIGGER_TOKENS", 180_000)
_COMPACT_TARGET = _int_env("STRIX_COMPACT_TARGET_TOKENS", 150_000)
_COMPACT_KEEP_RECENT = _int_env("STRIX_COMPACT_KEEP_RECENT_TURNS", 3)
_COMPACT_CHARS_PER_TOKEN = _float_env("STRIX_COMPACT_CHARS_PER_TOKEN", 3.5)
_SUMMARY_MAX_TOKENS = _int_env("STRIX_COMPACT_SUMMARY_MAX_TOKENS", 1200)

_SUMMARY_SYSTEM_PROMPT = (
    "You are compacting the transcript of an autonomous security-pentest agent "
    "so it fits the model context window. Summarise the messages below into a "
    "dense briefing that preserves everything needed to continue the scan: "
    "files, endpoints, and components already examined; hypotheses raised; "
    "vulnerabilities confirmed or ruled out; credentials, tokens, or IDs "
    "discovered; and any pending next steps. Omit raw file dumps and verbose "
    "command output, keep the security-relevant conclusions. Be terse and "
    "factual; use short bullet points."
)

_client = httpx.AsyncClient(base_url=UPSTREAM_BASE, timeout=httpx.Timeout(300.0))
_summary_client = httpx.Client(base_url=UPSTREAM_BASE, timeout=httpx.Timeout(120.0))


def _render_for_summary(messages: list[dict[str, Any]], *, per_message_cap: int = 3000) -> str:
    """Flatten evicted messages into bounded plain text for the summariser."""
    lines: list[str] = []
    for message in messages:
        role = message.get("role", "?")
        content = message.get("content")
        if isinstance(content, str) and content:
            text = content
        elif message.get("tool_calls"):
            calls = "; ".join(
                f"{call.get('function', {}).get('name')}"
                f"({str(call.get('function', {}).get('arguments', ''))[:200]})"
                for call in message["tool_calls"]
            )
            text = f"[tool calls] {calls}"
        else:
            text = json.dumps(content, ensure_ascii=False) if content is not None else ""
        lines.append(f"{role}: {text[:per_message_cap]}")
    return "\n\n".join(lines)


def _summarize_via_gateway(evicted: list[dict[str, Any]], *, model: str, authorization: str) -> str:
    """Summarise evicted messages with one direct (non-proxied) gateway call,
    reusing the bearer and model of the request being compacted."""
    if not authorization:
        raise RuntimeError("no Authorization header to reuse for summarisation")
    payload = {
        "model": model,
        "messages": [
            {"role": "system", "content": _SUMMARY_SYSTEM_PROMPT},
            {"role": "user", "content": _render_for_summary(evicted)},
        ],
        "temperature": 0,
        "max_tokens": _SUMMARY_MAX_TOKENS,
        "stream": False,
    }
    resp = _summary_client.post(CHAT_PATH, json=payload, headers={"authorization": authorization})
    resp.raise_for_status()
    return resp.json()["choices"][0]["message"]["content"] or ""


def _summarizer_for(model: str, authorization: str) -> Callable[[list[dict[str, Any]]], str]:
    def summarize(evicted: list[dict[str, Any]]) -> str:
        return _summarize_via_gateway(evicted, model=model, authorization=authorization)

    return summarize


def _maybe_compact(body: bytes, path: str, authorization: str) -> bytes:
    """Compact a chat-completions request body if it is over the window.

    Any parsing/compaction failure returns the original body unchanged -- the
    proxy must never turn a working request into a broken one.
    """
    if not (_COMPACT_ENABLED and path.endswith("/chat/completions")):
        return body
    try:
        parsed = json.loads(body)
    except (ValueError, TypeError):
        return body
    messages = parsed.get("messages")
    if not isinstance(messages, list):
        return body
    try:
        new_messages, stats = compact_messages(
            messages,
            summarize=_summarizer_for(parsed.get("model", ""), authorization),
            limit=_MODEL_CONTEXT,
            trigger=_COMPACT_TRIGGER,
            target=_COMPACT_TARGET,
            keep_recent_turns=_COMPACT_KEEP_RECENT,
            chars_per_token=_COMPACT_CHARS_PER_TOKEN,
        )
    except Exception:  # noqa: BLE001 - degrade to the uncompacted request
        logger.warning("compaction failed; forwarding request unchanged", exc_info=True)
        return body
    if stats is None:
        return body
    logger.info("compacted request: %s", stats)
    parsed["messages"] = new_messages
    return json.dumps(parsed).encode()


async def healthz(request: Request) -> Response:
    return JSONResponse({"ok": True})


async def proxy(request: Request) -> Response:
    headers = {k: v for k, v in request.headers.items() if k.lower() not in _HOP_BY_HOP}
    body = await request.body()
    body = await run_in_threadpool(
        _maybe_compact, body, request.url.path, request.headers.get("authorization", "")
    )

    upstream_req = _client.build_request(
        request.method,
        request.url.path,
        headers=headers,
        params=request.query_params,
        content=body,
    )
    upstream_resp = await _client.send(upstream_req, stream=True)

    async def relay() -> object:
        async for chunk in upstream_resp.aiter_raw():
            yield chunk
        await upstream_resp.aclose()

    response_headers = {
        k: v for k, v in upstream_resp.headers.items() if k.lower() not in _HOP_BY_HOP
    }
    return StreamingResponse(
        relay(),
        status_code=upstream_resp.status_code,
        headers=response_headers,
    )


app = Starlette(
    routes=[
        Route("/healthz", healthz, methods=["GET"]),
        Route("/{path:path}", proxy, methods=["GET", "POST"]),
    ]
)


if __name__ == "__main__":
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    port = int(os.environ.get("SKAINET_PROXY_PORT", "8899"))
    uvicorn.run(app, host="127.0.0.1", port=port, log_level="info")
