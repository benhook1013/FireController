#!/usr/bin/env bash
# Generate with genuine predecessor source, never reverse-migrate a current DB.
set -euo pipefail
parent=383ce8c0bc4505dd89cf5cdec7fb2da1d7e556c8
root=$(git rev-parse --show-toplevel)
output=${1:?Supply a new absolute fixture destination}
case "$output" in /*) ;; *) echo "Fixture destination must be absolute" >&2; exit 1;; esac
[[ ! -e "$output" ]] || { echo "Fixture destination already exists" >&2; exit 1; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/parent"
git -C "$root" archive "$parent" | tar -x -C "$work/parent"
mkdir "$work/parent/tools/admin-parent-fixture"
cp "$root/tools/ci/admin-parent-fixture.go.txt" "$work/parent/tools/admin-parent-fixture/main.go"
mkdir -m 700 "$output"
(cd "$work/parent" && GOWORK=off go run ./tools/admin-parent-fixture "$work/source" "$output")
# The predecessor closes/checkpoints SQLite before preserving its source.
mv "$work/source" "$output/source"
printf '%s\n' "$parent" > "$output/parent-commit"
