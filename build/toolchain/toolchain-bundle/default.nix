let
  # We want our overrides to only apply when building for static environments.
  conditionalOverlay = condition: overlay: (if condition then overlay else { });

  pkgs = import ../../../third_party/nix/default.nix {
    overlays = [
      # Overrides for allowing static builds
      (self: super: conditionalOverlay super.stdenv.hostPlatform.isStatic (with self; {
        # A minimal version of qemu allowing for static builds.
        qemu-minimal = self.callPackage ./pkgs/qemu { inherit super; };

        # Static perl builds are a rabbit hole as they need patches
        # and use of undocumented options. Check the derivation for more infos.
        perl = self.callPackage ./pkgs/perl { inherit super; };

        # Bison requires an override for not hardcoding nix paths.
        bison = self.callPackage ./pkgs/bison { inherit super; };

        # Provide a custom minimal version of util-linux
        util-linux-minimal = super.util-linux.override (old: {
          pamSupport = false;
          ncursesSupport = false;
          capabilitiesSupport = false;
          systemdSupport = false;
          translateManpages = false;
          nlsSupport = false;
          shadowSupport = false;
          writeSupport = false;
        });

        # Revert "fixup" which hardcodes a nix path.
        python3Minimal = super.python3Minimal.overrideAttrs (old: {
          postPatch = old.postPatch + ''
            substituteInPlace Lib/subprocess.py \
              --replace-fail "'${bashNonInteractive}/bin/sh'" "'/bin/sh'"
          '';
        });

        # Disable tests as they fail when static build.
        diffutils = super.diffutils.overrideAttrs (_: {
          doCheck = false;
          doInstallCheck = false;
        });

        # vde2 currently doesn't build without these additional flags.
        vde2 = super.vde2.overrideAttrs (oldAttrs: {
          env.NIX_CFLAGS_COMPILE = (oldAttrs.NIX_CFLAGS_COMPILE or "") + " -Wno-error=int-conversion -Wno-error=implicit-function-declaration";
        });
      }))
    ];

    config.replaceCrossStdenv = { buildPackages, baseStdenv }:
      (buildPackages.withCFlags [ "-fPIC" ]) baseStdenv;
  };

  # All platforms we want to build for.
  mkPlatforms = platforms: with platforms; [
    aarch64-multiplatform-musl
    musl64
  ];

  # All packages that we want in our bundle.
  mkPackages = platformPkgs: with platformPkgs; [
    gnumake
    flex
    bison
    lz4
    busybox
    findutils
    bc
    util-linux-minimal # custom pkg
    perl
    nasm
    acpica-tools
    patch
    diffutils
    qemu-minimal # custom pkg
    m4
    strace
    python3Minimal
  ];

  mkPackagesEnv = platform: pkgs.buildEnv {
    name = "toolchain-${platform.hostPlatform.config}";
    paths = mkPackages platform.pkgsStatic;
  };

  mkBundle = platform: pkgs.stdenv.mkDerivation rec {
    name = "toolchain-bundle-${platform.hostPlatform.config}";
    buildInputs = [ pkgs.gnutar pkgs.zstd ];

    phases = [ "buildPhase" ];
    buildPhase =
      let
        merged = mkPackagesEnv platform;
      in
      ''
        mkdir $out
        tar --zstd --sort=name --hard-dereference -hcf $out/${name}.tar.zst -C ${merged} .
      '';
  };
in
with pkgs; symlinkJoin {
  name = "toolchain";
  paths =
    let
      platforms = mkPlatforms pkgs.pkgsCross;
    in
    map mkBundle platforms;
}
