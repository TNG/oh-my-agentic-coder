#!/usr/bin/env python3
"""Guard the release workflow's Slack notification policy."""

import os
from pathlib import Path
import re
import subprocess
import tempfile


workflow = Path(".github/workflows/release.yml").read_text()
classifier = re.search(
    r"^      - name: Classify release tag\n"
    r"        id: classify_release\n"
    r"        env:\n"
    r"          VERSION: .*\n"
    r"        run: \|\n"
    r"(?P<script>(?:          .*\n)+)",
    workflow,
    re.M,
)
if not classifier:
    raise SystemExit("release workflow has no release-tag classifier")
classifier_script = "\n".join(
    line[10:] for line in classifier.group("script").splitlines()
)
for version, expected_prerelease in {
    "v1.2.3": "false",
    "v1.2.3-rc1": "true",
    "v1.2.3+build-1": "false",
    "v1.2.3-rc1+build-2": "true",
}.items():
    with tempfile.TemporaryDirectory() as temp_dir:
        output = Path(temp_dir) / "github-output"
        env = os.environ | {"VERSION": version, "GITHUB_OUTPUT": str(output)}
        subprocess.run(["bash", "-e"], input=classifier_script, text=True, env=env, check=True)
        values = dict(line.split("=", 1) for line in output.read_text().splitlines())
    if values.get("prerelease") != expected_prerelease:
        raise SystemExit(
            f"release classifier returned prerelease={values.get('prerelease')} "
            f"for {version}, want {expected_prerelease}"
        )

notify = re.search(r"^  notify:\n(?P<body>.*?)(?=^  \S|\Z)", workflow, re.M | re.S)
if not notify:
    raise SystemExit("release workflow has no notify job")
condition_block = re.search(
    r"^    if: >-\n(?P<condition>(?:      .*\n)+)", notify.group("body"), re.M
)
if not condition_block:
    raise SystemExit("release notify job has no multiline condition")
condition = " ".join(condition_block.group("condition").split())
expected = " ".join(
    """
    (github.event_name == 'push' && needs.goreleaser.outputs.prerelease != 'true') ||
    (github.event_name == 'workflow_dispatch' && !inputs.dry_run)
    """.split()
)

if condition != expected:
    raise SystemExit(
        "release notify condition must skip prerelease tag pushes; "
        f"got: {condition}"
    )

print("release notify condition skips prerelease tag pushes")
