# Bazel 8 Nix Package

This build is based on https://github.com/NixOS/nixpkgs/pull/400941, the only
difference being the addition of `bash` to the default shell environment. As 
soon as the PR is merged, we can replace it with a small override to inject
bash into the dependencies.