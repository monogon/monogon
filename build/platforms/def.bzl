def _multiplatform_transition_impl(_, attr):
    return [
        {"//command_line_option:platforms": str(platform)}
        for platform in attr.platforms
    ]

_multiplatform_transition = transition(
    implementation = _multiplatform_transition_impl,
    inputs = [],
    outputs = ["//command_line_option:platforms"],
)

def _multiplatform_transition_filegroup_impl(ctx):
    files = [src[DefaultInfo].files for src in ctx.attr.srcs]
    runfiles = ctx.runfiles().merge_all([src[DefaultInfo].default_runfiles for src in ctx.attr.srcs])
    return [DefaultInfo(
        files = depset(transitive = files),
        runfiles = runfiles,
    )]

multiplatform_transition_filegroup = rule(
    _multiplatform_transition_filegroup_impl,
    attrs = {
        "platforms": attr.label_list(
            providers = [platform_common.PlatformInfo],
            mandatory = True,
            doc = "The target platforms to transition the srcs.",
        ),
        "srcs": attr.label_list(
            cfg = _multiplatform_transition,
            mandatory = True,
            doc = "The input to be transitioned to the target platforms.",
        ),
    },
    doc = "Transitions the srcs to use the provided platforms. The filegroup will contain artifacts for all target platforms.",
)
