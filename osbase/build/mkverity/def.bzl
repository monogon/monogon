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
    ctx.actions.run(
        mnemonic = "GenVerityImage",
        progress_message = "Generating a dm-verity image: {}".format(image.short_path),
        inputs = [ctx.file.source],
        outputs = [
            image,
            table,
        ],
        executable = ctx.file._mkverity,
        arguments = [
            "-input=" + ctx.file.source.path,
            "-output=" + image.path,
            "-table=" + table.path,
            "-data_alias=" + ctx.attr.rootfs_partlabel,
            "-hash_alias=" + ctx.attr.rootfs_partlabel,
        ],
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
