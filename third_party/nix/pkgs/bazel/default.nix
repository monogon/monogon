{ pkgs }: with pkgs;
stdenv.mkDerivation {
  name = "bazel";
  src = builtins.fetchurl {
    url = "https://github.com/bazelbuild/bazel/releases/download/8.1.0/bazel-8.1.0-linux-x86_64";
    sha256 = "19dwgh631d6c1m4ds1b1b3pbz18zm5i0x8bggjgsc04fyljfbfml";
  };
  unpackPhase = ''
    true
  '';
  nativeBuildInputs = [ makeWrapper ];
  buildPhase = ''
    mkdir -p $out/bin
    cp $src $out/bin/.bazel-inner
    chmod +x $out/bin/.bazel-inner

    cp ${./bazel-inner.sh} $out/bin/bazel
    chmod +x $out/bin/bazel

    # Use wrapProgram to set the actual bazel path
    wrapProgram $out/bin/bazel --set BAZEL_REAL $out/bin/.bazel-inner
  '';
  dontStrip = true;
}
