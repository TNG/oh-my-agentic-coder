#!/usr/bin/env sh
set -eu

nix flake check --no-build
system="$(nix eval --raw --impure --expr builtins.currentSystem)"
version="$(nix eval --raw ".#packages.${system}.omac.version")"
case "$version" in
  0.9.0-unstable-*) ;;
  *)
    printf 'unexpected package version: %s\n' "$version" >&2
    exit 1
    ;;
esac
nix build ".#checks.${system}.omac"
actual_version="$(nix run . -- version)"
if [ "$actual_version" != "omac $version" ]; then
  printf 'binary version: %s; expected: omac %s\n' "$actual_version" "$version" >&2
  exit 1
fi
