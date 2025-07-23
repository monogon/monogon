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

# These are only for the headers of libcommon, which is a private dependency of libblkid and
# libuuid. Bazel doesn't support private dependencies and we want to avoid propagating these up
# to all users of libblkid and libuuid, so manually inject them one level up.
libcommon_hdrs_defines = [
    "_GNU_SOURCE",
    "HAVE_BYTESWAP_H",
    "HAVE_CLOSE_RANGE",
    "HAVE_CPU_SET_T",
    "HAVE_DECL_CPU_ALLOC",
    "HAVE_DIRFD",
    "HAVE_ENDIAN_H",
    "HAVE_ERR_H",
    "HAVE_ERR",
    "HAVE_ERRX",
    "HAVE_LOFF_T",
    "HAVE_MEMPCPY",
    "HAVE_NANOSLEEP",
    "HAVE_SETNS",
    "HAVE_SRANDOM",
    "HAVE_STDIO_EXT_H",
    "HAVE_STRNDUP",
    "HAVE_STRNLEN",
    "HAVE_SENDFILE",
    "HAVE_PIDFD_SEND_SIGNAL",
    "HAVE_PIDFD_OPEN",
    "HAVE_PATHS_H",
    "HAVE_LINUX_VERSION_H",
    "HAVE_OPENAT",
    "HAVE_SYS_IOCTL_H",
    "HAVE_SYS_SENDFILE_H",
    "HAVE_SYS_SYSMACROS_H",
    "HAVE_SYS_TTYDEFAULTS_H",
    "HAVE_TIMEGM",
    "HAVE_TIMER_CREATE",
    "HAVE_UNSHARE",
    "HAVE_WARN",
    "HAVE_WARNX",
    "HAVE_WIDECHAR",
    "HAVE_FSYNC",
    "HAVE___FPENDING",
]

expand_template(
    name = "blkid.h_expanded",
    template = ":libblkid/src/blkid.h.in",
    out = "blkid.h",
    substitutions = {
        "@LIBBLKID_VERSION@": "0.0.0",
        "@LIBBLKID_DATE@": "01.01.1970",
    },
)

cc_library(
    name = "common",
    srcs = glob(["lib/*.h"]) + [
        "lib/blkdev.c",
        "lib/canonicalize.c",
        "lib/crc32.c",
        "lib/crc32c.c",
        "lib/env.c",
        "lib/idcache.c",
        "lib/encode.c",
        "lib/fileutils.c",
        "lib/color-names.c",
        "lib/mangle.c",
        "lib/match.c",
        "lib/mbsalign.c",
        "lib/mbsedit.c",
        "lib/md5.c",
        "lib/pager.c",
        "lib/procutils.c",
        "lib/pwdutils.c",
        "lib/randutils.c",
        "lib/setproctitle.c",
        "lib/strutils.c",
        "lib/sysfs.c",
        "lib/timeutils.c",
        "lib/ttyutils.c",
        "lib/strv.c",
        "lib/path.c",
        "lib/cpuset.c",
        "lib/sha1.c",
        "lib/signames.c",
    ],
    hdrs = glob(["include/*.h"]),
    # Locale-related defines are intentionally missing as we don't want locale support
    local_defines = libcommon_hdrs_defines + [
        "HAVE_DECL_CPU_ALLOC",
        "HAVE_ENVIRON_DECL",
        "HAVE_ERRNO_H",
        "HAVE_GETDTABLESIZE",
        "HAVE_GETRANDOM",
        "HAVE_GETRLIMIT",
        "HAVE_LINUX_CDROM_H",
        "HAVE_LINUX_FD_H",
        "HAVE_JRAND48",
        "HAVE_MKOSTEMP",
        "HAVE_SYS_PRCTL_H",
        "HAVE_MNTENT_H",
        "HAVE_PRCTL",
        "HAVE_SECURE_GETENV",
        "HAVE_STRUCT_STAT_ST_MTIM_TV_NSEC",
        "HAVE_SYS_STAT_H",
        "HAVE_SYS_TYPES_H",
        "HAVE_EXPLICIT_BZERO",
        "HAVE_UNISTD_H",
        "HAVE_TLS",
    ],
    visibility = ["//visibility:private"],
)

cc_library(
    name = "uuid",
    srcs = [
        "libuuid/src/clear.c",
        "libuuid/src/compare.c",
        "libuuid/src/copy.c",
        "libuuid/src/gen_uuid.c",
        "libuuid/src/isnull.c",
        "libuuid/src/pack.c",
        "libuuid/src/parse.c",
        "libuuid/src/predefined.c",
        "libuuid/src/unpack.c",
        "libuuid/src/unparse.c",
        "libuuid/src/uuidP.h",
        "libuuid/src/uuid_time.c",
    ],
    hdrs = [
        "libuuid/src/uuid.h",
        "libuuid/src/uuidd.h",
    ],
    local_defines = libcommon_hdrs_defines + [
        "HAVE_NET_IF_H",
        "HAVE_NETINET_IN_H",
        "HAVE_STDLIB_H",
        "HAVE_SYS_FILE_H",
        "HAVE_SYS_SOCKET_H",
        "HAVE_SYS_SYSCALL_H",
        "HAVE_SYS_TIME_H",
        "HAVE_SYS_UN_H",
        "HAVE_TLS",
        "HAVE_UNISTD_H",
    ],
    visibility = ["//visibility:public"],
    deps = [":common"],
)

cc_library(
    name = "blkid",
    srcs = glob([
        "libblkid/src/**/*.c",
        "libblkid/src/**/*.h",
    ]),
    hdrs = [":blkid.h"],
    local_defines = libcommon_hdrs_defines + [
        "LIBBLKID_VERSION=\\\"0.0.0\\\"",
        "LIBBLKID_DATE=\\\"01.01.1970\\\"",
        "HAVE_SYS_STAT_H",
        "HAVE_ERRNO_H",
        "HAVE_LINUX_CDROM_H",
        "HAVE_POSIX_FADVISE",
        "HAVE_STDLIB_H",
        "HAVE_STRUCT_STAT_ST_MTIM_TV_NSEC",
        "HAVE_SYS_STAT_H",
        "HAVE_SYS_TYPES_H",
        "HAVE_UNISTD_H",
    ],
    visibility = ["//visibility:public"],
    deps = ["//:common"],
)
