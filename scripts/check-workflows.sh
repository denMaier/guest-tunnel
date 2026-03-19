#!/bin/sh
set -eu

mode="${1:---all}"

if ! command -v actionlint >/dev/null 2>&1; then
  cat >&2 <<'EOF'
actionlint is required but was not found on PATH.
Install it first, then rerun the workflow check.
EOF
  exit 127
fi

repo_root=$(git rev-parse --show-toplevel)

if [ "$mode" = "--staged" ]; then
  files=$(git diff --cached --name-only --diff-filter=ACM | grep '^\.github/workflows/.*\.ya\?ml$' || true)
else
  files=$(find .github/workflows -maxdepth 1 \( -name '*.yml' -o -name '*.yaml' \) -type f | sort)
fi

if [ -z "${files:-}" ]; then
  echo "No workflow files to check."
  exit 0
fi

echo "$files" | while IFS= read -r file; do
  [ -n "$file" ] || continue
  echo "Linting $file"
  actionlint "$file"
done

echo "Workflow checks passed."
