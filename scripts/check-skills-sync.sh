#!/usr/bin/env bash
# internal/bundled must be byte-identical with Swift's bundled resources while both
# servers run: a skill that differs behaves differently depending on which server
# took the request. Delete this script and its CI step when Swift is archived.
#
#   internal/bundled/skills        Sources/App/Resources/Skills
#   internal/bundled/hermesskills  Sources/App/Resources/HermesSkills
#
# Default: compares with the Swift image pinned in parity/SWIFT_IMAGE, the one
# production and the parity job run, so it needs that image and registry access.
# Offline: SWIFT_RESOURCES_DIR=../LuminaVaultServer/Sources/App/Resources compares
# with a checkout, which may hold ignored files the image doesn't (.DS_Store, an
# untracked skill). SWIFT_SKILLS_DIR, the old single-tree override, still works
# and checks Skills only.
#
# Compares file contents only, not modes.
set -euo pipefail

cd "$(dirname "$0")/.."

# go_tree:swift_name pairs.
trees=(internal/bundled/skills:Skills internal/bundled/hermesskills:HermesSkills)

swift_root="${SWIFT_RESOURCES_DIR:-}"
if [ -n "${SWIFT_SKILLS_DIR:-}" ]; then
  swift_root="$(mktemp -d)"
  trap 'rm -rf "$swift_root"' EXIT
  ln -s "$(cd "$SWIFT_SKILLS_DIR" && pwd)" "$swift_root/Skills"
  trees=(internal/bundled/skills:Skills)
elif [ -z "$swift_root" ]; then
  image="${PARITY_SWIFT_IMAGE:-$(cat parity/SWIFT_IMAGE)}"
  # SwiftPM names the bundle <package>_<target>; the package is still called
  # ObsidianClaudeBrainServer. The Dockerfile copies it to /app.
  bundle=/app/ObsidianClaudeBrainServer_App.resources
  swift_root="$(mktemp -d)"
  container="$(docker create "$image")"
  trap 'docker rm -f "$container" >/dev/null; rm -rf "$swift_root"' EXIT
  for pair in "${trees[@]}"; do
    name="${pair#*:}"
    docker cp "$container:$bundle/$name" "$swift_root/$name"
  done
fi

status=0
for pair in "${trees[@]}"; do
  go_tree="${pair%%:*}"
  name="${pair#*:}"
  if ! diff -r "$go_tree" "$swift_root/$name/"; then
    echo "::error::$go_tree differs from Swift's Resources/$name. Copy the change across: both servers must serve the same skills." >&2
    status=1
    continue
  fi
  echo "$go_tree matches Swift's Resources/$name ($(find "$go_tree" -type f | wc -l | tr -d ' ') files)."
done
exit "$status"
