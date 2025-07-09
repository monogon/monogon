load("@rules_foreign_cc//toolchains/native_tools:native_tools_toolchain.bzl", "native_tool_toolchain")

SUPPORTED_TARGETS = [
    struct(
        tuple = "linux_x86_64",
        triple = "x86_64-unknown-linux-musl",
        constrain = ["@platforms//os:linux", "@platforms//cpu:x86_64"],
    ),
    struct(
        tuple = "linux_aarch64",
        triple = "aarch64-unknown-linux-musl",
        constrain = ["@platforms//os:linux", "@platforms//cpu:aarch64"],
    ),
]

# Copied from bazel-contrib/rules_foreign_cc licensed under Apache-2.0
def _current_toolchain_impl(ctx):
    toolchain = ctx.toolchains[ctx.attr._toolchain]

    if toolchain.data.target:
        return [
            toolchain,
            platform_common.TemplateVariableInfo(toolchain.data.env),
            DefaultInfo(
                files = toolchain.data.target.files,
                runfiles = toolchain.data.target.default_runfiles,
            ),
        ]
    return [
        toolchain,
        platform_common.TemplateVariableInfo(toolchain.data.env),
        DefaultInfo(),
    ]

def current_toolchain(name):
    return rule(
        implementation = _current_toolchain_impl,
        attrs = {
            "_toolchain": attr.string(default = "//build/toolchain/toolchain-bundle:%s_toolchain" % name),
        },
        toolchains = [
            "//build/toolchain/toolchain-bundle:%s_toolchain" % name,
        ],
    )

def toolchain_for(name, config):
    native.toolchain_type(
        name = "%s_toolchain" % name,
    )

    config.current_toolchain_func(
        name = name,
    )

    for target in SUPPORTED_TARGETS:
        native.toolchain(
            name = "%s_%s_toolchain" % (name, target.tuple),
            exec_compatible_with = target.constrain,
            toolchain = ":%s_%s" % (name, target.tuple),
            toolchain_type = ":%s_toolchain" % name,
        )

        native_tool_toolchain(
            name = "%s_%s" % (name, target.tuple),
            env = {
                name.upper(): "$(execpath @toolchain-bundle-%s//:%s)" % (target.triple, config.target),
            },
            target = "@toolchain-bundle-%s//:%s" % (target.triple, config.target),
        )

current_qemu_img_toolchain = current_toolchain("qemu-img")
current_qemu_kvm_toolchain = current_toolchain("qemu-kvm")
current_make_toolchain = current_toolchain("make")
current_strace_toolchain = current_toolchain("strace")
current_nasm_toolchain = current_toolchain("nasm")
current_bison_toolchain = current_toolchain("bison")
current_flex_toolchain = current_toolchain("flex")
current_m4_toolchain = current_toolchain("m4")
current_bc_toolchain = current_toolchain("bc")
current_busybox_toolchain = current_toolchain("busybox")
current_diff_toolchain = current_toolchain("diff")
current_perl_toolchain = current_toolchain("perl")
current_iasl_toolchain = current_toolchain("iasl")
current_lz4_toolchain = current_toolchain("lz4")

TOOLCHAINS = {
    "qemu-img": struct(
        target = "bin/qemu-img",
        current_toolchain_func = current_qemu_img_toolchain,
    ),
    "qemu-kvm": struct(
        target = "qemu-kvm",
        current_toolchain_func = current_qemu_kvm_toolchain,
    ),
    "make": struct(
        target = "bin/make",
        current_toolchain_func = current_make_toolchain,
    ),
    "strace": struct(
        target = "bin/strace",
        current_toolchain_func = current_strace_toolchain,
    ),
    "nasm": struct(
        target = "bin/nasm",
        current_toolchain_func = current_nasm_toolchain,
    ),
    "bison": struct(
        target = "bison",
        current_toolchain_func = current_bison_toolchain,
    ),
    "flex": struct(
        target = "bin/flex",
        current_toolchain_func = current_flex_toolchain,
    ),
    "m4": struct(
        target = "bin/m4",
        current_toolchain_func = current_m4_toolchain,
    ),
    "bc": struct(
        target = "bin/bc",
        current_toolchain_func = current_bc_toolchain,
    ),
    "diff": struct(
        target = "bin/diff",
        current_toolchain_func = current_diff_toolchain,
    ),
    "iasl": struct(
        target = "bin/iasl",
        current_toolchain_func = current_iasl_toolchain,
    ),
    "busybox": struct(
        target = "busybox",
        current_toolchain_func = current_busybox_toolchain,
    ),
    "perl": struct(
        target = "perl",
        current_toolchain_func = current_perl_toolchain,
    ),
    "lz4": struct(
        target = "bin/lz4",
        current_toolchain_func = current_lz4_toolchain,
    ),
}

def build_toolchain_env(ctx, toolchains):
    toolchain_info = [ctx.toolchains[t] for t in toolchains]
    env = dict([(k, v) for t in toolchain_info for k, v in t.data.env.items()])
    env = env | {"TOOL_PATH": ":".join([t.data.target.files.to_list()[0].path.rsplit("/", 1)[0] for t in toolchain_info])}

    inputs = depset(transitive = [
        depset(transitive = [t.data.target.files, t.data.target.default_runfiles.files])
        for t in toolchain_info
    ])

    return env, inputs

TOOLCHAIN_ENV_SETUP = """
set -e

# Iterate over all environment variables and expand paths that are
# either external or bazel-out.
for name in $(env | cut -d= -f1); do
  val="${!name}"
  [[ "$val" != *external/* && "$val" != *bazel-out/* ]] && continue # Quick skip

  sep=' '; [[ $name == "TOOL_PATH" ]] && sep=':' # Set separator: : for PATH, space otherwise
  IFS=$sep read -r -a items <<< "$val"     # Split value into array using correct separator

  for i in "${!items[@]}"; do
    key="${items[i]%%=*}"; v="${items[i]#*=}" # Handle 'key=val' and standalone paths
    if [[ ( $v == external/* || $v == bazel-out/* ) && -e "$v" ]]; then
      [ "$key" = "$v" ] && items[i]=$(realpath -s "$v") || items[i]="$key=$(realpath -s "$v")"
    fi
  done
  export "$name=$(IFS=$sep; echo "${items[*]}")" # Re-export with correct separator
done

# Add our now expanded TOOL_PATH to PATH
PATH="$PATH:$TOOL_PATH"

"""
