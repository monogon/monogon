# If you're on NixOS, use me! `nix-shell --pure`.
{ pkgs ? (import ./third_party/nix { }), extraConf ? "" }:
let
  wrapper = pkgs.writeScript "wrapper.sh"
    ''
      # Fancy colorful PS1 to make people notice easily they're in the Monogon Nix shell.
      PS1='\[\033]0;\u/monogon:\w\007\]'
      if type -P dircolors >/dev/null ; then
        PS1+='\[\033[01;35m\]\u/monogon\[\033[01;36m\] \w \$\[\033[00m\] '
      fi
      export PS1

      # Use Nix-provided cert store.
      export NIX_SSL_CERT_FILE="${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
      export SSL_CERT_FILE="${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"

      # Let some downstream machinery know we're on NixOS. This is used mostly to
      # work around Bazel/NixOS interactions.
      export MONOGON_NIXOS=yep

      # Convince rules_go to use /bin/bash and not a NixOS store bash which has
      # no idea how to resolve other things in the nix store once PATH is
      # stripped by (host_)action_env.
      export BAZEL_SH=/bin/bash

      # buildFHSEnv makes /etc a tmpfs and symlinks some files from host /etc.
      # Create some additional symlinks for files we want from host /etc.
      for i in bazel.bazelrc gitconfig; do
          if [[ -e "/.host-etc/$i" ]] && [[ ! -e "/etc/$i" ]]; then
              ln -s "/.host-etc/$i" "/etc/$i"
          fi
      done

      ${extraConf}

      # Allow passing a custom command via env since nix-shell doesn't support
      # this yet: https://github.com/NixOS/nix/issues/534
      if [ ! -n "$COMMAND" ]; then
          COMMAND="bash --noprofile --norc"
      fi
      exec $COMMAND
    '';
in
(pkgs.buildFHSEnv {
  name = "monogon-nix";
  targetPkgs = targetPkgs: with targetPkgs; [
    bazel-unwrapped # Our custom bazel package based on upstream
    git
    buildifier
    zlib
    curl
    gcc
    binutils
    openjdk21
    patch
    python3
    busybox
    niv
    google-cloud-sdk
    swtpm
    nix
  ];
  runScript = wrapper;
}).env
