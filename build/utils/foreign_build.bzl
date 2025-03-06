# Copyright The Monogon Project Authors.
# SPDX-License-Identifier: Apache-2.0

load("@rules_cc//cc:action_names.bzl", "CPP_LINK_EXECUTABLE_ACTION_NAME", "C_COMPILE_ACTION_NAME")
load("@rules_cc//cc/common:cc_common.bzl", "cc_common")
load("//build/toolchain/toolchain-bundle:toolchain.bzl", "TOOLCHAIN_ENV_SETUP", "build_toolchain_env")

DISABLED_FEATURES = []

def build_llvm_compiler_env(ctx, cc_toolchain, prefix = ""):
    feature_configuration = cc_common.configure_features(
        ctx = ctx,
        cc_toolchain = cc_toolchain,
        requested_features = ctx.features,
        unsupported_features = DISABLED_FEATURES + ctx.disabled_features,
    )
    c_compiler_path = cc_common.get_tool_for_action(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
    )
    c_compile_variables = cc_common.create_compile_variables(
        feature_configuration = feature_configuration,
        cc_toolchain = cc_toolchain,
        user_compile_flags = ctx.fragments.cpp.copts + ctx.fragments.cpp.conlyopts,
    )
    c_compiler_flags = cc_common.get_memory_inefficient_command_line(
        feature_configuration = feature_configuration,
        action_name = C_COMPILE_ACTION_NAME,
        variables = c_compile_variables,
    )
    c_linker_flags = cc_common.get_memory_inefficient_command_line(
        feature_configuration = feature_configuration,
        action_name = CPP_LINK_EXECUTABLE_ACTION_NAME,
        variables = c_compile_variables,
    )

    # NOTE: Multicall tool is called as path/to/llvm clang to workaround a bug
    # in out-of-process execution where tool name is repeated and parsing breaks.
    return {
        prefix + "CC_PATH": c_compiler_path.rsplit("/", 1)[0],
        prefix + "CC": c_compiler_path.rsplit("/", 1)[0] + "/llvm clang",
        prefix + "CXX": c_compiler_path.rsplit("/", 1)[0] + "/llvm clang++",
        prefix + "LD": c_compiler_path.rsplit("/", 1)[0] + "/ld.lld",
        prefix + "AR": c_compiler_path.rsplit("/", 1)[0] + "/llvm-ar",
        prefix + "NM": c_compiler_path.rsplit("/", 1)[0] + "/llvm-nm",
        prefix + "STRIP": c_compiler_path.rsplit("/", 1)[0] + "/llvm-strip",
        prefix + "OBJCOPY": c_compiler_path.rsplit("/", 1)[0] + "/llvm-objcopy",
        prefix + "OBJDUMP": c_compiler_path.rsplit("/", 1)[0] + "/llvm-objdump",
        prefix + "READELF": c_compiler_path.rsplit("/", 1)[0] + "/llvm-readelf",
        prefix + "CFLAGS": " ".join(c_compiler_flags),
        prefix + "LDFLAGS": " ".join(c_linker_flags),
    }, cc_toolchain.all_files

def merge_env(env, extra_env):
    for k, v in extra_env.items():
        if k in env:
            env[k] += " " + v
        else:
            env[k] = v
    return env

def generate_foreign_build_env(ctx, target_toolchain, exec_toolchain, toolchain_bundle_tools):
    env = {}

    # Figure out cc_toolchains
    target_toolchain_env, target_toolchain_inputs = build_llvm_compiler_env(ctx, target_toolchain)
    env = merge_env(env, target_toolchain_env)

    exec_toolchain_env, exec_toolchain_inputs = build_llvm_compiler_env(ctx, exec_toolchain, "HOST")
    env = merge_env(env, exec_toolchain_env)

    # Setup tools from toolchain-bundle.
    toolchain_bundle_env, toolchain_bundle_inputs = build_toolchain_env(ctx, toolchain_bundle_tools)
    env = merge_env(env, toolchain_bundle_env)

    inputs = depset(
        transitive = [
            target_toolchain_inputs,
            exec_toolchain_inputs,
            toolchain_bundle_inputs,
        ],
    )

    return env, inputs, TOOLCHAIN_ENV_SETUP
