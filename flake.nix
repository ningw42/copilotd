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

      # Version metadata is sourced deterministically from flake attributes (not
      # impure date/VCS calls), so the build stays reproducible. shortRev is
      # absent on a dirty tree, hence the "dirty" fallback.
      commit = self.shortRev or "dirty";
      date = self.lastModifiedDate or "unknown";

      # The Go toolchain is pinned explicitly to 1.26 in BOTH the package build
      # and the devShell, so `nix flake update` cannot silently change the Go
      # minor. Bump this pin deliberately, once per Go release.
      copilotdFor = forSupportedSystems (
        system:
        let
          pkgs = pkgsFor.${system};
        in
        (pkgs.buildGoModule.override { go = pkgs.go_1_26; }) {
          pname = "copilotd";
          version = commit;
          src = ./.;
          # Non-vendored: go.mod/go.sum are the source of truth; a single
          # vendorHash covers the whole fetched dependency set. It changes only
          # when dependencies change.
          vendorHash = "sha256-iL7CguyDDJDVyH/3g+XHGChL2GvXfWQRCXjwd22ZOQ0=";

          # CGO off -> a truly static binary on Linux. On Darwin, Go always links
          # libSystem (Apple ships no fully-static binaries), so the aarch64-darwin
          # artifact is self-contained except for libSystem. The "single static
          # binary" claim is Linux-precise.
          env.CGO_ENABLED = "0";

          ldflags = [
            "-s"
            "-w"
            # Version is deliberately left at its "dev" default (§8): there is no
            # release-versioning scheme yet. Only Commit and Date are sourced
            # from the flake, so the build stays reproducible.
            "-X github.com/ningw42/copilotd/internal/build.Commit=${commit}"
            "-X github.com/ningw42/copilotd/internal/build.Date=${date}"
          ];

          # doCheck defaults to true, so `go test ./...` runs in the checkPhase
          # during the package build (and therefore under `nix flake check`).

          meta = {
            description = "Run Anthropic Messages API and OpenAI Responses API off GitHub Copilot";
            mainProgram = "copilotd";
          };
        }
      );
    in
    {
      formatter = forSupportedSystems (system: treefmtEval.${system}.config.build.wrapper);

      packages = forSupportedSystems (system: {
        default = copilotdFor.${system};
      });

      # `nix run` launches the binary.
      apps = forSupportedSystems (system: {
        default = {
          type = "app";
          program = "${copilotdFor.${system}}/bin/copilotd";
          meta.description = "Run the copilotd binary";
        };
      });

      checks = forSupportedSystems (system: {
        formatting = treefmtEval.${system}.config.build.check self;
        pre-commit-check = gitHooksCheck.${system};
        # Building the package compiles it and runs the test suite via its
        # checkPhase, so `nix flake check` gives local verification without CI.
        package = copilotdFor.${system};
      });

      # devShell: Go toolchain (pinned) + the treefmt formatter.
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
              pkgs.go_1_26
              pkgs.gopls
              treefmtEval.${system}.config.build.wrapper
            ]
            ++ (builtins.attrValues treefmtEval.${system}.config.build.programs);
          };
        }
      );
    };
}
