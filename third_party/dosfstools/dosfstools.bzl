load("@aspect_bazel_lib//lib:expand_template.bzl", "expand_template")
load("@rules_cc//cc:defs.bzl", "cc_binary")

cc_binary(
    name = "fsck",
    srcs = [
        "src/boot.c",
        "src/boot.h",
        "src/charconv.c",
        "src/charconv.h",
        "src/check.c",
        "src/check.h",
        "src/common.c",
        "src/common.h",
        "src/endian_compat.h",
        "src/fat.c",
        "src/fat.h",
        "src/file.c",
        "src/file.h",
        "src/fsck.fat.c",
        "src/fsck.fat.h",
        "src/io.c",
        "src/io.h",
        "src/lfn.c",
        "src/lfn.h",
        "src/msdos_fs.h",
        ":version.h",
    ],
    copts = ["-DHAVE_ENDIAN_H", "-DHAVE_VASPRINTF"],
    visibility = ["//visibility:public"],
    includes = ["."],
)

expand_template(
    name = "version.h_expanded",
    template = ":src/version.h.in",
    out = "version.h",
    substitutions = {
        # ONCHANGE(//build/bazel:third_party.MODULE.bazel): version needs to be kept in sync
        "@PACKAGE_VERSION@": "unstable-2022-07-25",
        "@RELEASE_DATE@": "2022-07-25",
    },
)
