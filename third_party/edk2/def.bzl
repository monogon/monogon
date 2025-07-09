# Copyright The Monogon Project Authors.
# SPDX-License-Identifier: Apache-2.0

load("@rules_cc//cc:find_cc_toolchain.bzl", "find_cpp_toolchain", "use_cc_toolchain")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load("@rules_cc//cc/common:cc_info.bzl", "CcInfo")
load("//build/utils:detect_root.bzl", "detect_root", "detect_roots")
load("//build/utils:foreign_build.bzl", "generate_foreign_build_env", "merge_env")
load("//build/utils:target_info.bzl", "TargetInfo")

TOOLCHAINS = [
    "//build/toolchain/toolchain-bundle:make_toolchain",
    "//build/toolchain/toolchain-bundle:nasm_toolchain",
    "//build/toolchain/toolchain-bundle:iasl_toolchain",
    "//build/toolchain/toolchain-bundle:strace_toolchain",
]

def _edk2_impl(ctx):
    _, libuuid_gen = detect_roots(ctx.attr._libuuid[CcInfo].compilation_context.direct_public_headers)
    extra_env = {
        "HOSTLDFLAGS": " -L ".join(
            [
                "",  # First element empty, for force a the join prefix
                detect_root(ctx.attr._libuuid.files.to_list()).rsplit("/", 1)[0],
            ],
        ),
        "HOSTCFLAGS": " -I ".join(
            [
                "",  # First element empty, for force a the join prefix
                libuuid_gen,
            ],
        ),
        "CROSS_LIB_UUID_INC": libuuid_gen.rsplit("/", 1)[0],
        "CROSS_LIB_UUID": detect_root(ctx.attr._libuuid.files.to_list()).rsplit("/", 1)[0],
    }

    inputs = depset(
        ctx.files.src +
        ctx.files._libuuid +
        ctx.attr._libuuid[CcInfo].compilation_context.direct_public_headers,
    )

    # Setup the environment for the foreign build.
    toolchain_env, toolchain_inputs, toolchain_cmd = generate_foreign_build_env(
        ctx = ctx,
        target_toolchain = find_cpp_toolchain(ctx),
        exec_toolchain = ctx.attr._exec_toolchain[cc_common.CcToolchainInfo],
        toolchain_bundle_tools = TOOLCHAINS,
    )

    target_arch = ctx.attr._target_arch[TargetInfo].value
    target_path = None
    export_script = None
    if target_arch == "X64":
        target_path = "OvmfPkg/OvmfPkgX64.dsc"
        export_script = """
            cp {src}/Build/OvmfX64/{release_type}_"$TOOLCHAIN"/FV/OVMF_CODE.fd {code}
            cp {src}/Build/OvmfX64/{release_type}_"$TOOLCHAIN"/FV/OVMF_VARS.fd {vars}
        """
    elif target_arch == "AARCH64":
        target_path = "ArmVirtPkg/ArmVirtQemu.dsc"
        export_script = """
            dd of="{code}" if=/dev/zero bs=1M count=64
            dd of="{code}" if={src}/Build/ArmVirtQemu-AARCH64/{release_type}_"$TOOLCHAIN"/FV/QEMU_EFI.fd conv=notrunc
            dd of="{vars}" if=/dev/zero bs=1M count=64
            dd of="{vars}" if={src}/Build/ArmVirtQemu-AARCH64/{release_type}_"$TOOLCHAIN"/FV/QEMU_VARS.fd conv=notrunc
        """
    else:
        fail("Unsupported target architecture: %s" % target_arch)

    code = ctx.actions.declare_file("CODE.fd")
    vars = ctx.actions.declare_file("VARS.fd")
    ctx.actions.run_shell(
        outputs = [code, vars],
        inputs = depset(transitive = [inputs, toolchain_inputs]),
        env = merge_env(toolchain_env, extra_env),
        progress_message = "Building EDK2 firmware",
        mnemonic = "BuildEDK2Firmware",
        command = toolchain_cmd + ("""
            TOOLCHAIN=CLANGDWARF
            export CLANG_BIN="$CC_PATH/"

            (
                cd {src}
                . edksetup.sh
                make \
                BUILD_OPTFLAGS="$HOSTCFLAGS" EXTRA_LDFLAGS="$HOSTLDFLAGS" \
                -C BaseTools/Source/C

                build -DTPM2_ENABLE -DSECURE_BOOT_ENABLE \
                -t $TOOLCHAIN -a {target_arch} -b {release_type} \
                -p $PWD/{target_path}
            ) > /dev/null
            """ + export_script).format(
            src = detect_root(ctx.attr.src.files.to_list()),
            code = code.path,
            vars = vars.path,
            target_arch = target_arch,
            target_path = target_path,
            release_type = ctx.attr._compilation_mode[TargetInfo].value,
        ),
        use_default_shell_env = True,
    )

    return [
        DefaultInfo(
            files = depset([code, vars]),
            runfiles = ctx.runfiles(files = [code, vars]),
        ),
    ]

edk2 = rule(
    doc = """
        Build EDK2 hermetically.
    """,
    implementation = _edk2_impl,
    attrs = {
        "src": attr.label(
            doc = """
                Filegroup containing EDK2 sources.
            """,
        ),
        "_libuuid": attr.label(
            default = "@libuuid//:uuid",
        ),
        "_exec_toolchain": attr.label(
            default = "@rules_cc//cc:current_cc_toolchain",
            cfg = "exec",
        ),
        "_target_arch": attr.label(
            default = "//third_party/edk2:target_arch",
        ),
        "_compilation_mode": attr.label(
            default = "//third_party/edk2:compilation_mode",
        ),
    },
    fragments = ["cpp"],
    toolchains = TOOLCHAINS + use_cc_toolchain(),
)
