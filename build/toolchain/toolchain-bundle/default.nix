{ pkgs ? import ../../../third_party/nix/default.nix { } }: with pkgs;
symlinkJoin {
  name = "toolchain";
  paths =
    let
      platforms = with pkgsCross; [
        aarch64-multiplatform-musl
        musl64
      ];
    in
    map
      (platform: (
        let
          merged = buildEnv {
            name = "toolchain-env";
            paths = with platform.pkgsStatic; [
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
          };
        in
        stdenv.mkDerivation rec {
          name = "toolchain-bundle";
          buildInputs = [ gnutar zstd ];

          phases = [ "buildPhase" "installPhase" ];
          buildPhase = ''
            tar --zstd --sort=name --hard-dereference -hcf bundle.tar.zst -C ${merged} .
          '';

          installPhase = ''
            mkdir $out
            mv bundle.tar.zst $out/${name}-${platform.hostPlatform.config}.tar.zst
          '';
        }
      ))
      platforms;
}
