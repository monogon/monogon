load("@rules_go//go:def.bzl", "GoSource", "go_context")

# This implements the toolchain_library rule, which is used to generate a
# rules_go compatible go_library-style target which contains toolchain.go.in
# augmented with information about the Go SDK's toolchain used by Bazel.
#
# The library can then be used to build tools that call the `go` tool, for
# example to perform static analysis or dependency management.

def _toolchain_library_impl(ctx):
    go = go_context(ctx)

    out = go.declare_file(go, ext = ".go")
    ctx.actions.expand_template(
        template = ctx.file._template,
        output = out,
        substitutions = {
            "GOROOT": go.sdk.root_file.dirname,
            "GOTOOL": go.sdk.go.path,
        },
    )

    library = go.new_library(go)
    source = go.library_to_source(go, struct(
        srcs = [struct(files = [out])],
        deps = ctx.attr.deps,
    ), library, ctx.coverage_instrumented())

    # Hack: we want to inject runfiles into the generated GoSource, because
    # there's no other way to make rules_go pick up runfiles otherwise.
    runfiles = ctx.runfiles(
        [
            go.sdk.go,
            go.sdk.root_file,
        ],
        transitive_files = depset(transitive = [go.sdk.headers, go.sdk.srcs, go.sdk.libs, go.sdk.tools]),
    )
    source = {
        key: getattr(source, key)
        for key in dir(source)
        if key not in ["to_json", "to_proto"]
    }
    source["runfiles"] = runfiles
    source = GoSource(**source)

    return [
        library,
        source,
        OutputGroupInfo(
            go_generated_srcs = depset([out]),
        ),
    ]

toolchain_library = rule(
    implementation = _toolchain_library_impl,
    attrs = {
        "importpath": attr.string(
            mandatory = True,
        ),
        "deps": attr.label_list(),
        "_template": attr.label(
            allow_single_file = True,
            default = ":toolchain.go.in",
        ),
        "_go_context_data": attr.label(
            default = "@rules_go//:go_context_data",
        ),
    },
    toolchains = ["@rules_go//go:toolchain"],
)
