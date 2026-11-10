{ pkgs, treefmtWrapper }:
{
  # Format everything via treefmt on commit.
  treefmt = {
    enable = true;
    packageOverrides.treefmt = treefmtWrapper;
  };

  # File hygiene for everything treefmt doesn't format (Markdown, config, ...).
  check-merge-conflicts.enable = true;
  trim-trailing-whitespace.enable = true;
  end-of-file-fixer = {
    enable = true;
    # Preserve the independently identified vendored response bytes exactly.
    excludes = [ "^internal/usage/pricing/modelsdevdata/api\\.json$" ];
  };
  mixed-line-endings = {
    enable = true;
    args = [ "--fix=lf" ];
  };

  # This project handles GitHub tokens and managed API tokens; a credential
  # committed by accident is the worst-case bug. Keep this on from day one.
  detect-private-keys.enable = true;

  # Enforce Conventional Commits (runs at the commit-msg stage).
  convco = {
    enable = true;
    # Work around nixpkgs' missing system libgit2 dependency for convco 0.7.2.
    # TODO(removal): drop this override once nixpkgs supplies the SHA-256-enabled libgit2.
    package = pkgs.convco.overrideAttrs (old: {
      buildInputs = old.buildInputs ++ [
        (pkgs.libgit2.override { withExperimentalSha256 = true; })
      ];
    });
  };
}
