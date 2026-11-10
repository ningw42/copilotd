{ ... }:
{
  projectRootFile = "flake.nix";

  # Nix — the flake and its hook/formatter modules.
  programs.nixfmt.enable = true;

  # Go — the project language.
  programs.gofmt.enable = true;
}
