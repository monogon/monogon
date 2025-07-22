# Monogon Monorepo

This is the main repository containing the source code for the [Monogon Platform](https://monogon.tech).

*This is pre-release software - take a look, and check back later! In the meantime, join us on Matrix ([`#monogon-os-community:matrix.org`](https://app.element.io/#/room/#monogon-os-community:matrix.org)) or [Discord](https://discord.gg/ajYd42xzXV)*.

## Environment

Our build environment is self-contained and requires only minimal host dependencies:

- A Linux machine or VM.
- [Bazelisk](https://github.com/bazelbuild/bazelisk) >= v1.15.0 (or a working Nix environment).
- `git` to check out modules.
- `python3` to generate the workspace status to stamp.
- A reasonably recent kernel, user namespaces are recommended for performance reasons.
- Working KVM with access to `/dev/kvm` (if you want to run tests).

Our docs assume that Bazelisk is available as `bazel` on your PATH.

Refer to [SETUP.md](./SETUP.md) for detailed instructions.

## Monogon OS

The source code lives in [`//metropolis`](./metropolis) (Metropolis is the codename of Monogon OS).

See the [`//metropolis/README.md`](./metropolis/README.md) for a developer quick start guide, or see
the [Monogon OS Handbook](https://docs.monogon.dev/metropolis-v0.1/handbook/index.html) for user documentation.
