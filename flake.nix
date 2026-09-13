{
  description = "omac, a sandboxed agent-coding facade";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils, ... }:
    flake-utils.lib.eachSystem [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ] (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        version = "0.9.0-unstable-${self.lastModifiedDate or "19700101"}";
        omac = pkgs.buildGoModule {
          pname = "omac";
          inherit version;
          src = ./.;

          vendorHash = "sha256-gWqBhFQFANzF2tgpMMCIeaQEm7eSnUQxPtwcFQceh/s=";

          go = pkgs.go_1_25;
          subPackages = [ "cmd/omac" ];
          ldflags = [ "-s" "-w" "-X main.Version=${version}" ];

          nativeBuildInputs = [ pkgs.makeWrapper ];
          postInstall = pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
            wrapProgram "$out/bin/omac" \
              --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.bubblewrap pkgs.zenity pkgs.libnotify ]}
          '';

          meta = {
            description = "Sandboxed agent-coding facade with keychain-backed secrets";
            homepage = "https://github.com/TNG/oh-my-agentic-coder";
            license = pkgs.lib.licenses.asl20;
            mainProgram = "omac";
            platforms = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
          };
        };
      in {
        packages = {
          inherit omac;
          default = omac;
        };

        checks.omac = pkgs.runCommand "omac-smoke" {
          nativeBuildInputs = [ omac ];
        } ''
          omac version > "$out"
          omac doctor >> "$out"
          ! grep -F "bwrap is not installed" "$out"
          grep -F "[ok] network prompt: dialog backend available" "$out"
        '';
      });
}
