# VerityInfo is emitted by verity_image, and contains a file enclosing a
# singular dm-verity target table.
VerityInfo = provider(
    "Information necessary to mount a single dm-verity target.",
    fields = {
        "table": "A file containing the dm-verity target table. See: https://www.kernel.org/doc/html/latest/admin-guide/device-mapper/verity.html",
    },
)

def _verity_image_impl(ctx):
    """
    Create a new file containing the source image data together with the Verity
    metadata appended to it, and provide an associated DeviceMapper Verity target
    table in a separate file, through VerityInfo provider.
    """

    # Run mkverity.
    image = ctx.actions.declare_file(ctx.attr.name + ".img")
    table = ctx.actions.declare_file(ctx.attr.name + ".dmt")
    inputs = [ctx.file.source]
    args = ctx.actions.args()
    args.add("-input", ctx.file.source)
    args.add("-output", image)
    if ctx.file.salt:
        args.add("-salt", ctx.file.salt)
        inputs.append(ctx.file.salt)
    args.add("-table", table)
    args.add("-data_alias", ctx.attr.rootfs_partlabel)
    args.add("-hash_alias", ctx.attr.rootfs_partlabel)
    ctx.actions.run(
        mnemonic = "GenVerityImage",
        progress_message = "Generating a dm-verity image: {}".format(image.short_path),
        inputs = inputs,
        outputs = [image, table],
        executable = ctx.file._mkverity,
        arguments = [args],
    )

    return [
        DefaultInfo(
            files = depset([image]),
            runfiles = ctx.runfiles(files = [image]),
        ),
        VerityInfo(
            table = table,
        ),
    ]

verity_image = rule(
    implementation = _verity_image_impl,
    doc = """
      Build a dm-verity target image by appending Verity metadata to the source
      image. A corresponding dm-verity target table will be made available
      through VerityInfo provider.
  """,
    attrs = {
        "source": attr.label(
            doc = "A source image.",
            allow_single_file = True,
            mandatory = True,
        ),
        "salt": attr.label(
            doc = """
                A file which will be hashed to generate the salt.
                This should be a small file which is different for each
                released image, but which only changes when the source also
                changes. The product info file is a good choice for this.
            """,
            allow_single_file = True,
        ),
        "rootfs_partlabel": attr.string(
            doc = "GPT partition label of the rootfs to be used with dm-mod.create.",
            default = "PARTLABEL=METROPOLIS-SYSTEM-X",
        ),
        "_mkverity": attr.label(
            doc = "The mkverity executable needed to generate the image.",
            default = "//osbase/build/mkverity",
            allow_single_file = True,
            executable = True,
            cfg = "exec",
        ),
    },
)
