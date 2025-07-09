{ sources ? import ./sources.nix }:
let
  pkgs = import sources.nixpkgs
    {
      overlays = [
        (self: super: {
          qemu-minimal = import ./pkgs/qemu { pkgs = super; };
          diffutils = import ./pkgs/diffutils { pkgs = super; };
          util-linux-minimal = import ./pkgs/util-linux { pkgs = super; };
          bazel-unwrapped = import ./pkgs/bazel { pkgs = super; };
          perl = import ./pkgs/perl { pkgs = super; };
          python3Minimal = import ./pkgs/python3 { pkgs = super; };
          bison = import ./pkgs/bison { pkgs = super; };
        })
        (self: super: {
          vde2 = super.vde2.overrideAttrs (oldAttrs: {
            env.NIX_CFLAGS_COMPILE = (oldAttrs.NIX_CFLAGS_COMPILE or "") + " -Wno-error=int-conversion -Wno-error=implicit-function-declaration";
          });
        })
      ];

      config.replaceCrossStdenv = { buildPackages, baseStdenv }:
        (buildPackages.withCFlags [ "-fPIC" ]) baseStdenv;
    };
in
pkgs // {
  lib.version = "${sources.nixpkgs.branch}.${sources.nixpkgs.rev}";
}
