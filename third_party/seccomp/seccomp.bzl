#  Copyright 2020 The Monogon Project Authors.
#
#  SPDX-License-Identifier: Apache-2.0
#
#  Licensed under the Apache License, Version 2.0 (the "License");
#  you may not use this file except in compliance with the License.
#  You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
#  Unless required by applicable law or agreed to in writing, software
#  distributed under the License is distributed on an "AS IS" BASIS,
#  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#  See the License for the specific language governing permissions and
#  limitations under the License.

load("@aspect_bazel_lib//lib:expand_template.bzl", "expand_template")
load("@rules_cc//cc:defs.bzl", "cc_library")

genrule(
    name = "config-h",
    outs = ["configure.h"],
    cmd = "echo \"#define HAVE_LINUX_SECCOMP_H 1\" > \"$@\"",
    visibility = ["//visibility:public"],
)

genrule(
    name = "syscalls-tables",
    srcs = [
        "src/syscalls.perf.template",
        "src/syscalls.csv",
    ],
    outs = ["syscalls.perf.c"],
    cmd = """
    # From src/arch-gperf-generate, modified to not write over source files
    grep -v '^#' $(location src/syscalls.csv) | nl -ba -s, -v0 | \
        sed -e 's/^[[:space:]]\\+\\([0-9]\\+\\),\\([^,]\\+\\),\\(.*\\)/\\2,\\1,\\3/' \
            -e ':repeat; {s|\\([^,]\\+\\)\\(.*\\)[^_]PNR|\\1\\2,__PNR_\\1|g;}; t repeat' \
             > "$(@D)/syscalls_tmp.csv"

    # create the gperf file
    sed -e "/@@SYSCALLS_TABLE@@/r $(@D)/syscalls_tmp.csv" \
        -e '/@@SYSCALLS_TABLE@@/d' \
        $(location src/syscalls.perf.template) > "$(@D)/syscalls.perf"
    ./$(location @gperf//:gperf) -m 100 --null-strings --pic -tCEG -T -S1 --output-file="$(location syscalls.perf.c)" "$(@D)/syscalls.perf"
    """,
    tools = [
        "@gperf//:gperf",
    ],
)

expand_template(
    name = "seccomp.h_expanded",
    template = ":include/seccomp.h.in",
    out = "seccomp.h",
    substitutions = {
        # Known dependencies relying on this version information:
        # - @com_github_seccomp_libseccomp_golang//:libseccomp-golang
        "@VERSION_MAJOR@": "2",
        "@VERSION_MINOR@": "5",
        "@VERSION_MICRO@": "1",
    },
)

cc_library(
    name = "seccomp",
    srcs = glob(
        [
            "src/*.c",
            "src/*.h",
        ],
        exclude = [
            "src/arch-syscall-check.c",
            "src/arch-syscall-dump.c",
        ],
    ) + ["//:configure.h", ":syscalls.perf.c"],
    hdrs = [
        ":seccomp.h",
        "include/seccomp-syscalls.h",
    ],
    includes = ["."],
    visibility = ["//visibility:public"],
)
