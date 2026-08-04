#!/usr/bin/env python3
"""Guardrails for the prose docs.

Two checks, both cheap enough for the Lint job:

1. README.md stays an entry point, not a manual. It is capped at
   MAX_README_LINES; details belong in docs/ (see issue #190). The
   README-onboarding e2e hands an agent nothing but README.md, so the cap is
   deliberately generous — install, prerequisites and quickstart must fit.
2. Every relative Markdown link in the tracked prose docs resolves, including
   its `#anchor`. Splitting the README across docs/ only stays useful while the
   cross-links are live.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

MAX_README_LINES = 400

REPO = Path(__file__).resolve().parent.parent
DOCS = [REPO / "README.md", REPO / "CREATING_A_SKILL.md", REPO / "COLLABORATION.md"]
DOCS += sorted((REPO / "docs").glob("*.md"))

LINK = re.compile(r"\[(?:[^\]]*)\]\(([^)\s]+)\)")
HEADING = re.compile(r"^(#{1,6})\s+(.*)$")


def slugs(text: str) -> set[str]:
    """GitHub-flavored anchor slugs for every ATX heading in text."""
    out = set()
    fenced = False
    for line in text.splitlines():
        if line.startswith("```"):
            fenced = not fenced
            continue
        if fenced:
            continue
        m = HEADING.match(line)
        if not m:
            continue
        title = m.group(2).strip().rstrip("#").strip()
        title = title.replace("`", "")
        title = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", title)
        slug = re.sub(r"[^\w\- ]", "", title.lower()).replace(" ", "-")
        out.add(slug)
    return out


def main() -> int:
    problems: list[str] = []

    readme = REPO / "README.md"
    lines = len(readme.read_text().splitlines())
    if lines > MAX_README_LINES:
        problems.append(
            f"README.md is {lines} lines (max {MAX_README_LINES}). Move reference "
            f"material into docs/ and link to it — see issue #190."
        )

    cache: dict[Path, str] = {}

    def read(path: Path) -> str:
        if path not in cache:
            cache[path] = path.read_text()
        return cache[path]

    for doc in DOCS:
        if not doc.exists():
            continue
        text = read(doc)
        rel = doc.relative_to(REPO)
        for match in LINK.finditer(text):
            target = match.group(1)
            if target.startswith(("http://", "https://", "mailto:", "<")):
                continue
            path_part, _, anchor = target.partition("#")
            if not path_part:
                if anchor not in slugs(text):
                    problems.append(f"{rel}: no heading for anchor '#{anchor}'")
                continue
            resolved = (doc.parent / path_part).resolve()
            if not resolved.exists():
                problems.append(f"{rel}: link target does not exist: {target}")
            elif anchor and resolved.suffix == ".md" and anchor not in slugs(read(resolved)):
                problems.append(f"{rel}: no heading for anchor in {target}")

    for problem in problems:
        print(f"[fail] {problem}", file=sys.stderr)
    if problems:
        return 1
    print(f"[ok] README.md {lines}/{MAX_README_LINES} lines; all doc links resolve")
    return 0


if __name__ == "__main__":
    sys.exit(main())
