# How to set up a build environment

We strongly recommend a Linux workstation - it offers the
best developer experience. NixOS, Fedora or Ubuntu are good choices.

On Windows, use WSL2. Building on ARM64 - including on Apple Silicon Macs (M1, M2, etc.) - will be supported soon.

The more CPU cores, the merrier - our build is fully parallelized, but the monorepo builds
a LOT of stuff, including all of EDK2, QEMU, the Linux kernel, Kubernetes... Bazel is smart about
not rebuilding things that haven't changed, but sometimes, you'll still be hit with a full rebuild
(such as when the sysroot or Bazel version changes).

We will offer pre-warmed build caches in the future, but for now, bring a big rig!

## Dependencies

Monogon's monorepo uses [Bazel](https://bazel.build) for fast, hermetic and fully reproducible builds.

Our build environment brings its own hermetic and reproducible sysroot,
so we only require minimal host dependencies:

- Any Linux distribution with a reasonably recent kernel with unprivileged 
  user namespaces enabled. Bazel requires user namespaces to set up its hermetic per-action 
  sandbox without special privileges or capabilities.

- [Bazelisk](https://github.com/bazelbuild/bazelisk) >= v1.15.0. Bazel is serious about breaking
  backwards compatibility with each major release, so you need the right version to build the repo.
  Bazelisk downloads (and verifies) the correct version of Bazel for you. It's the de-facto standard
  way of using Bazel, a bit like rustup is for Rust users.

The following distributions are known to work:

- NixOS (see below)
- Fedora
- Ubuntu
- Debian >= 11
- RHEL / Alma / Rocky >= 8.4

You can use this snippet to install the official Bazelisk release binary to `/usr/local/bin`:

```bash
TMPFILE=$(mktemp) && \
  curl -L -o $TMPFILE \
    https://github.com/bazelbuild/bazelisk/releases/download/v1.26.0/bazelisk-linux-amd64 && \
  sha256sum -c - <<< "6539c12842ad76966f3d493e8f80d67caa84ec4a000e220d5459833c967c12bc  $TMPFILE" && \
  sudo install -m 0755 $TMPFILE /usr/local/bin/bazel && \
  rm $TMPFILE
```

Alternatively, if you have a Go >= 1.16 toolchain, you can compile it yourself:

```bash
# This uses Go's transparency log for pinning to ensure the release hasn't been tampered with.
go install github.com/bazelbuild/bazelisk@v1.26.0 
sudo mv ~/go/bin/bazelisk /usr/local/bin/bazel
```

### /dev/kvm access for test suites

Monogon's tests make extensive use of KVM to run virtual machines, both to test the OS as well
as running various microVM-based unit tests. If you want to run all tests, you'll need to make sure
that your local user has access to `/dev/kvm`. You can check this by running `touch /dev/kvm`.

If you only want to build artifacts without running tests, no KVM access is required.

On most Linux distributions, you can add your user to the `kvm` group to allow access to `/dev/kvm`:

```bash
sudo gpasswd -a $USER kvm
# re-login or run "sudo -u $USER -i" to get a shell with the new group membership
```

If you are running in a virtual machine, make sure that your virtualization software supports
nested virtualization - otherwise, it won't be possible to use KVM inside the VM.

`/dev/kvm` is considered safe to use by unprivileged users. All of Monogon's monorepo can
be built and tested without root privileges or other dangerous capabilities.

### NixOS

We fully support building on NixOS, and we provide a `shell.nix` file to make it easy. Just run `nix-shell` in the
project root! This will drop you into a shell with all dependencies installed, and you can run `bazel ...` as usual.

You can also execute tools/bazel directly on a system with Nix installed. The wrapper automatically uses the nix-shell.

## VSCode / gopls

The monorepo is compatible with gopls via GOPACKAGESDRIVER, which uses a Bazel backend to resolve generated code.

The [included VSCode workspace config](.vscode/settings.json) should work out of the box.

The following extensions are recommended:
- [Go](https://marketplace.visualstudio.com/items?itemName=golang.go)
- [proto3](https://marketplace.visualstudio.com/items?itemName=zxh404.vscode-proto3)
- [Bazel](https://marketplace.visualstudio.com/items?itemName=BazelBuild.vscode-bazel)


## IntelliJ

This repository is compatible with the IntelliJ Bazel plugin out of the box, which enables
full autocompletion for external dependencies and generated code.

The following steps are necessary:

- Install the [Bazel](https://plugins.jetbrains.com/plugin/8609-bazel),
  Go and Protocol Buffer plugins in IntelliJ.
- Make sure that Bazel "*Bazel Binary Location*" in Other Settings → Bazel Settings points to Bazelisk.
- Use _File → Import Bazel project_... and select your monorepo checkout.

After running the first sync (Alt-Y), everything should now resolve in the IDE, including generated code.
Whenever the project structure changes, re-run the sync to update the IDE.

It can be useful to configure an External Tool to run Gazelle and add a keyboard shortcut
to quickly run it after changing the project layout.

## Trouble?

Developer experience is very important. Please file a GitHub issue if you run into any problems
or encounter any pain points - we want to fix them!
