#!/usr/bin/env python3
"""Update the pinned release directly in flake.nix; never commit or publish it."""

import argparse
import json
import re
import subprocess
import sys
from pathlib import Path


def update_release(tag: str, rev: str, flake: Path) -> None:
    number = r"(?:0|[1-9][0-9]*)"
    identifier = r"(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)"
    if not re.fullmatch(
        rf"v{number}\.{number}\.{number}(?:-{identifier}(?:\.{identifier})*)?"
        r"(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?", tag
    ):
        raise ValueError("expected a canonical release tag such as v0.10.0 or v0.10.0-rc.1")
    if not re.fullmatch(r"[0-9a-f]{40}", rev):
        raise ValueError("source revision must be a full 40-character Git commit hash")

    flake = flake.resolve()
    original = flake.read_text()
    blocks = list(re.finditer(r"(?m)^      release = \{\n(?:        .*\n)*      \};", original))
    if len(blocks) != 1:
        raise ValueError("flake.nix must contain exactly one release metadata block")
    block = blocks[0]
    fields = re.findall(r'^        (\w+) = "[^"\n]*";$', block.group(), re.M)
    if sorted(fields) != ["hash", "rev", "vendorHash", "version"]:
        raise ValueError("release metadata must contain version, rev, hash, and vendorHash")

    def command(*args):
        result = subprocess.run(args, cwd=flake.parent, text=True, capture_output=True)
        if result.returncode:
            raise RuntimeError(f"{' '.join(args)} failed:\n{result.stderr}")
        return result.stdout.strip()

    def write_metadata(source_hash, vendor_hash):
        metadata = {"version": tag[1:], "rev": rev, "hash": source_hash, "vendorHash": vendor_hash}
        replacement = "      release = {\n" + "".join(
            f'        {key} = "{value}";\n' for key, value in metadata.items()
        ) + "      };"
        flake.write_text(original[:block.start()] + replacement + original[block.end():])

    try:
        source_hash = json.loads(command(
            "nix", "store", "prefetch-file", "--unpack", "--json", "--name", "source",
            f"https://github.com/TNG/oh-my-agentic-coder/archive/{rev}.tar.gz",
        ))["hash"]
        if not re.fullmatch(r"sha256-[A-Za-z0-9+/]{43}=", source_hash):
            raise ValueError("source prefetch returned an invalid SHA-256 hash")
        system = command("nix", "eval", "--raw", "--impure", "--expr", "builtins.currentSystem")
        if system not in ("x86_64-linux", "aarch64-linux", "aarch64-darwin"):
            raise ValueError(f"unsupported build system: {system}")
        write_metadata(source_hash, "sha256-" + "A" * 43 + "=")
        build = ("nix", "build", "--no-link", "--no-write-lock-file", f".#packages.{system}.omac.goModules")
        probe = subprocess.run(build, cwd=flake.parent, text=True, capture_output=True)
        match = re.search(
            r"hash mismatch in fixed-output derivation '[^'\n]*-go-modules\.drv':\s*"
            r"specified: sha256-A{43}=\s*got:\s*(sha256-[A-Za-z0-9+/]{43}=)",
            probe.stderr,
        )
        if probe.returncode == 0 or match is None:
            raise RuntimeError(f"expected a Go vendor hash mismatch, got:\n{probe.stderr}")
        write_metadata(source_hash, match[1])
        command(*build)
    except BaseException:
        flake.write_text(original)
        raise


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag", help="published release tag, including v")
    parser.add_argument("rev", help="exact commit pointed to by that tag")
    parser.add_argument("--flake", type=Path, default=Path("flake.nix"))
    args = parser.parse_args()
    try:
        update_release(args.tag, args.rev, args.flake)
    except (ValueError, RuntimeError, OSError, KeyError) as exc:
        print(f"Nix release update failed: {exc}", file=sys.stderr)
        return 1
    print(f"Updated {args.flake} for {args.tag} at {args.rev}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
