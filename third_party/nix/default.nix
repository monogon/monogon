{ sources ? import ./sources.nix, overlays ? [ ], config ? { } }:
let
  pkgs = import sources.nixpkgs
    {
      overlays = overlays ++ [
        (self: super: {
          bazel_8 = self.callPackage ./pkgs/bazel_8/package.nix { };
        })
      ];
      config = config;
    };
in
pkgs // {
  lib.version = "${sources.nixpkgs.branch}.${sources.nixpkgs.rev}";
}
