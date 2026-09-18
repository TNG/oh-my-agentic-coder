#!/usr/bin/env python3
"""Test release metadata updates without network access or Git mutations."""

import importlib.util
import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

SCRIPT = Path(__file__).with_name("update-nix-release.py")
REV = "a" * 40
SOURCE_HASH = "sha256-" + "A" * 43 + "="
VENDOR_HASH = "sha256-" + "B" * 43 + "="
ORIGINAL = '''{
      release = {
        version = "0.9.0";
        rev = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
        hash = "old-source";
        vendorHash = "old-vendor";
      };
  unrelated = "preserve me";
}
'''


class UpdateReleaseTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not SCRIPT.exists():
            raise AssertionError("release metadata updater is missing")
        spec = importlib.util.spec_from_file_location("update_nix_release", SCRIPT)
        cls.module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.module)

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.flake = Path(self.temp.name) / "flake.nix"
        self.flake.write_text(ORIGINAL)

    def responses(self):
        return [
            subprocess.CompletedProcess([], 0, json.dumps({"hash": SOURCE_HASH}), ""),
            subprocess.CompletedProcess([], 0, "x86_64-linux", ""),
            subprocess.CompletedProcess([], 1, "", (
                "error: hash mismatch in fixed-output derivation '/nix/store/example-go-modules.drv':\n"
                f"         specified: {SOURCE_HASH}\n            got:    {VENDOR_HASH}\n"
            )),
            subprocess.CompletedProcess([], 0, "", ""),
        ]

    def test_updates_only_release_block_and_verifies_vendor_hash(self):
        with patch.object(self.module.subprocess, "run", side_effect=self.responses()) as run:
            self.module.update_release("v0.10.0", REV, self.flake)
        text = self.flake.read_text()
        for field, value in {
            "version": "0.10.0", "rev": REV, "hash": SOURCE_HASH, "vendorHash": VENDOR_HASH,
        }.items():
            self.assertIn(f'{field} = "{value}";', text)
        self.assertEqual(text.split("      };", 1)[1], ORIGINAL.split("      };", 1)[1])
        calls = [call.args[0] for call in run.call_args_list]
        self.assertIn(REV, calls[0][-1])
        self.assertEqual(calls[2], calls[3])
        self.assertIn("--no-link", calls[2])

    def test_accepts_prerelease_and_build_metadata(self):
        with patch.object(self.module.subprocess, "run", side_effect=self.responses()):
            self.module.update_release("v1.2.3-rc.1+build.2", REV, self.flake)
        self.assertIn('version = "1.2.3-rc.1+build.2";', self.flake.read_text())

    def test_invalid_identifiers_do_not_run_nix_or_change_flake(self):
        for tag in ["1.2.3", "v01.2.3", "v1.2", "v1.2.3-01", "v1.2.3;exit", "v1.2.3\n"]:
            with self.subTest(tag=tag), patch.object(self.module.subprocess, "run") as run:
                with self.assertRaises(ValueError):
                    self.module.update_release(tag, REV, self.flake)
                run.assert_not_called()
                self.assertEqual(self.flake.read_text(), ORIGINAL)
        with patch.object(self.module.subprocess, "run") as run:
            with self.assertRaises(ValueError):
                self.module.update_release("v1.2.3", "main", self.flake)
            run.assert_not_called()

    def test_missing_or_duplicate_metadata_is_rejected(self):
        for content in ["{}", ORIGINAL + ORIGINAL, ORIGINAL.replace("vendorHash", "otherHash")]:
            self.flake.write_text(content)
            with patch.object(self.module.subprocess, "run") as run:
                with self.assertRaises(ValueError):
                    self.module.update_release("v1.2.3", REV, self.flake)
                run.assert_not_called()
                self.assertEqual(self.flake.read_text(), content)

    def test_failures_restore_original(self):
        variants = []
        for stage in range(4):
            responses = self.responses()
            responses[stage] = subprocess.CompletedProcess([], 1, "", "network unavailable")
            variants.append(responses)
        responses = self.responses()
        responses[2] = subprocess.CompletedProcess([], 0, "", "")
        variants.append(responses)
        responses = self.responses()
        responses[0] = subprocess.CompletedProcess([], 0, '{"hash":"not-a-hash"}', "")
        variants.append(responses)
        responses = self.responses()
        responses[2] = subprocess.CompletedProcess([], 1, "", f"unrelated failure\ngot: {VENDOR_HASH}")
        variants.append(responses)
        for responses in variants:
            with self.subTest(responses=responses), patch.object(
                self.module.subprocess, "run", side_effect=responses
            ):
                with self.assertRaises((ValueError, RuntimeError)):
                    self.module.update_release("v1.2.3", REV, self.flake)
            self.assertEqual(self.flake.read_text(), ORIGINAL)


if __name__ == "__main__":
    unittest.main()
