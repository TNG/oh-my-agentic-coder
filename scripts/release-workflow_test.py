#!/usr/bin/env python3
"""Guard the release workflow's Slack notification policy."""

import os
import re
import subprocess
import sys
import tempfile
import textwrap
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent


def extract_job(workflow: str, name: str) -> str:
    job = re.search(
        rf"^  {re.escape(name)}:\n(?P<body>.*?)(?=^  \S|\Z)",
        workflow,
        re.M | re.S,
    )
    if not job:
        raise SystemExit(f"release workflow has no {name} job")
    return job.group("body")


def extract_step_script(job: str, step_id: str) -> str:
    lines = job.splitlines()
    try:
        id_index = next(
            index
            for index, line in enumerate(lines)
            if line.strip() == f"id: {step_id}"
        )
    except StopIteration:
        raise SystemExit(f"release workflow has no {step_id} step") from None

    id_indent = len(lines[id_index]) - len(lines[id_index].lstrip())
    step_start = next(
        (
            index
            for index in range(id_index, -1, -1)
            if len(lines[index]) - len(lines[index].lstrip()) < id_indent
            and lines[index].lstrip().startswith("- ")
        ),
        None,
    )
    if step_start is None:
        raise SystemExit(f"release workflow cannot locate the {step_id} step")

    step_indent = len(lines[step_start]) - len(lines[step_start].lstrip())
    step_end = next(
        (
            index
            for index in range(id_index + 1, len(lines))
            if len(lines[index]) - len(lines[index].lstrip()) == step_indent
            and lines[index].lstrip().startswith("- ")
        ),
        len(lines),
    )
    step = lines[step_start:step_end]
    try:
        run_index = next(
            index
            for index, line in enumerate(step)
            if re.fullmatch(r"\s*run:\s*\|[+-]?\s*", line)
        )
    except StopIteration:
        raise SystemExit(
            f"release workflow {step_id} step has no run block"
        ) from None

    run_indent = len(step[run_index]) - len(step[run_index].lstrip())
    script_lines = []
    for line in step[run_index + 1 :]:
        indent = len(line) - len(line.lstrip())
        if line.strip() and indent <= run_indent:
            break
        script_lines.append(line)
    script = textwrap.dedent("\n".join(script_lines))
    if not script.strip():
        raise SystemExit(f"release workflow {step_id} run block is empty")
    return script


def main() -> int:
    workflow = (REPO / ".github/workflows/release.yml").read_text()
    goreleaser = extract_job(workflow, "goreleaser")

    outputs = re.search(
        r"^    outputs:\s*\n(?P<body>(?:      .*\n?)+)", goreleaser, re.M
    )
    output_mapping = (
        re.search(
            r"^      prerelease:\s*"
            r"\$\{\{\s*steps\.classify_release\.outputs\.prerelease\s*\}\}\s*$",
            outputs.group("body"),
            re.M,
        )
        if outputs
        else None
    )
    if not output_mapping:
        raise SystemExit(
            "goreleaser must expose steps.classify_release.outputs.prerelease "
            "as the prerelease job output"
        )

    classifier_script = extract_step_script(goreleaser, "classify_release")
    with tempfile.TemporaryDirectory() as temp_dir:
        output = Path(temp_dir) / "github-output"
        for version, expected_prerelease in {
            "v1.2.3": "false",
            "v1.2.3-rc1": "true",
            "v1.2.3+build-1": "false",
            "v1.2.3-rc1+build-2": "true",
        }.items():
            output.unlink(missing_ok=True)
            env = os.environ | {"VERSION": version, "GITHUB_OUTPUT": str(output)}
            subprocess.run(
                ["bash", "-e"],
                input=classifier_script,
                text=True,
                env=env,
                check=True,
            )
            values = dict(
                line.split("=", 1) for line in output.read_text().splitlines()
            )
            if values.get("prerelease") != expected_prerelease:
                raise SystemExit(
                    f"release classifier returned prerelease={values.get('prerelease')} "
                    f"for {version}, want {expected_prerelease}"
                )

    notify = extract_job(workflow, "notify")
    condition_block = re.search(
        r"^    if:\s*[>|][+-]?\s*\n(?P<condition>(?:      .*\n?)+)",
        notify,
        re.M,
    )
    if not condition_block:
        raise SystemExit("release notify job has no multiline condition")
    condition = " ".join(condition_block.group("condition").split())
    required_branches = (
        "(github.event_name == 'push' && "
        "needs.goreleaser.outputs.prerelease != 'true')",
        "(github.event_name == 'workflow_dispatch' && !inputs.dry_run)",
    )
    missing = [branch for branch in required_branches if branch not in condition]
    if missing:
        raise SystemExit(
            "release notify condition is missing required policy branch(es): "
            + ", ".join(missing)
        )

    print("release notify condition skips prerelease tag pushes")
    return 0


if __name__ == "__main__":
    sys.exit(main())
