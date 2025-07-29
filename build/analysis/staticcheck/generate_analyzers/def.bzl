def _generate_analyzers_impl(ctx):
    analyzers_file = ctx.actions.declare_file("analyzers.bzl")

    args = ctx.actions.args()
    args.add("-out")
    args.add(analyzers_file)

    ctx.actions.run(
        outputs = [analyzers_file],
        executable = ctx.executable._generate_analyzers,
        arguments = [args],
    )

    return [DefaultInfo(files = depset([analyzers_file]), runfiles = ctx.runfiles(files = [analyzers_file]))]

generate_analyzers = rule(
    implementation = _generate_analyzers_impl,
    attrs = {
        "_generate_analyzers": attr.label(
            default = ":generate_analyzers",
            executable = True,
            cfg = "exec",
        ),
    },
)
