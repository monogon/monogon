{ pkgs }: with pkgs;
# Only override for our actual build
if (!stdenv.hostPlatform.isStatic) then python3Minimal else
python3Minimal.overrideAttrs (old: {
  # Revert "fixup" which hardcodes a nix path.
  postPatch = old.postPatch + ''
    substituteInPlace Lib/subprocess.py \
      --replace-fail "'${bashNonInteractive}/bin/sh'" "'/bin/sh'"
  '';
})
