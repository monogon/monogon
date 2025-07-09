load("//osbase/build/fsspec:def.bzl", "FSSpecInfo", "fsspec_core_impl")

def _erofs_image_impl(ctx):
    fs_name = ctx.label.name + ".img"
    fs_out = ctx.actions.declare_file(fs_name)

    fsspec_core_impl(ctx, ctx.executable._mkerofs, fs_out)

    return [DefaultInfo(files = depset([fs_out]))]

erofs_image = rule(
    implementation = _erofs_image_impl,
    doc = """
        Build an EROFS. All files specified in files and all specified symlinks will be contained.
        Executable files will have their permissions set to 0555, non-executable files will have
        their permissions set to 0444. All parent directories will be created with 0555 permissions.
    """,
    attrs = {
        "files": attr.string_keyed_label_dict(
            mandatory = True,
            allow_files = True,
            doc = """
                Dictionary of Labels to String, placing a given Label's output file in the EROFS at the location
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

        # Tools, implicit dependencies.
        "_mkerofs": attr.label(
            default = Label("//osbase/build/mkerofs"),
            executable = True,
            cfg = "exec",
        ),
    },
)
