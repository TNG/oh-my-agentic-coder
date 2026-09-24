#!/usr/bin/env sh
set -eu

nix flake check --all-systems --no-build
system="$(nix eval --raw --impure --expr builtins.currentSystem)"
version="$(nix eval --raw ".#packages.${system}.omac.version")"
source_rev="$(nix eval --raw ".#packages.${system}.omac.src.rev")"
printf '%s\n' "$source_rev" | grep -Eq '^[0-9a-f]{40}$'
if [ "$version" != "${EXPECTED_VERSION:-$version}" ]; then
  printf 'unexpected package version: %s\n' "$version" >&2
  exit 1
fi
nix build --no-link ".#checks.${system}.omac"
actual_version="$(nix run . -- version)"
if [ "$actual_version" != "omac $version" ]; then
  printf 'binary version: %s; expected: omac %s\n' "$actual_version" "$version" >&2
  exit 1
fi

agent_wrapper="$(nix eval --raw --impure --expr '
let
  flake = builtins.getFlake (toString ./.);
  configuration = flake.inputs.nixpkgs.lib.nixosSystem {
  system = "x86_64-linux";
  modules = [
    flake.nixosModules.default
    ({ pkgs, ... }: {
      system.stateVersion = "26.05";
      omac.enable = true;
      omac.agents.pi = { enable = true; package = pkgs.hello; };
    })
  ];
  };
  packages = configuration.config.environment.systemPackages;
in if builtins.any (package: package.name == "omac-with-agents") packages
  && !builtins.elem configuration.pkgs.hello packages then "ok" else "failed"')"
if [ "$agent_wrapper" != "ok" ]; then
  printf 'enabled Pi package must only be available through omac\n' >&2
  exit 1
fi
