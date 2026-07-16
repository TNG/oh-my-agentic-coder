"""In-flight context-window compaction for the strix scan's chat traffic.

strix (via the openai-agents SDK) resends the *entire* conversation on every
request and has no compaction of its own, so a long whole-repo scan grows past
the gateway model's context window (GLM-5.2: 202752 tokens). Once it does, every
subsequent request 422s with ``context_length_exceeded`` -- and the failure is
unrecoverable by ``strix --resume``, which reloads the same over-length state
and overflows again immediately. This module compacts the ``messages`` array of
a chat-completions request before it is forwarded upstream: when the estimated
token count crosses ``trigger``, the oldest complete exchanges are evicted and
replaced by a single LLM-generated briefing (Tier 2), shrinking the request
back under ``target`` while preserving

* the leading ``system`` prefix (so the gateway's prompt cache still hits),
* the most recent turns verbatim (recency matters most to the agent),
* ``tool_calls`` -> ``tool`` message pairing (an orphaned ``tool`` message is a
  hard 400 from the API), by only ever evicting whole turns at a boundary.

The summariser is injected (``summarize`` callable) so this module stays pure
and unit-testable; :mod:`skainet_proxy` supplies the real gateway-backed one. If
the summariser raises or returns nothing, eviction still happens with a static
placeholder, so a summariser outage degrades coverage rather than crashing the
scan. As a last resort -- when even the minimum kept turns exceed the hard
``limit`` -- oversized ``tool`` result bodies are elided in place (Tier 1).
"""

from __future__ import annotations

import json
import logging
from typing import Any, Callable

logger = logging.getLogger("skainet_proxy.compaction")

Message = dict[str, Any]

# Rough chars-per-token for the size estimate. Deliberately low so the estimate
# runs slightly high and compaction triggers with headroom to spare rather than
# one request too late. Override via STRIX_COMPACT_CHARS_PER_TOKEN.
DEFAULT_CHARS_PER_TOKEN = 3.5
_PER_MESSAGE_OVERHEAD_TOKENS = 4

_SUMMARY_LABEL = (
    "[Context compacted to fit the model window. Briefing of earlier "
    "investigation steps that were summarised out of the transcript:]\n\n"
)
_SUMMARY_FALLBACK = (
    "[Earlier investigation steps were compacted out to fit the model context "
    "window; their raw transcript is unavailable. Continue from the most recent "
    "steps below and re-gather any earlier detail you still need.]"
)


def estimate_tokens(messages: list[Message], *, chars_per_token: float = DEFAULT_CHARS_PER_TOKEN) -> int:
    """Estimate the token footprint of a messages array.

    A serialization-length heuristic rather than a real tokenizer: the gateway
    owns the authoritative count, and compaction leaves tens of thousands of
    tokens of headroom, so an approximate-but-cheap estimate is sufficient and
    keeps this module dependency-free.
    """
    total = 0.0
    for message in messages:
        total += len(json.dumps(message, ensure_ascii=False, separators=(",", ":"))) / chars_per_token
        total += _PER_MESSAGE_OVERHEAD_TOKENS
    return int(total)


def _split_prefix(messages: list[Message]) -> tuple[list[Message], list[Message]]:
    """Peel the leading run of ``system`` messages off the front."""
    index = 0
    while index < len(messages) and messages[index].get("role") == "system":
        index += 1
    return messages[:index], messages[index:]


def _group_turns(messages: list[Message]) -> list[list[Message]]:
    """Group messages into turns so eviction never orphans a ``tool`` result.

    A turn starts at any non-``tool`` message; ``tool`` messages attach to the
    turn they answer. Evicting or keeping whole turns therefore keeps every
    assistant ``tool_calls`` message together with its ``tool`` replies.
    """
    turns: list[list[Message]] = []
    for message in messages:
        if message.get("role") == "tool" and turns:
            turns[-1].append(message)
        else:
            turns.append([message])
    return turns


def _drop_leading_orphan_tool_turns(turns: list[list[Message]]) -> list[list[Message]]:
    """Discard leading turns that begin with a ``tool`` message.

    Only possible on malformed input, but a kept window that opens on a ``tool``
    message would be an orphaned result once the preceding turns are evicted --
    a hard API error. Cheap to guard against unconditionally.
    """
    while turns and turns[0] and turns[0][0].get("role") == "tool":
        turns = turns[1:]
    return turns


def _summarise_evicted(
    evicted: list[Message],
    summarize: Callable[[list[Message]], str],
) -> str:
    """Run the injected summariser, falling back to a static note on failure."""
    if not evicted:
        return _SUMMARY_FALLBACK
    try:
        text = summarize(evicted)
    except Exception:  # noqa: BLE001 - a summariser outage must not fail the scan
        logger.warning("summariser raised; degrading to placeholder", exc_info=True)
        return _SUMMARY_FALLBACK
    text = (text or "").strip()
    return text or _SUMMARY_FALLBACK


def _elide_tool_bodies(messages: list[Message], *, limit: int, chars_per_token: float) -> list[Message]:
    """Tier-1 last resort: shrink oldest ``tool`` result bodies to a stub.

    Used only when the kept turns alone still exceed the hard ``limit`` (e.g. a
    single huge tool result). Preserves message identity and pairing -- only the
    ``content`` string is replaced -- so the request stays valid.
    """
    for message in messages:
        if estimate_tokens(messages, chars_per_token=chars_per_token) <= limit:
            break
        if message.get("role") != "tool":
            continue
        body = message.get("content")
        if isinstance(body, str) and len(body) > 200:
            message["content"] = f"[elided {len(body)} chars of an earlier tool result to fit context]"
    return messages


def compact_messages(
    messages: list[Message],
    *,
    summarize: Callable[[list[Message]], str],
    limit: int,
    trigger: int,
    target: int,
    keep_recent_turns: int = 3,
    chars_per_token: float = DEFAULT_CHARS_PER_TOKEN,
) -> tuple[list[Message], dict[str, Any] | None]:
    """Compact ``messages`` if it is over ``trigger``; otherwise return it as-is.

    Returns ``(messages, stats)`` where ``stats`` is ``None`` when no compaction
    was needed, else a small dict describing what happened (for logging).

    ``trigger`` is when to act, ``target`` is what to shrink down to (the gap
    between them is the runway before the next compaction), and ``limit`` is the
    hard model window that the Tier-1 elision guarantees we stay under.
    """
    before = estimate_tokens(messages, chars_per_token=chars_per_token)
    if before <= trigger:
        return messages, None

    prefix, rest = _split_prefix(messages)
    turns = _group_turns(rest)

    kept = list(turns)
    evicted: list[list[Message]] = []
    summary_reserve = [{"role": "assistant", "content": _SUMMARY_LABEL + "x" * 4000}]
    while len(kept) > keep_recent_turns:
        candidate = prefix + summary_reserve + [m for turn in kept for m in turn]
        if estimate_tokens(candidate, chars_per_token=chars_per_token) <= target:
            break
        evicted.append(kept.pop(0))

    kept = _drop_leading_orphan_tool_turns(kept)
    evicted_messages = [m for turn in evicted for m in turn]
    summary_text = _summarise_evicted(evicted_messages, summarize)
    summary_message: Message = {"role": "assistant", "content": _SUMMARY_LABEL + summary_text}

    new_messages = prefix + [summary_message] + [m for turn in kept for m in turn]
    new_messages = _elide_tool_bodies(new_messages, limit=limit, chars_per_token=chars_per_token)

    after = estimate_tokens(new_messages, chars_per_token=chars_per_token)
    stats = {
        "evicted_turns": len(evicted),
        "evicted_messages": len(evicted_messages),
        "kept_turns": len(kept),
        "before_tokens": before,
        "after_tokens": after,
        "summarised": bool(evicted_messages) and summary_text != _SUMMARY_FALLBACK,
    }
    return new_messages, stats
