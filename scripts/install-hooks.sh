#!/bin/sh
set -eu

repo_root=$(git rev-parse --show-toplevel)
git config core.hooksPath "$repo_root/.githooks"
echo "Installed git hooks from $repo_root/.githooks"
