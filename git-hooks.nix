{ pkgs, treefmtWrapper }:
{
  # Format everything via treefmt on commit.
  treefmt = {
    enable = true;
    packageOverrides.treefmt = treefmtWrapper;
  };

  # This project handles GitHub tokens and managed API tokens; a credential
  # committed by accident is the worst-case bug. Keep this on from day one.
  detect-private-keys.enable = true;

  # Enforce Conventional Commits (runs at the commit-msg stage).
  convco.enable = true;
}
