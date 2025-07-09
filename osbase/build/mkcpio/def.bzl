load("//osbase/build/fsspec:def.bzl", "FSSpecInfo", "fsspec_core_impl")

def _node_initramfs_impl(ctx):
    initramfs_name = ctx.label.name + ".cpio.zst"
    initramfs = ctx.actions.declare_file(initramfs_name)

    fsspec_core_impl(ctx, ctx.executable._mkcpio, initramfs)

    return [DefaultInfo(runfiles = ctx.runfiles(files = [initramfs]), files = depset([initramfs]))]

node_initramfs = rule(
    implementation = _node_initramfs_impl,
    doc = """
        Build a node initramfs. The initramfs will contain a basic /dev directory and all the files specified by the
        `files` attribute. Executable files will have their permissions set to 0755, non-executable files will have
        their permissions set to 0444. All parent directories will be created with 0755 permissions.
    """,
    attrs = {
        "files": attr.string_keyed_label_dict(
            mandatory = True,
            allow_files = True,
            doc = """
                Dictionary of Labels to String, placing a given Label's output file in the initramfs at the location
                specified by the String value. The specified labels must only have a single output.
            """,
        ),
        "symlinks": attr.string_dict(
            default = {},
            doc = """
                Symbolic links to create. Similar format as in `files`, so the key is the location of the
                symlink itself and the value of it is target of the symlink. Only raw strings are allowed as targets,
                labels are not permitted. Include the file using `files`, then symlink to its location.
            """,
        ),
        "fsspecs": attr.label_list(
            default = [],
            doc = """
                List of file system specs (osbase.build.fsspec.FSSpec) to also include in the resulting image.
                These will be merged with all other given attributes.
            """,
            providers = [FSSpecInfo],
            allow_files = True,
        ),

        # Tool
        "_mkcpio": attr.label(
            default = Label("//osbase/build/mkcpio"),
            executable = True,
            cfg = "exec",
        ),
    },
)
