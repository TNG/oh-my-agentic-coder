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
        version = "0.11.0";
        rev = "6f9500a6af758d5a7077e49ce774e062e0fc6907";
        hash = "sha256-3upMMqO4mk3ZlkWXWbfAcGzdNg/CfDiu/j/Dy0ggfE8=";
        vendorHash = "sha256-ZLXFiuYcTI3D2xMlZT3pBWaN9TVWY92aR5MyCIXVHdM=";
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

          # Backport NixOS runtime paths until upstream grants /nix/store and
          # /run/current-system in its baseline. Once the upstream fix lands,
          # --replace-fail breaks the build loudly; delete this patch then.
          postPatch = ''
            substituteInPlace internal/sandboxprofile/baseline.go \
              --replace-fail '"/bin", "/sbin", "/usr",' \
                '"/bin", "/sbin", "/usr", "/nix/store", "/run/current-system",'
          '';

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
          nativeBuildInputs = [ omac ] ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [ pkgs.jq ];
        } (''
          omac version > "$out"
          grep -Fx "omac ${release.version}" "$out"
        '' + pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
          # macOS system utilities are not available in Nix's build sandbox.
          omac doctor >> "$out"
          ! grep -F "bwrap is not installed" "$out"
          grep -F "[ok] network prompt: dialog backend available" "$out"
          omac provenance --json | jq -e '
            .filesystem.entries
            | map(select(.entry == "/nix/store" or .entry == "/run/current-system"))
            | sort_by(.entry) == [
                {entry: "/nix/store", action: "read", source: "builtin"},
                {entry: "/run/current-system", action: "read", source: "builtin"}
              ]
          '
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
