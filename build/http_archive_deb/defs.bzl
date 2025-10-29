load(
    "@bazel_tools//tools/build_defs/repo:cache.bzl",
    "CANONICAL_ID_DOC",
    "DEFAULT_CANONICAL_ID_ENV",
    "get_default_canonical_id",
)
load(
    "@bazel_tools//tools/build_defs/repo:utils.bzl",
    "get_auth",
    "update_attrs",
    "workspace_and_buildfile",
)

_http_archive_deb_attrs = {
    "url": attr.string(doc = "A URL to a deb file. See http_archive for more info."),
    "urls": attr.string_list(doc = "A list of URLs to a deb file. See http_archive for more info."),
    "integrity": attr.string(
        doc = """Expected checksum in Subresource Integrity format of the file downloaded.

This must match the checksum of the file downloaded. _It is a security risk
to omit the checksum as remote files can change._ At best omitting this
field will make your build non-hermetic. It is optional to make development
easier but this attribute should be set before shipping.""",
    ),
    "netrc": attr.string(
        doc = "Location of the .netrc file to use for authentication",
    ),
    "auth_patterns": attr.string_dict(
        doc = "See http_archive",
    ),
    "canonical_id": attr.string(
        doc = CANONICAL_ID_DOC,
    ),
    "build_file": attr.label(
        allow_single_file = True,
        doc =
            "The file to use as the BUILD file for this repository." +
            "This attribute is an absolute label (use '@//' for the main " +
            "repo). The file does not need to be named BUILD, but can " +
            "be (something like BUILD.new-repo-name may work well for " +
            "distinguishing it from the repository's actual BUILD files. " +
            "Either build_file or build_file_content can be specified, but " +
            "not both.",
    ),
    "build_file_content": attr.string(
        doc =
            "The content for the BUILD file for this repository. " +
            "Either build_file or build_file_content can be specified, but " +
            "not both.",
    ),
    "workspace_file": attr.label(
        doc = "No-op attribute; do not use.",
    ),
    "workspace_file_content": attr.string(
        doc = "No-op attribute; do not use.",
    ),
}

def _get_source_urls(ctx):
    """Returns source urls provided via the url, urls attributes.

    Also checks that at least one url is provided."""
    if not ctx.attr.url and not ctx.attr.urls:
        fail("At least one of url and urls must be provided")

    source_urls = []
    if ctx.attr.urls:
        source_urls = ctx.attr.urls
    if ctx.attr.url:
        source_urls = [ctx.attr.url] + source_urls
    return source_urls

def _http_archive_deb_impl(ctx):
    """Implementation of the http_archive_deb rule."""
    if ctx.attr.build_file and ctx.attr.build_file_content:
        fail("Only one of build_file and build_file_content can be provided.")

    source_urls = _get_source_urls(ctx)
    download_info = ctx.download_and_extract(
        source_urls,
        output = "debian-package",
        type = ".deb",
        canonical_id = ctx.attr.canonical_id or get_default_canonical_id(ctx, source_urls),
        auth = get_auth(ctx, source_urls),
        integrity = ctx.attr.integrity,
    )
    files = ctx.path("debian-package").readdir()

    data_archive = None
    control_archive = None
    has_marker = False
    for f in files:
        if f.basename.startswith("data.tar."):
            data_archive = f.basename
        elif f.basename.startswith("control.tar."):
            control_archive = f.basename
        elif f.basename == "debian-binary":
            has_marker = True

    if not has_marker:
        fail("deb package does not contain a debian-binary marker file, check the file.")
    if not data_archive:
        fail("Failed to find data.tar.* archive in .deb contents.")
    if not control_archive:
        fail("Failed to find control.tar.* archive in .deb contents.")

    ctx.extract(
        archive = "debian-package/" + data_archive,
    )
    ctx.extract(
        archive = "debian-package/" + control_archive,
        output = "debian",
    )
    ctx.delete("debian-package")
    workspace_and_buildfile(ctx)

    if ctx.attr.integrity:
        return ctx.repo_metadata(reproducible = True)

    return ctx.repo_metadata(attrs_for_reproducibility = update_attrs(ctx.attr, _http_archive_deb_attrs.keys(), {"integrity": download_info.integrity}))

http_archive_deb = repository_rule(
    implementation = _http_archive_deb_impl,
    attrs = _http_archive_deb_attrs,
    environ = [DEFAULT_CANONICAL_ID_ENV],
    doc = """http_archive for Debian packages. Extracts all contents into a
    repository, control files are extracted into a `debian` subfolder.""",
)
