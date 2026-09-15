{
  description = "omac, a sandboxed agent-coding facade";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils, ... }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      # Updated together by scripts/update-nix-release.py before the nix-* tag.
      release = {
        version = "0.9.0";
        rev = "a44213177e298ecdf2a5161004185bcc98000579";
        hash = "sha256-7p1X+n2zwJl2dTRDMLbNNh2hnAmDWv8d8fT2cd12ibg=";
        vendorHash = "sha256-gWqBhFQFANzF2tgpMMCIeaQEm7eSnUQxPtwcFQceh/s=";
      };
    in (flake-utils.lib.eachSystem systems (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        omac = pkgs.buildGoModule {
          pname = "omac";
          inherit (release) version vendorHash;
          src = pkgs.fetchFromGitHub {
            owner = "TNG";
            repo = "oh-my-agentic-coder";
            inherit (release) rev hash;
          };

          go = pkgs.go_1_25;
          subPackages = [ "cmd/omac" ];
          ldflags = [ "-s" "-w" "-X main.Version=${release.version}" ];

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
        } (''
          omac version > "$out"
          grep -Fx "omac ${release.version}" "$out"
        '' + pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
          # macOS system utilities are not available in Nix's build sandbox.
          omac doctor >> "$out"
          ! grep -F "bwrap is not installed" "$out"
          grep -F "[ok] network prompt: dialog backend available" "$out"
        '');
      })) // {
        nixosModules.default = { config, lib, pkgs, ... }:
          let
            cfg = config.omac;
            mkAgent = description: package: {
              enable = lib.mkEnableOption description;
              package = lib.mkOption {
                type = lib.types.nullOr lib.types.package;
                default = package;
                description = "Package that provides ${description}.";
              };
            };
            enabledAgents = lib.filter (agent: agent.enable) (lib.attrValues cfg.agents);
            agentPackages = map (agent: agent.package) enabledAgents;
            omacWithAgents = pkgs.runCommand "omac-with-agents" {
              nativeBuildInputs = [ pkgs.makeWrapper ];
            } ''
              mkdir -p "$out/bin"
              makeWrapper ${cfg.package}/bin/omac "$out/bin/omac" \
                --prefix PATH : ${lib.makeBinPath agentPackages}
            '';
          in {
            options.omac = {
              enable = lib.mkEnableOption "omac";
              package = lib.mkOption {
                type = lib.types.package;
                default = self.packages.${pkgs.stdenv.hostPlatform.system}.omac;
                description = "The omac package to wrap.";
              };
              agents = {
                opencode = mkAgent "OpenCode" pkgs.opencode;
                codex = mkAgent "Codex" pkgs.codex;
                copilot = mkAgent "Copilot" pkgs.github-copilot-cli;
                pi = mkAgent "Pi" null;
                claude-code = mkAgent "Claude Code" null;
                codewhale = mkAgent "CodeWhale" null;
              };
            };

            config = lib.mkIf cfg.enable {
              assertions = lib.mapAttrsToList (name: agent: {
                assertion = !agent.enable || agent.package != null;
                message = "omac.agents.${name}.package must be set when enabled.";
              }) cfg.agents;
              environment.systemPackages = [ omacWithAgents ];
            };
          };
      };
}
