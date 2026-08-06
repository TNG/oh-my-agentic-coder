#!/usr/bin/env python3
"""Bash-syntax-check every `run:` block in .github/workflows/.

A workflow's `run:` body is shell, but nothing in the normal edit loop parses it
— a quoting mistake ships and only surfaces when that job next runs, which for
the weekly LLM workflows can be days later. The specific trap this was written
for: an escaped backtick inside a double-quoted string. `\\\\`` leaves a literal
backslash and an UNESCAPED backtick, silently turning step-summary markdown into
command substitution:

    echo "Model: \\\\`$m\\\\`"   ->   bash: $m\\: command not found

`${{ ... }}` expressions are replaced with a placeholder token before parsing:
they are substituted by the Actions runner before the shell ever sees them, so
leaving them in would produce false positives on every step.

This checks syntax only — it is not a linter. actionlint (with shellcheck)
covers style and semantics; this covers "does bash accept it at all", which is
the part that turns into a broken job.

Exit 0 when clean, 1 with a per-step report otherwise.
"""
import glob
import re
import subprocess
import sys

import yaml

EXPR = re.compile(r"\$\{\{[^}]*\}\}")


def check_file(path):
    """Yield a diagnostic string for each run block bash rejects."""
    try:
        doc = yaml.safe_load(open(path)) or {}
    except yaml.YAMLError as exc:
        yield f"{path}: not parseable as YAML: {exc}"
        return
    for job_name, job in (doc.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        for index, step in enumerate(job.get("steps") or []):
            if not isinstance(step, dict):
                continue
            run = step.get("run")
            if not run:
                continue
            shell = step.get("shell", "bash")
            if shell not in ("bash", "sh"):
                continue  # pwsh/python steps are not ours to parse
            script = EXPR.sub("EXPR", run)
            proc = subprocess.run(
                ["bash", "-n"], input=script, text=True, capture_output=True
            )
            if proc.returncode != 0:
                name = step.get("name", "<unnamed>")
                detail = (proc.stderr or "").strip().splitlines()
                first = detail[0] if detail else "bash -n failed"
                yield f"{path}: job '{job_name}' step {index} ('{name}'): {first}"


def main():
    paths = sys.argv[1:] or sorted(glob.glob(".github/workflows/*.yml"))
    if not paths:
        print("no workflow files found", file=sys.stderr)
        return 1
    problems = [p for path in paths for p in check_file(path)]
    for problem in problems:
        print(problem, file=sys.stderr)
    if problems:
        print(
            f"\n{len(problems)} run block(s) are not valid bash", file=sys.stderr
        )
        return 1
    print(f"{len(paths)} workflow file(s): all run blocks are valid bash")
    return 0


if __name__ == "__main__":
    sys.exit(main())
