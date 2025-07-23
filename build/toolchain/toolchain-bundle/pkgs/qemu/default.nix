{ qemu_kvm, audit, ... }:
let
  qemuMinimal = qemu_kvm.override (old: {
    hostCpuOnly = true;
    vncSupport = true;

    # Disable everything we don't need.
    enableDocs = false;
    ncursesSupport = false;
    seccompSupport = false;
    numaSupport = false;
    alsaSupport = false;
    pulseSupport = false;
    pipewireSupport = false;
    sdlSupport = false;
    jackSupport = false;
    gtkSupport = false;
    smartcardSupport = false;
    spiceSupport = false;
    usbredirSupport = false;
    xenSupport = false;
    cephSupport = false;
    glusterfsSupport = false;
    openGLSupport = false;
    rutabagaSupport = false;
    virglSupport = false;
    libiscsiSupport = false;
    smbdSupport = false;
    uringSupport = false;
    canokeySupport = false;
    capstoneSupport = false;
  });
in
qemuMinimal.overrideAttrs (old: {
  # Static build patch
  # Based on https://github.com/NixOS/nixpkgs/pull/333923

  patches = (old.patches ++ [
    ./static_build_crc32c_duplicate_definition.patch
  ]);

  configureFlags = (builtins.filter (v: v != "--static") old.configureFlags) ++ [ "--disable-libcbor" ];
  strictDeps = true;
  # a private dependency of PAM which is not linked explicitly in static builds
  buildInputs = old.buildInputs ++ [ audit ];
  env.NIX_LDFLAGS = " -laudit ";
})
