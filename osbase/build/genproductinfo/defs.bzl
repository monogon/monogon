# Copyright The Monogon Project Authors.
# SPDX-License-Identifier: Apache-2.0

def _product_info_impl(ctx):
    product_info_file = ctx.actions.declare_file(ctx.label.name + ".json")
    args = ctx.actions.args()
    args.add("-status_file", ctx.info_file)
    args.add("-product_info_file", product_info_file)
    args.add("-os_release_file", ctx.outputs.out_os_release)
    args.add("-stamp_var", ctx.attr.stamp_var)
    args.add("-name", ctx.attr.os_name)
    args.add("-id", ctx.attr.os_id)
    args.add("-architecture", ctx.attr.architecture)
    args.add("-build_flags", "-".join(ctx.attr.build_flags))
    args.add("-platform_os", ctx.attr.platform_os)
    args.add_all(ctx.attr.components, before_each = "-component")
    ctx.actions.run(
        mnemonic = "GenProductInfo",
        progress_message = "Generating product info",
        inputs = [ctx.info_file],
        outputs = [product_info_file, ctx.outputs.out_os_release],
        executable = ctx.executable._genproductinfo,
        arguments = [args],
    )
    return [DefaultInfo(files = depset([product_info_file]))]

_product_info = rule(
    implementation = _product_info_impl,
    attrs = {
        "os_name": attr.string(mandatory = True),
        "os_id": attr.string(mandatory = True),
        "stamp_var": attr.string(mandatory = True),
        "platform_os": attr.string(default = "unknown"),
        "components": attr.string_list(),
        "out_os_release": attr.output(
            mandatory = True,
            doc = """Output, contains the os-release file.""",
        ),
        "architecture": attr.string(mandatory = True),
        "build_flags": attr.string_list(),
        "_genproductinfo": attr.label(
            default = ":genproductinfo",
            cfg = "exec",
            executable = True,
            allow_files = True,
        ),
    },
)

def _product_info_macro_impl(**kwargs):
    _product_info(
        architecture = select({
            "@platforms//cpu:x86_64": "x86_64",
            "@platforms//cpu:aarch64": "aarch64",
        }),
        build_flags = select({
            Label(":flag_debug"): ["debug"],
            "//conditions:default": [],
        }) + select({
            Label(":flag_race"): ["race"],
            "//conditions:default": [],
        }),
        **kwargs
    )

product_info = macro(
    inherit_attrs = _product_info,
    attrs = {"architecture": None, "build_flags": None},
    implementation = _product_info_macro_impl,
)
