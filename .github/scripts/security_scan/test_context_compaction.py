"""Tests for the in-flight context compaction used by the strix scan proxy.

Runnable either under pytest or standalone (``python3 test_context_compaction.py``)
so it needs no test harness beyond the standard library. The summariser is a
stub -- the live gateway-backed one can only be exercised in CI, where the
skainet M2M secret exists.
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from context_compaction import (  # noqa: E402
    _SUMMARY_FALLBACK,
    compact_messages,
    estimate_tokens,
)

# Small, explicit thresholds keep the synthetic payloads tiny and the intent
# obvious; production defaults live in skainet_proxy.py.
LIMIT = 12_000
TRIGGER = 10_000
TARGET = 6_000
BIG = "A" * 3_500  # ~1000 estimated tokens per tool result


def _stub_summary(_evicted: list[dict]) -> str:
    return "- earlier steps summarised"


def _system() -> dict:
    return {"role": "system", "content": "You are a pentest agent. " * 50}


def _turn(index: int, *, body: str = BIG) -> list[dict]:
    call_id = f"call_{index}"
    return [
        {
            "role": "assistant",
            "content": None,
            "tool_calls": [
                {"id": call_id, "type": "function", "function": {"name": "read", "arguments": "{}"}}
            ],
        },
        {"role": "tool", "tool_call_id": call_id, "content": body},
    ]


def _conversation(n_turns: int) -> list[dict]:
    messages = [_system(), {"role": "user", "content": "Scan the repo."}]
    for i in range(n_turns):
        messages += _turn(i)
    return messages


def _kwargs(**overrides) -> dict:
    base = dict(
        summarize=_stub_summary,
        limit=LIMIT,
        trigger=TRIGGER,
        target=TARGET,
        keep_recent_turns=3,
    )
    base.update(overrides)
    return base


def _assert_pairing(messages: list[dict]) -> None:
    """Every tool message's id must belong to an assistant tool_call seen before it."""
    seen_ids: set[str] = set()
    for message in messages:
        if message.get("role") == "assistant":
            for call in message.get("tool_calls") or []:
                seen_ids.add(call["id"])
        elif message.get("role") == "tool":
            tid = message.get("tool_call_id")
            assert tid in seen_ids, f"orphaned tool message {tid!r} (no preceding tool_call)"


def test_no_compaction_under_trigger() -> None:
    messages = _conversation(3)
    assert estimate_tokens(messages) <= TRIGGER
    result, stats = compact_messages(messages, **_kwargs())
    assert stats is None
    assert result is messages


def test_compaction_reduces_below_limit_and_target() -> None:
    messages = _conversation(30)
    before = estimate_tokens(messages)
    assert before > TRIGGER
    result, stats = compact_messages(messages, **_kwargs())
    assert stats is not None
    after = estimate_tokens(result)
    assert after < before
    assert after <= LIMIT
    assert stats["before_tokens"] == before
    assert stats["evicted_turns"] > 0
    assert stats["summarised"] is True


def test_pairing_preserved_after_compaction() -> None:
    messages = _conversation(30)
    result, _ = compact_messages(messages, **_kwargs())
    _assert_pairing(result)


def test_system_prefix_preserved() -> None:
    messages = _conversation(30)
    result, _ = compact_messages(messages, **_kwargs())
    assert result[0] == messages[0]
    assert result[0]["role"] == "system"


def test_recent_turns_kept_verbatim() -> None:
    messages = _conversation(30)
    result, _ = compact_messages(messages, **_kwargs())
    # The last kept turn (assistant tool_call + tool result) must survive intact.
    assert messages[-2] in result  # assistant tool_call
    assert messages[-1] in result  # its tool result


def test_summary_message_inserted_after_prefix() -> None:
    messages = _conversation(30)
    result, _ = compact_messages(messages, **_kwargs())
    summary = result[1]
    assert summary["role"] == "assistant"
    assert "compacted" in summary["content"].lower()


def test_degrades_to_fallback_when_summariser_raises() -> None:
    def boom(_evicted: list[dict]) -> str:
        raise RuntimeError("gateway down")

    messages = _conversation(30)
    result, stats = compact_messages(messages, **_kwargs(summarize=boom))
    assert stats is not None
    assert stats["summarised"] is False
    assert _SUMMARY_FALLBACK in result[1]["content"]
    _assert_pairing(result)  # eviction still valid even without a summary
    assert estimate_tokens(result) <= LIMIT


def test_tier1_elision_when_recent_turn_exceeds_limit() -> None:
    huge = "B" * (LIMIT * 4 * 4)  # one tool result far larger than the whole window
    messages = [_system(), {"role": "user", "content": "go"}] + _turn(0, body=huge)
    result, stats = compact_messages(messages, **_kwargs())
    assert stats is not None
    assert estimate_tokens(result) <= LIMIT
    tool_bodies = [m["content"] for m in result if m.get("role") == "tool"]
    assert any("elided" in body for body in tool_bodies)


def test_no_orphan_tool_at_kept_boundary() -> None:
    # Force keep_recent_turns=1 so the kept window is a single turn; it must
    # still open on the assistant tool_call, never the bare tool result.
    messages = _conversation(30)
    result, _ = compact_messages(messages, **_kwargs(keep_recent_turns=1))
    _assert_pairing(result)
    first_non_prefix = next(m for m in result if m.get("role") != "system")
    assert first_non_prefix["role"] != "tool"


def _run_standalone() -> int:
    tests = [obj for name, obj in sorted(globals().items()) if name.startswith("test_")]
    failures = 0
    for test in tests:
        try:
            test()
            print(f"PASS {test.__name__}")
        except AssertionError as exc:
            failures += 1
            print(f"FAIL {test.__name__}: {exc}")
        except Exception as exc:  # noqa: BLE001
            failures += 1
            print(f"ERROR {test.__name__}: {exc!r}")
    print(f"\n{len(tests) - failures}/{len(tests)} passed")
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(_run_standalone())
