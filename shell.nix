# If you're on NixOS, use me! `nix-shell --pure`.
{ pkgs ? (import ./third_party/nix { }) }:
pkgs.mkShell {
  # Let some downstream machinery know we're on NixOS. This is used mostly to
  # work around Bazel/NixOS interactions.
  env.MONOGON_NIXOS="yep";

  buildInputs = with pkgs; [
    bazel_8 # Our custom bazel package
    python3 # Workspace status script
    git # Bazel expects git to be available
    gnupg # our gopass integration requires gpg in the PATH
    niv # For updating third_party/nix
    google-cloud-sdk # Pushing containers to GCR
  ];
}
