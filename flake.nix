{
  description = "Run Anthropic Messages API and OpenAI Responses API off GitHub Copilot";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";

    treefmt-nix = {
      url = "github:numtide/treefmt-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    git-hooks = {
      url = "github:cachix/git-hooks.nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      treefmt-nix,
      git-hooks,
      ...
    }:
    let
      # Nix dev/build hosts. The roadmap's Windows targets are Go cross-compile
      # outputs (GOOS/GOARCH), not Nix systems, so they don't belong here.
      supportedSystems = [
        "x86_64-linux"
        "aarch64-darwin"
      ];
      forSupportedSystems = nixpkgs.lib.genAttrs supportedSystems;
      pkgsFor = forSupportedSystems (system: import nixpkgs { inherit system; });

      treefmtEval = forSupportedSystems (
        system: treefmt-nix.lib.evalModule pkgsFor.${system} ./treefmt.nix
      );

      gitHooksCheck = forSupportedSystems (
        system:
        git-hooks.lib.${system}.run {
          src = ./.;
          hooks = import ./git-hooks.nix {
            pkgs = pkgsFor.${system};
            treefmtWrapper = treefmtEval.${system}.config.build.wrapper;
          };
        }
      );
    in
    {
      formatter = forSupportedSystems (system: treefmtEval.${system}.config.build.wrapper);

      checks = forSupportedSystems (system: {
        formatting = treefmtEval.${system}.config.build.check self;
        pre-commit-check = gitHooksCheck.${system};
      });

      # devShell: Go toolchain + the treefmt formatter.
      # shellHook installs the git pre-commit hooks via git-hooks.nix.
      # Run `nix develop` once to install hooks; re-run after input or hook changes.
      devShells = forSupportedSystems (
        system:
        let
          pkgs = pkgsFor.${system};
        in
        {
          default = pkgs.mkShell {
            name = "copilotd";
            inherit (gitHooksCheck.${system}) shellHook;
            buildInputs = gitHooksCheck.${system}.enabledPackages;
            packages = [
              pkgs.go
              pkgs.gopls
              treefmtEval.${system}.config.build.wrapper
            ]
            ++ (builtins.attrValues treefmtEval.${system}.config.build.programs);
          };
        }
      );
    };
}
