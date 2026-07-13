{ ... }:
{
  projectRootFile = "flake.nix";

  # Nix — we write it now.
  programs.nixfmt.enable = true;

  # Go — the project language. No-ops until the first .go file lands.
  programs.gofmt.enable = true;
}
