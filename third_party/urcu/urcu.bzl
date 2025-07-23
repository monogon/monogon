load("@aspect_bazel_lib//lib:expand_template.bzl", "expand_template")
load("@rules_cc//cc:defs.bzl", "cc_library")

expand_template(
    name = "config.h_expanded",
    template = ":config.h.in",
    out = "config.h",
    substitutions = {},
)

cc_library(
    name = "urcu",
    srcs = glob(
        [
            "src/*.c",
            "src/*.h",
        ],
    ),
    hdrs = glob(["include/**/*.h"]),
    includes = ["include"],
    local_defines = ["RCU_MEMBARRIER", "_GNU_SOURCE"],
    visibility = ["//visibility:public"],
)
