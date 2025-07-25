load("@io_bazel_rules_go//go:def.bzl", "go_binary")
load("//osbase/build/genproductinfo:test.bzl", "test_product_info")
load("//osbase/build/mkerofs:def.bzl", "erofs_image")
load("//osbase/build/mkoci:def.bzl", "oci_os_image")
load("//osbase/build/mkoci/index:def.bzl", "oci_index")
load("//osbase/build/mkpayload:def.bzl", "efi_unified_kernel_image")
load("//osbase/build/mkverity:def.bzl", "verity_image")

# Macro for generating multiple TestOS instances to check if the updater works.
# buildifier: disable=unnamed-macro
def testos(variant):
    erofs_image(
        name = "rootfs_" + variant,
        files = {
            "/init": ":testos_" + variant,
            "/etc/product-info.json": ":product_info_" + variant,
            "/etc/resolv.conf": "//osbase/net/dns:resolv.conf",
        },
        fsspecs = [
            "//osbase/build:earlydev.fsspec",
            ":rootfs.fsspec",
        ],
    )

    verity_image(
        name = "verity_rootfs_" + variant,
        source = ":rootfs_" + variant,
    )

    efi_unified_kernel_image(
        name = "kernel_efi_" + variant,
        cmdline = "console=ttyS0 quiet rootfstype=erofs init=/init loadpin.exclude=kexec-image,kexec-initramfs",
        kernel = "//third_party/linux",
        verity = ":verity_rootfs_" + variant,
    )

    test_product_info(
        name = "product_info_" + variant,
        os_id = "testos_" + variant,
        os_name = "Test OS " + variant.upper(),
    )

    oci_os_image(
        name = "testos_image_" + variant,
        srcs = {
            "system": ":verity_rootfs_" + variant,
            "kernel.efi": ":kernel_efi_" + variant,
        },
        product_info = ":product_info_" + variant,
        visibility = ["//metropolis/node/core/update/e2e:__pkg__"],
    )

    oci_index(
        name = "testos_multiarch_" + variant,
        src = ":testos_image_" + variant,
        platforms = [
            "//build/platforms:linux_x86_64",
            "//build/platforms:linux_aarch64",
        ],
        tags = ["manual"],
        visibility = ["//metropolis/node/core/update/e2e:__pkg__"],
    )

    go_binary(
        name = "testos_" + variant,
        embed = [":testos_lib"],
        visibility = ["//visibility:public"],
        x_defs = {"source.monogon.dev/metropolis/node/core/update/e2e/testos.Variant": variant.upper()},
    )
