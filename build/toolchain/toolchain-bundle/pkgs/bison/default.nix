{ super, ... }:
super.bison.overrideAttrs (_: {
  # Check overrided file for more informations
  postPatch = ''
    cp ${./yacc.in} src/yacc.in
  '';

  env.BISON = "${builtins.placeholder "out"}/bin/bison";
})
