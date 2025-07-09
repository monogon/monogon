#  Copyright 2020 The Monogon Project Authors.
#
#  SPDX-License-Identifier: Apache-2.0
#
#  Licensed under the Apache License, Version 2.0 (the "License");
#  you may not use this file except in compliance with the License.
#  You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
#  Unless required by applicable law or agreed to in writing, software
#  distributed under the License is distributed on an "AS IS" BASIS,
#  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#  See the License for the specific language governing permissions and
#  limitations under the License.

def _binary_tarball_impl(ctx):
    layer_spec = ctx.actions.declare_file(ctx.label.name + ".prototxt")
    executable = ctx.attr.executable[DefaultInfo].files_to_run.executable
    runfiles = ctx.attr.executable[DefaultInfo].default_runfiles
    files = []
    for file in runfiles.files.to_list():
        layer_path = file.short_path

        # Weird shenanigans with external repos
        if layer_path.startswith("../"):
            layer_path = "external/" + layer_path[3:]
        files.append(struct(
            path = layer_path,
            src = file.path,
        ))
    ctx.actions.write(layer_spec, proto.encode_text(struct(file = files)))

    layer_out = ctx.actions.declare_file(ctx.label.name + ".tar")
    ctx.actions.run(
        outputs = [layer_out],
        inputs = [layer_spec, executable] + runfiles.files.to_list(),
        tools = [ctx.executable._container_binary],
        executable = ctx.executable._container_binary,
        arguments = ["-out", layer_out.path, "-spec", layer_spec.path],
    )

    return [DefaultInfo(files = depset([layer_out]), runfiles = ctx.runfiles(files = [layer_out]))]

binary_tarball = rule(
    implementation = _binary_tarball_impl,
    doc = """
        Build a tarball from a binary given in `executable` and its runfiles. Everything will be put under
        /app with the same filesystem layout as if run under `bazel run`. So if your executable works under bazel run,
        it will work when packaged with this rule with the exception of runfile manifests, which this rule currently
        doesn't support.
    """,
    attrs = {
        "executable": attr.label(
            mandatory = True,
            executable = True,
            allow_single_file = True,
            cfg = "target",
        ),
        "_container_binary": attr.label(
            default = Label("//build/binary_tarball"),
            cfg = "exec",
            executable = True,
            allow_files = True,
        ),
    },
)
