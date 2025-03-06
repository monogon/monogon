#!/usr/bin/bash
function get_workspace_root() {
  workspace_dir="${PWD}"
  while [[ "${workspace_dir}" != / ]]; do
    if [[ -e "${workspace_dir}/WORKSPACE" || -e "${workspace_dir}/WORKSPACE.bazel" || -e "${workspace_dir}/MODULE.bazel" ]]; then
      readonly workspace_dir
      return
    fi
    workspace_dir="$(dirname "${workspace_dir}")"
  done
  readonly workspace_dir=""
}

get_workspace_root
readonly wrapper="${workspace_dir}/tools/bazel"
if [ -f "${wrapper}" ]; then
  exec -a "$0" "${wrapper}" "$@"
fi
exec -a "$0" "${BAZEL_REAL}" "$@"
