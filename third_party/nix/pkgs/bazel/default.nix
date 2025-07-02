{ pkgs }: with pkgs;
stdenv.mkDerivation {
  name = "bazel";
  src = builtins.fetchurl {
    url = "https://github.com/bazelbuild/bazel/releases/download/8.3.1/bazel-8.3.1-linux-x86_64";
    sha256 = "0k3067d06b8160day48afskr42c41bz0qgb3pk9mjpr4hj57w90p";
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
