#!/usr/bin/env python3
"""Guard the privileged OCR workflow's trust boundary and configuration."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml

REPO = Path(__file__).resolve().parent.parent
WORKFLOW = REPO / ".github/workflows/ocr-review.yml"


class OCRWorkflowTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not WORKFLOW.exists():
            raise AssertionError("Missing advisory OCR review workflow")
        cls.workflow = yaml.load(WORKFLOW.read_text(), Loader=yaml.BaseLoader)
        cls.job = cls.workflow["jobs"]["review"]
        cls.steps = cls.job["steps"]

    def test_only_same_repository_nondraft_prs_targeting_main(self):
        self.assertEqual(set(self.workflow["on"]), {"pull_request_target"})
        trigger = self.workflow["on"]["pull_request_target"]
        self.assertEqual(trigger["branches"], ["main"])
        self.assertEqual(
            set(trigger["types"]),
            {"opened", "synchronize", "reopened", "ready_for_review"},
        )
        self.assertEqual(
            " ".join(self.job["if"].split()),
            "github.event.pull_request.head.repo.full_name == github.repository "
            "&& !github.event.pull_request.draft",
        )

    def test_review_credentials_cannot_write_repository_contents(self):
        self.assertEqual(self.workflow["permissions"], {"contents": "read"})
        self.assertEqual(
            self.job["permissions"],
            {"contents": "read", "pull-requests": "write", "issues": "write"},
        )
        checkout = self.steps[0]
        self.assertTrue(checkout["uses"].startswith("actions/checkout@"))
        self.assertEqual(checkout["with"]["persist-credentials"], "false")
        self.assertNotIn("ref", checkout["with"])
        # Local scripts run only from the trusted base checkout.
        self.assertEqual(
            [s.get("id") for s in self.steps if "run" in s],
            ["configure", "model", "publication"],
        )
        model = next(s for s in self.steps if s.get("id") == "model")
        self.assertEqual(model["run"].strip(), "scripts/probe-model.sh opencode --github-output")

    def test_pinned_action_receives_incremental_english_review_settings(self):
        action = next(s for s in self.steps if s.get("id") == "ocr")
        self.assertEqual(
            action["uses"],
            "alibaba/open-code-review@7e5ce06c88de5ce81df5dc4f24e2e485b517fae8",
        )
        inputs = action["with"]
        for name, value in {
            "ocr_version": "1.13.10",
            "language": "English",
            "effort": "high",
            "llm_use_anthropic": "false",
            "llm_extra_body": "{}",
            "sticky_summary": "true",
            "incremental": "true",
            "checkpoint_range": "true",
            "llm_url": "${{ steps.configure.outputs.url }}",
            "llm_model": "${{ steps.model.outputs.model }}",
            "llm_auth_token": "${{ secrets.TRUSTEDTOKENS_TOKEN }}",
            "github_token": "${{ github.token }}",
            "full_review": "${{ contains(github.event.pull_request.labels.*.name, 'ocr-full-review') }}",
        }.items():
            with self.subTest(input=name):
                self.assertEqual(inputs[name], value)
        self.assertNotIn("OCR_FAIL_ON_SEVERITY", self.job.get("env", {}))

    def test_runs_are_bounded_and_do_not_hide_errors(self):
        self.assertEqual(self.job["timeout-minutes"], "30")
        self.assertEqual(
            self.workflow["concurrency"],
            {
                "group": "ocr-review-${{ github.event.pull_request.number }}",
                "cancel-in-progress": "true",
            },
        )
        self.assertNotIn("continue-on-error", self.job)
        self.assertTrue(all("continue-on-error" not in s for s in self.steps))

    def run_configure(self, base, token):
        configure = next(s for s in self.steps if s.get("id") == "configure")
        self.assertEqual(
            configure["env"],
            {
                "SKAINET_INTERNAL": "https://api.trustedtokens.eu/v1",
                "SKAINET_TOKEN": "${{ secrets.TRUSTEDTOKENS_TOKEN }}",
            },
        )
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "output"
            result = subprocess.run(
                ["bash", "-euo", "pipefail", "-c", configure["run"]],
                env=os.environ
                | {
                    "SKAINET_INTERNAL": base,
                    "SKAINET_TOKEN": token,
                    "GITHUB_OUTPUT": str(output),
                },
                capture_output=True,
                text=True,
            )
            return result, output.read_text() if output.exists() else ""

    def test_gateway_endpoint_preserves_path_without_double_slash(self):
        for base in ("https://gateway.example/v1", "https://gateway.example/v1/"):
            with self.subTest(base=base):
                result, output = self.run_configure(base, "test-private-token")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(output, "url=https://gateway.example/v1/chat/completions\n")
                self.assertNotIn("test-private-token", result.stdout + result.stderr + output)

    def test_missing_credentials_fail_before_review_without_leaking_values(self):
        for base, token in (
            ("", "test-private-token"),
            ("https://gateway.example/v1", ""),
        ):
            with self.subTest(base=base, token=bool(token)):
                result, output = self.run_configure(base, token)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(output, "")
                self.assertNotIn("test-private-token", result.stdout + result.stderr)
                self.assertNotIn("https://gateway.example", result.stdout + result.stderr)

    def test_publication_errors_fail_but_checkpoint_noop_does_not(self):
        publication = next(
            (s for s in self.steps if s.get("id") == "publication"), None
        )
        self.assertIsNotNone(publication, "Missing publication failure check")
        self.assertEqual(
            publication["env"],
            {
                "COMMENTS_FAILED": "${{ steps.ocr.outputs.comments_failed }}",
                "SUMMARY_URL": "${{ steps.ocr.outputs.summary_comment_url }}",
                "RANGE_REASON": "${{ steps.ocr.outputs.range_reason }}",
            },
        )
        for failed, summary, reason, expected in (
            ("0", "https://github.example/summary", "ok", 0),
            ("1", "https://github.example/summary", "ok", 1),
            ("0", "", "ok", 1),
            ("", "", "", 1),
            ("0", "", "same_head_noop", 0),
            ("1", "", "same_head_noop", 1),
        ):
            with self.subTest(failed=failed, summary=bool(summary), reason=reason):
                result = subprocess.run(
                    ["bash", "-euo", "pipefail", "-c", publication["run"]],
                    env=os.environ
                    | {
                        "COMMENTS_FAILED": failed,
                        "SUMMARY_URL": summary,
                        "RANGE_REASON": reason,
                    },
                    capture_output=True,
                    text=True,
                )
                self.assertEqual(result.returncode, expected, result.stderr)


if __name__ == "__main__":
    unittest.main()
