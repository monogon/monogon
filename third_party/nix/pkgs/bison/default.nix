{ pkgs }: with pkgs;
if (!stdenv.hostPlatform.isStatic) then bison else
bison.overrideAttrs (old: {
  # Check overrided file for more informations
  postPatch = ''
    cp ${./yacc.in} src/yacc.in
  '';

  env.BISON = "${builtins.placeholder "out"}/bin/bison";
})
