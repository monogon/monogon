{ pkgs }: with pkgs;
util-linux.override (old: {
  pamSupport = false;
  ncursesSupport = false;
  capabilitiesSupport = false;
  systemdSupport = false;
  translateManpages = false;
  nlsSupport = false;
  shadowSupport = false;
  writeSupport = false;
})
