{ lib, super, ... }@inputs:
let
  # Passthrough default configuration without our custom super attribute. Perl
  # requires itself which breaks when we don't pass through the default attributes.
  perl = super.perl.override (_: (lib.filterAttrs (name: _: name != "super") inputs));
in
perl.overrideAttrs (old: {
  patches = old.patches ++ [
    ./static_build.patch
  ];

  preConfigure = old.preConfigure + ''
    cat >> config.over <<EOF
    osvers="musllinux"
    EOF
  '';

  configureFlags = old.configureFlags ++ [
    "-Dotherlibdirs=.../../lib/perl5/${old.version}" # Tell perl to use a relative libdir
    # 1. Why isn't this the default?
    # 2. Apparently nobody uses this option, because it is missing the quotes inside the config_h.SH
    # 3. Why should a variable called "procselfexe" be used with a different path than /proc/self/exe?
    # 4. I really dislike perl. - fionera
    "-Dprocselfexe=\"/proc/self/exe\""
  ];

  env.NIX_CFLAGS_COMPILE = (old.NIX_CFLAGS_COMPILE or "") + " -Wno-error=implicit-function-declaration";
})
