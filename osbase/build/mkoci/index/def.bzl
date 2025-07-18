def _multi_platform_transition_impl(_settings, attr):
    return {
        str(platform): {"//command_line_option:platforms": str(platform)}
        for platform in attr.platforms
    }

_multi_platform_transition = transition(
    implementation = _multi_platform_transition_impl,
    inputs = [],
    outputs = ["//command_line_option:platforms"],
)

def _platform_independent_transition_impl(_settings, _attr):
    return {"//command_line_option:platforms": "//build/platforms:all"}

_platform_independent_transition = transition(
    implementation = _platform_independent_transition_impl,
    inputs = [],
    outputs = ["//command_line_option:platforms"],
)

def _oci_index_impl(ctx):
    inputs = []
    transitive_runfiles = []
    args = ctx.actions.args()

    for platform in ctx.attr.platforms:
        # Use ctx.split_attr because for ctx.attr, the order is unspecified.
        image = ctx.split_attr.src[str(platform.label)]
        files = image[DefaultInfo].files.to_list()
        if len(files) != 1:
            fail("image does not have exactly one directory: {}", files)
        file = files[0]
        if not file.is_directory:
            fail("image is not a directory: {}", file)
        inputs.append(file)
        args.add("-image", file.path)
        transitive_runfiles.append(image[DefaultInfo].default_runfiles)

    output = ctx.actions.declare_directory(ctx.label.name)
    args.add("-out", output.path)

    ctx.actions.run(
        mnemonic = "MkOCIIndex",
        executable = ctx.executable._mkoci_index,
        arguments = [args],
        inputs = inputs,
        outputs = [output],
    )

    # The inputs are referenced by symlinks.
    runfiles = ctx.runfiles(files = inputs)

    # Also merge the runfiles of the input images, in case they already use symlinks.
    runfiles = runfiles.merge_all(transitive_runfiles)
    return [DefaultInfo(
        files = depset([output]),
        runfiles = runfiles,
    )]

oci_index = rule(
    cfg = _platform_independent_transition,
    implementation = _oci_index_impl,
    doc = """
        Build a multi-platform OCI index. This rule works with arbitrary image
        types, as it does not attempt to parse the image config.

        Since the index is not for a specific platform, it is transitioned to
        the platform-independent platform.
    """,
    attrs = {
        "src": attr.label(
            doc = """
                OCI image, stored in an OCI layout directory. The descriptor in
                the index.json should include platform information, as the
                descriptor is copied as is into the generated index.
            """,
            mandatory = True,
            allow_files = True,
            cfg = _multi_platform_transition,
        ),
        "platforms": attr.label_list(
            doc = """
                A list of platforms for which the OCI image is built and added to the index.
            """,
            mandatory = True,
            providers = [platform_common.PlatformInfo],
        ),

        # Tool
        "_mkoci_index": attr.label(
            default = Label("//osbase/build/mkoci/index:mkoci_index"),
            executable = True,
            cfg = "exec",
        ),
    },
)
