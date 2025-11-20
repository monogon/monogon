load("@rules_go//go:def.bzl", "go_binary", "go_library")
load("//osbase/build/mkcpio:def.bzl", "node_initramfs")
load("//osbase/build/mkpayload:def.bzl", "efi_unified_kernel_image")

# buildifier: disable=unnamed-macro
def bringup_test(variant):
    go_library(
        name = "%s_lib" % variant,
        srcs = ["main_%s.go" % variant],
        importpath = "source.monogon.dev/osbase/bringup/test",
        visibility = ["//visibility:private"],
        deps = [
            "//osbase/bootparam",
            "//osbase/bringup",
            "//osbase/efivarfs",
            "//osbase/logtree",
            "//osbase/supervisor",
            "@org_golang_x_sys//unix",
            "@org_uber_go_multierr//:multierr",
        ],
    )

    go_binary(
        name = "%s_bin" % variant,
        embed = [":%s_lib" % variant],
        visibility = ["//visibility:private"],
    )

    node_initramfs(
        name = "initramfs_%s" % variant,
        files = {
            "/init": ":%s_bin" % variant,
        },
        fsspecs = [
            "//osbase/build:earlydev.fsspec",
        ],
        visibility = ["//visibility:private"],
    )

    efi_unified_kernel_image(
        name = "kernel_%s" % variant,
        cmdline = "quiet console=ttyS0 init=/init",
        initrd = [":initramfs_%s" % variant],
        kernel = "//third_party/linux",
        visibility = ["//visibility:private"],
    )
