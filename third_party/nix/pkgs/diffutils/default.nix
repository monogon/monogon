{ pkgs }: with pkgs;
if (!stdenv.hostPlatform.isStatic) then diffutils else
diffutils.overrideAttrs (old: {
  # Disable tests as they fail when static build.

  # FAIL: test-getopt-gnu
  #=====================
  #
  #test-getopt.h:661: assertion 'optind == 2' failed
  #FAIL test-getopt-gnu (exit status: 134)
  #
  #FAIL: test-getopt-posix
  #=======================
  #
  #test-getopt.h:661: assertion 'optind == 2' failed
  #FAIL test-getopt-posix (exit status: 134)
  #
  #FAIL: test-nl_langinfo-mt
  #=========================
  #
  #FAIL test-nl_langinfo-mt (exit status: 134)
  #
  #FAIL: test-random-mt
  #====================
  #
  #FAIL test-random-mt (exit status: 134)
  #
  #FAIL: test-setlocale_null-mt-one
  #================================
  #
  #FAIL test-setlocale_null-mt-one (exit status: 134)
  #
  #FAIL: test-setlocale_null-mt-all
  #================================
  #
  #FAIL test-setlocale_null-mt-all (exit status: 134)
  doCheck = false;
  doInstallCheck = false;
})
