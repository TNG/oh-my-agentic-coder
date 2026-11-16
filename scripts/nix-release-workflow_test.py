#!/usr/bin/env python3
"""Release policy and real publication helper tests, using only local Git remotes."""

import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
HELPER = ROOT / "scripts/publish-nix-release.sh"
REPOSITORY = "TNG/oh-my-agentic-coder"


class PublicationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="nix-release-", dir="/tmp")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "work"
        self.remote = self.root / "remote.git"
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.env = os.environ | {
            "HOME": str(self.root), "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_CONFIG_GLOBAL": os.devnull, "GIT_TERMINAL_PROMPT": "0",
            "GIT_AUTHOR_NAME": "Test", "GIT_AUTHOR_EMAIL": "test@example.invalid",
            "GIT_COMMITTER_NAME": "Test", "GIT_COMMITTER_EMAIL": "test@example.invalid",
            "GITHUB_REPOSITORY": REPOSITORY, "GH_REPO": REPOSITORY,
            "GH_TOKEN": "fake", "GH_STATE": str(self.root / "prs.json"),
            "GH_LOG": str(self.root / "gh.log"),
            "GITHUB_OUTPUT": str(self.root / "output"),
            "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
        }
        self.run_cmd("git", "init", "--bare", str(self.remote), cwd=self.root)
        self.run_cmd("git", "init", "-b", "main", str(self.repo), cwd=self.root)
        (self.repo / ".github").mkdir()
        shutil.copy(ROOT / ".github/pull_request_template.md", self.repo / ".github")
        self.write_flake("1.0.0", "a" * 40)
        self.git("add", ".")
        self.git("commit", "-m", "base")
        self.base = self.git("rev-parse", "HEAD").stdout.strip()
        self.source = self.base
        self.tag = "v1.2.3"
        self.git("tag", "-a", self.tag, "-m", "release")
        self.git("remote", "add", "origin", str(self.remote))
        self.git("push", "origin", "main", self.tag)
        self.stub("gh", '''#!/usr/bin/env python3
import json, os, pathlib, subprocess, sys
a = sys.argv[1:]
assert a[a.index('--repo') + 1] == 'TNG/oh-my-agentic-coder', a
with open(os.environ['GH_LOG'], 'a') as f: f.write(json.dumps(a) + '\\n')
p = pathlib.Path(os.environ['GH_STATE'])
if a[:2] == ['release', 'view']:
    print(json.dumps({'tagName': os.environ.get('RELEASE_TAG', a[2]),
                      'isDraft': os.environ.get('DRAFT') == '1',
                      'publishedAt': None if os.environ.get('UNPUBLISHED') else '2026-01-01'}))
elif a[:2] == ['pr', 'list']:
    print(p.read_text() if p.exists() else '[]')
elif a[:2] == ['pr', 'create']:
    body = sys.stdin.read() if a[a.index('--body-file')+1] == '-' else pathlib.Path(a[a.index('--body-file')+1]).read_text()
    pathlib.Path(os.environ['GH_STATE'] + '.body').write_text(body)
    if os.environ.get('FAIL_PR'): sys.exit('simulated PR failure')
    branch = a[a.index('--head')+1].removeprefix('TNG:')
    oid = subprocess.check_output(['git', 'ls-remote', 'origin', 'refs/heads/' + branch], text=True).split()[0]
    p.write_text(json.dumps([{'state': 'OPEN', 'headRefOid': oid, 'url': 'https://example.invalid/pr/1'}]))
    print('https://example.invalid/pr/1')
else: sys.exit('unexpected gh command: ' + str(a))
''')

    def stub(self, name, text):
        path = self.bin / name
        path.write_text(text)
        path.chmod(0o755)

    def run_cmd(self, *args, cwd=None, check=True, env=None):
        return subprocess.run(args, cwd=cwd or self.repo, env=env or self.env,
                              text=True, capture_output=True, check=check)

    def git(self, *args, **kwargs):
        return self.run_cmd("git", *args, **kwargs)

    def write_flake(self, version, rev):
        (self.repo / "flake.nix").write_text(
            '{\n  # Packaging metadata\n  release = {\n'
            f'    version = "{version}";\n    rev = "{rev}";\n'
            '    hash = "sha256-source";\n    vendorHash = "sha256-vendor";\n'
            '  };\n  untouched = true;\n}\n')

    def helper(self, mode, *, ok=True, extra_env=None, tag=None):
        args = ["sh", str(HELPER), mode, tag or self.tag]
        if mode != "prepare":
            args += [self.source, self.base]
        result = self.run_cmd(*args, check=False, env=self.env | (extra_env or {}))
        if ok:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertNotIn(result.returncode, (126, 127), result.stderr)
        return result

    def refs(self):
        return self.git("ls-remote", "origin").stdout

    def update(self):
        self.write_flake(self.tag[1:], self.source)

    def publish(self, **kwargs):
        self.helper("prepare")
        self.update()
        return self.helper("publish", **kwargs)

    def fresh_checkout(self):
        self.git("restore", "--staged", "--worktree", "flake.nix")
        self.git("checkout", "main")

    def test_publishes_packaging_commit_tag_and_template_pr_without_moving_main(self):
        self.publish()
        commit = self.git("rev-parse", "nix-1.2.3^{commit}").stdout.strip()
        self.assertNotEqual(commit, self.base)
        self.assertEqual(self.git("rev-parse", commit + "^").stdout.strip(), self.base)
        self.assertEqual(self.git("diff-tree", "--no-commit-id", "--name-only", "-r", commit).stdout.strip(), "flake.nix")
        self.assertIn(self.base + "\trefs/heads/main", self.refs())
        self.assertIn(commit + "\trefs/heads/automation/nix-1.2.3", self.refs())
        self.assertIn(commit + "\trefs/tags/nix-1.2.3", self.refs())
        body = Path(self.env["GH_STATE"] + ".body").read_text()
        for heading in re.findall(r"^## .+$", (ROOT / ".github/pull_request_template.md").read_text(), re.M):
            self.assertIn(heading, body)
        self.assertIn("Refs #285", body)
        self.assertIn("Generated", body)
        self.git("push", "origin", "--delete", "automation/nix-1.2.3")
        self.assertIn(commit + "\trefs/tags/nix-1.2.3", self.refs())

    def test_dry_validation_never_mutates_refs(self):
        before = self.refs()
        self.helper("prepare")
        output = Path(self.env["GITHUB_OUTPUT"]).read_text()
        self.assertIn("source_rev=" + self.source, output)
        self.assertIn("base_rev=" + self.base, output)
        self.update()
        self.helper("check")
        self.assertEqual(before, self.refs())
        self.assertEqual(self.git("rev-parse", "HEAD").stdout.strip(), self.base)
        self.assertNotIn('"create"', Path(self.env["GH_LOG"]).read_text())

    def test_retry_after_pr_failure_reuses_permanent_commit(self):
        self.publish(ok=False, extra_env={"FAIL_PR": "1"})
        before = self.refs()
        self.fresh_checkout()
        self.helper("prepare")
        self.helper("publish")
        self.assertEqual(before, self.refs())
        self.helper("publish")
        log = Path(self.env["GH_LOG"]).read_text()
        self.assertEqual(log.count('["pr", "create"'), 2)

    def test_failed_job_retry_redetects_tag_before_downloading_artifact(self):
        self.publish(ok=False, extra_env={"FAIL_PR": "1"})
        before = self.refs()
        self.fresh_checkout()
        # The prepare job's old recovery=false output is stale. A fresh prepare
        # on the publication runner must skip the modified-flake artifact.
        self.helper("prepare")
        outputs = dict(line.split("=", 1) for line in Path(self.env["GITHUB_OUTPUT"]).read_text().splitlines())
        self.assertEqual(outputs["recovery"], "true")
        if outputs["recovery"] != "true":
            self.update()
        self.helper("check")
        self.helper("publish")
        self.assertEqual(before, self.refs())

    def test_existing_tag_wrong_pins_is_never_overwritten(self):
        self.git("tag", "nix-1.2.3")
        self.git("push", "origin", "nix-1.2.3")
        before = self.refs()
        self.helper("prepare", ok=False)
        self.assertEqual(before, self.refs())

    def test_main_advancing_during_validation_stops(self):
        self.helper("prepare")
        self.git("commit", "--allow-empty", "-m", "advance")
        self.git("push", "origin", "main")
        self.git("checkout", "--detach", self.base)
        self.update()
        before = self.refs()
        self.assertIn("main advanced", self.helper("publish", ok=False).stderr)
        self.assertEqual(before, self.refs())

    def test_changed_head_stops(self):
        self.git("commit", "--allow-empty", "-m", "unexpected")
        self.update()
        self.assertIn("HEAD changed", self.helper("publish", ok=False).stderr)

    def test_only_release_block_may_change(self):
        self.update()
        with (self.repo / "flake.nix").open("a") as f:
            f.write("# unrelated\n")
        self.assertIn("Only the release block", self.helper("publish", ok=False).stderr)

    def test_other_staged_or_untracked_files_stop(self):
        for staged in (False, True):
            with self.subTest(staged=staged):
                self.update()
                (self.repo / "unexpected").write_text("no")
                if staged:
                    self.git("add", "unexpected")
                self.assertIn("Only flake.nix", self.helper("publish", ok=False).stderr)

    def test_newer_main_version_stops_publication(self):
        self.set_main_version("2.0.0")
        self.assertIn("newer version", self.helper("prepare", ok=False).stderr)
        self.update()
        self.helper("publish", ok=False)

    def set_main_version(self, version):
        self.write_flake(version, self.source)
        self.git("add", "flake.nix")
        self.git("commit", "-m", "change packaged version")
        self.base = self.git("rev-parse", "HEAD").stdout.strip()
        self.git("push", "origin", "main")

    def test_stable_release_follows_its_release_candidate(self):
        self.set_main_version("1.2.3-rc.10")
        self.publish()

    def test_prerelease_cannot_replace_stable_release(self):
        self.tag = "v1.2.3-rc.1"
        self.git("tag", self.tag)
        self.git("push", "origin", self.tag)
        self.set_main_version("1.2.3")
        self.assertIn("newer version", self.helper("prepare", ok=False).stderr)

    def test_prerelease_numbers_compare_numerically(self):
        self.tag = "v1.2.3-rc.2"
        self.git("tag", self.tag)
        self.git("push", "origin", self.tag)
        self.set_main_version("1.2.3-rc.10")
        self.assertIn("newer version", self.helper("prepare", ok=False).stderr)

    def test_invalid_or_unpublished_release_stops(self):
        for env in ({"DRAFT": "1"}, {"UNPUBLISHED": "1"}, {"RELEASE_TAG": "v9.9.9"},
                    {"GITHUB_REPOSITORY": "fork/oh-my-agentic-coder"}):
            with self.subTest(env=env):
                self.helper("prepare", ok=False, extra_env=env)
        for tag in ("1.2.3", "v01.2.3", "v1.2.3-01", "v1.2.3^{commit}", "v1.2.3\nother"):
            with self.subTest(tag=tag):
                self.helper("prepare", ok=False, tag=tag)

    def test_prerelease_with_build_metadata(self):
        self.tag = "v1.2.3-rc.1+build.2"
        self.git("tag", self.tag)
        self.git("push", "origin", self.tag)
        self.publish()
        self.assertIn("refs/tags/nix-1.2.3-rc.1+build.2", self.refs())

    def test_removed_branch_fails_with_recovery_message(self):
        self.publish(ok=False, extra_env={"FAIL_PR": "1"})
        self.fresh_checkout()
        self.git("push", "origin", "--delete", "automation/nix-1.2.3")
        before = self.refs()
        self.assertIn("recovery", self.helper("publish", ok=False).stderr.lower())
        self.assertEqual(before, self.refs())

    def test_merged_pr_with_removed_branch_is_already_complete(self):
        self.publish()
        p = Path(self.env["GH_STATE"])
        prs = json.loads(p.read_text())
        prs[0]["state"] = "MERGED"
        p.write_text(json.dumps(prs))
        self.fresh_checkout()
        self.git("push", "origin", "--delete", "automation/nix-1.2.3")
        before = self.refs()
        self.helper("publish")
        self.assertEqual(before, self.refs())

    def test_atomic_push_tag_race_leaves_no_branch(self):
        self.update()
        real_git = shutil.which("git")
        self.stub("git", f'''#!/bin/sh
case " $* " in
  *" push --atomic "*)
    "{real_git}" --git-dir="{self.remote}" update-ref refs/tags/nix-1.2.3 {self.base}
    ;;
esac
exec "{real_git}" "$@"
''')
        self.helper("publish", ok=False)
        self.assertNotIn("refs/heads/automation/nix-1.2.3", self.refs())
        self.assertIn(self.base + "\trefs/tags/nix-1.2.3", self.refs())

    def test_executable_flake_is_not_packaging_only(self):
        self.update()
        (self.repo / "flake.nix").chmod(0o755)
        self.assertIn("non-executable", self.helper("publish", ok=False).stderr)

    def test_source_tag_changed_during_validation_stops(self):
        self.helper("prepare")
        self.git("commit", "--allow-empty", "-m", "different source")
        changed = self.git("rev-parse", "HEAD").stdout.strip()
        self.git("push", "origin", "HEAD:refs/heads/changed-source")
        self.git("update-ref", "refs/tags/" + self.tag, changed, cwd=self.remote)
        self.git("checkout", "--detach", self.base)
        self.update()
        self.assertIn("Published source tag changed", self.helper("publish", ok=False).stderr)

    def test_closed_pr_is_not_reopened(self):
        self.publish()
        p = Path(self.env["GH_STATE"])
        prs = json.loads(p.read_text())
        prs[0]["state"] = "CLOSED"
        p.write_text(json.dumps(prs))
        self.fresh_checkout()
        before = self.refs()
        self.assertIn("closed without merging", self.helper("publish", ok=False).stderr)
        self.assertEqual(before, self.refs())

    def test_retry_from_new_clone_without_local_packaging_branch(self):
        self.publish(ok=False, extra_env={"FAIL_PR": "1"})
        before = self.refs()
        clone = self.root / "retry"
        self.git("clone", "--branch", "main", str(self.remote), str(clone))
        self.repo = clone
        self.helper("prepare")
        self.helper("publish")
        self.assertEqual(before, self.refs())

    def test_base_version_cannot_inject_nix_expression(self):
        self.write_flake("1.${builtins.currentSystem}.0", "a" * 40)
        self.git("add", "flake.nix")
        self.git("commit", "-m", "invalid metadata")
        self.base = self.git("rev-parse", "HEAD").stdout.strip()
        self.git("push", "origin", "main")
        self.assertIn("literal release block", self.helper("prepare", ok=False).stderr)


class WorkflowTests(unittest.TestCase):
    def setUp(self):
        self.text = (ROOT / ".github/workflows/nix-release.yml").read_text()
        # Extract actual job/step blocks by indentation, without a YAML dependency.
        self.jobs = dict(re.findall(
            r"^  ([\w-]+):\n((?:(?!^  \S).|\n)*)", self.text.split("\njobs:\n", 1)[1], re.M))

    def job(self, name):
        self.assertIn(name, self.jobs)
        return self.jobs[name]

    def steps(self, name):
        return re.findall(r"^      - name: ([^\n]+)\n((?:(?!^      - ).|\n)*)",
                          self.job(name), re.M)

    def condition(self, block, indent):
        match = re.search(rf"^ {{{indent}}}if: (.*(?:\n {{{indent + 2}}}[^\n]+)*)", block, re.M)
        self.assertIsNotNone(match, "missing condition")
        return " ".join(match[1].removeprefix(">-").split())

    def test_workflow_contract(self):
        text = self.text
        for required in ("workflow_call:", "workflow_dispatch:", "default: true",
                         "contents: read", "cancel-in-progress: false", "ref: main",
                         "fetch-depth: 0", "persist-credentials: false",
                         "cachix/install-nix-action@v31", "EXPECTED_VERSION=${RELEASE_TAG#v}",
                         "python3 scripts/update-nix-release.py", "sh scripts/flake_test.sh",
                         "github.repository == 'TNG/oh-my-agentic-coder'",
                         "github.ref == 'refs/heads/main'"):
            self.assertIn(required, text)
        self.assertEqual(set(self.jobs), {"prepare", "validate", "publish"})
        self.assertRegex(text, r"(?m)^concurrency:\n  group: nix-release-\$\{\{ inputs.release_tag \}\}\n  cancel-in-progress: false$")
        self.assertNotIn("nix flake update", text)
        self.assertNotRegex(text, r"(?m)^\s+(?:contents|pull-requests): write")
        self.assertNotRegex(text, r"git push|gh pr create")
        release = (ROOT / ".github/workflows/release.yml").read_text()
        job = release[release.index("\n  nix-release:"):]
        for required in ("needs: goreleaser", "startsWith(github.ref, 'refs/tags/v')",
                         "github.event_name == 'push' || !inputs.dry_run",
                         "uses: ./.github/workflows/nix-release.yml",
                         "release_tag: ${{ github.ref_name }}",
                         "NIX_RELEASE_TOKEN: ${{ secrets.NIX_RELEASE_TOKEN }}"):
            self.assertIn(required, job)

    def test_prepare_is_read_only_and_exports_verified_metadata(self):
        prepare = self.job("prepare")
        self.assertIn("ref: main", prepare)
        self.assertIn("fetch-depth: 0", prepare)
        self.assertIn("persist-credentials: false", prepare)
        for output in ("base_rev", "source_rev", "recovery"):
            self.assertIn(f"      {output}: ${{{{ steps.release.outputs.{output} }}}}", prepare)
        steps = self.steps("prepare")
        resolve = next(i for i, (_, step) in enumerate(steps) if ' prepare "$RELEASE_TAG"' in step)
        install = next(i for i, (_, step) in enumerate(steps) if "install-nix-action" in step)
        self.assertLess(resolve, install)
        self.assertIn("id: release", steps[resolve][1])
        self.assertIn("GH_TOKEN: ${{ github.token }}", steps[resolve][1])
        for marker in ("install-nix-action", "scripts/update-nix-release.py", "actions/upload-artifact@v4"):
            step = next(step for _, step in steps if marker in step)
            self.assertEqual(self.condition(step, 8), "steps.release.outputs.recovery != 'true'")
        self.assertNotIn("flake_test.sh", prepare)
        self.assertNotIn("nix build", prepare)
        self.assertNotIn("dry_run", prepare.split("    steps:\n", 1)[1])
        check = next(step for _, step in steps if ' check "$RELEASE_TAG"' in step)
        self.assertIn("GH_TOKEN: ${{ github.token }}", check)
        self.assertNotIn("if:", check)
        self.assertIn("SOURCE_REV: ${{ steps.release.outputs.source_rev }}", check)
        self.assertIn("BASE_REV: ${{ steps.release.outputs.base_rev }}", check)

    def test_validation_builds_all_native_systems_before_publication(self):
        validate = self.job("validate")
        self.assertIn("    needs: prepare\n", validate)
        self.assertEqual(self.condition(validate, 4), "needs.prepare.outputs.recovery != 'true'")
        self.assertIn("    runs-on: ${{ matrix.runner }}\n", validate)
        self.assertIn("      fail-fast: false\n", validate)
        self.assertEqual(re.findall(r"          - runner: (\S+)\n            system: (\S+)", validate), [
            ("ubuntu-latest", "x86_64-linux"),
            ("ubuntu-24.04-arm", "aarch64-linux"),
            ("macos-latest", "aarch64-darwin"),
        ])
        self.assertNotIn("continue-on-error", validate)
        self.assertNotIn("GH_TOKEN", validate)
        self.assertNotIn("dry_run", validate)
        steps = self.steps("validate")
        build = next(step for _, step in steps if "sh scripts/flake_test.sh" in step)
        self.assertIn("EXPECTED_VERSION=${RELEASE_TAG#v} sh scripts/flake_test.sh", build)
        self.assertIn("git diff --check", build)
        self.assertNotIn("if:", build)
        self.assertIn("RELEASE_TAG: ${{ inputs.release_tag }}", validate)
        self.assertLess(validate.index("actions/download-artifact@v4"), validate.index("install-nix-action"))
        self.assertLess(validate.index("install-nix-action"), validate.index("sh scripts/flake_test.sh"))

    def test_same_artifact_and_captured_base_reach_fresh_jobs(self):
        upload = next(step for _, step in self.steps("prepare") if "actions/upload-artifact@v4" in step)
        self.assertIn("          path: flake.nix\n", upload)
        self.assertIn("          if-no-files-found: error\n", upload)
        retention = re.search(r"retention-days: (\d+)", upload)
        self.assertIsNotNone(retention)
        self.assertTrue(1 <= int(retention[1]) <= 7)
        artifact = re.search(r"^          name: (.+)$", upload, re.M)[1]
        for name in ("validate", "publish"):
            steps = self.steps(name)
            checkout = steps[0][1]
            self.assertIn("actions/checkout@v4", checkout)
            self.assertIn("ref: ${{ needs.prepare.outputs.base_rev }}", checkout)
            self.assertIn("persist-credentials: false", checkout)
            download = next(step for _, step in steps if "actions/download-artifact@v4" in step)
            self.assertIn(f"          name: {artifact}\n", download)
            self.assertNotIn("run-id:", download)
            self.assertNotIn("github-token:", download)
            if name == "publish":
                self.assertIn("fetch-depth: 0", checkout)
                self.assertEqual(self.condition(download, 8), "steps.publication.outputs.recovery != 'true'")
                detect_index = next(i for i, (_, step) in enumerate(steps) if "id: publication" in step)
                download_index = next(i for i, (_, step) in enumerate(steps) if "actions/download-artifact@v4" in step)
                self.assertLess(detect_index, download_index)
                self.assertIn('prepare "$RELEASE_TAG"', steps[detect_index][1])
                self.assertIn("GH_TOKEN: ${{ github.token }}", steps[detect_index][1])
            else:
                self.assertNotIn("if:", download)

    def test_publish_is_isolated_and_pat_is_only_in_last_step(self):
        publish = self.job("publish")
        self.assertIn("    runs-on: ubuntu-latest\n", publish)
        self.assertIn("    needs: [prepare, validate]\n", publish)
        for output in ("source_rev", "base_rev"):
            self.assertIn(f"{output.upper()}: ${{{{ needs.prepare.outputs.{output} }}}}", publish)
        self.assertNotIn("install-nix-action", publish)
        self.assertNotIn("flake_test.sh", publish)
        self.assertNotIn("nix build", publish)
        steps = self.steps("publish")
        self.assertIn('check "$RELEASE_TAG" "$SOURCE_REV" "$BASE_REV"', steps[-2][1])
        self.assertIn("GH_TOKEN: ${{ github.token }}", steps[-2][1])
        self.assertIn('publish "$RELEASE_TAG" "$SOURCE_REV" "$BASE_REV"', steps[-1][1])
        self.assertIn("GH_TOKEN: ${{ secrets.NIX_RELEASE_TOKEN }}", steps[-1][1])
        self.assertEqual(self.text.count("${{ secrets.NIX_RELEASE_TOKEN }}"), 1)
        for name in ("prepare", "validate"):
            self.assertNotIn("secrets.", self.job(name))
        self.assertNotIn("secrets.", publish.split("    steps:\n", 1)[0])
        self.assertNotIn("if:", steps[-2][1])
        self.assertNotIn("if:", steps[-1][1])

    def test_manual_reusable_recovery_and_dry_run_gates(self):
        import itertools

        # Evaluate the conditions extracted from the workflow, including the
        # manual reusable call whose event is also workflow_dispatch.
        conditions = {name: self.condition(self.job(name), 4) for name in ("prepare", "publish")}
        self.assertIn("always()", conditions["publish"])
        for prepare, validate, recovery, dry_run, upstream, main in itertools.product(
                ("success", "failure", "cancelled", "skipped"),
                ("success", "failure", "cancelled", "skipped"),
                ("true", "false"), (None, True, False), (True, False), (True, False)):
            with self.subTest(prepare=prepare, validate=validate, recovery=recovery,
                              dry_run=dry_run, upstream=upstream, main=main):
                values = {
                    "always()": True,
                    'contains(toJSON(inputs), \'"dry_run"\')': dry_run is not None,
                    "github.repository": REPOSITORY if upstream else "fork/omac",
                    "github.ref": "refs/heads/main" if main else "refs/tags/v1.2.3",
                    "github.event_name": "workflow_dispatch",
                    "needs.prepare.result": prepare,
                    "needs.validate.result": validate,
                    "needs.prepare.outputs.recovery": recovery,
                    "inputs.dry_run": dry_run,
                }
                allowed = upstream and (dry_run is None or main)
                for name, condition in conditions.items():
                    expression = condition
                    for key, value in values.items():
                        expression = expression.replace(key, repr(value))
                    expression = expression.replace("&&", " and ").replace("||", " or ")
                    expression = re.sub(r"!(?!=)", " not ", expression)
                    actual = eval(expression.strip(), {"__builtins__": {}})
                    expected = allowed if name == "prepare" else (
                        allowed and prepare == "success" and dry_run is not True
                        and (recovery == "true" or validate == "success"))
                    self.assertEqual(bool(actual), expected, name)


if __name__ == "__main__":
    unittest.main()
