#!/usr/bin/env sh
set -eu

nix flake check --no-build
system="$(nix eval --raw --impure --expr builtins.currentSystem)"
nix build ".#checks.${system}.omac"
