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

agent_wrapper="$(nix eval --raw --impure --expr '
let
  flake = builtins.getFlake (toString ./.);
  configuration = flake.inputs.nixpkgs.lib.nixosSystem {
  system = "x86_64-linux";
  modules = [
    flake.nixosModules.default
    ({ pkgs, ... }: {
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
