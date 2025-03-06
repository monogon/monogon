# Toolchain Bundle (`toolchain-bundle/`)

To ensure that tools like `make`, `nasm`, `qemu`, or `perl` are available in the Bazel build environment, we provide a `toolchain-bundle`. This bundle is pre-built and fetched as an external repository, allowing Bazel to use these tools without needing to install them on the host system. They are built for both `x86_64-unknown-linux-musl` and `aarch64-unknown-linux-musl` platforms with Nix.

You can build these toolchains by invoking the `nix-build` via `nix-build build/toolchain/toolchain-bundle/default.nix`

---

# Rust EFI Toolchain (`rust-efi/`)

The `rust-efi` directory configures a Rust toolchain for building EFI applications.