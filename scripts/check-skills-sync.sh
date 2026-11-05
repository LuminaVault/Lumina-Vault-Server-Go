#!/usr/bin/env bash
# internal/skills/builtin must be byte-identical with Swift's Resources/Skills while
# both servers run: a skill that differs behaves differently depending on which
# server took the request. Delete this script and its CI step when Swift is archived.
#
# Default: compares with the Swift image pinned in parity/SWIFT_IMAGE, the one
# production and the parity job run, so it needs that image and registry access.
# Offline: SWIFT_SKILLS_DIR=../LuminaVaultServer/Sources/App/Resources/Skills
# compares with a checkout, which may hold ignored files the image doesn't.
#
# Compares file contents only, not modes.
set -euo pipefail

cd "$(dirname "$0")/.."
go_tree=internal/skills/builtin

swift_tree="${SWIFT_SKILLS_DIR:-}"
if [ -z "$swift_tree" ]; then
  image="${PARITY_SWIFT_IMAGE:-$(cat parity/SWIFT_IMAGE)}"
  # SwiftPM names the bundle <package>_<target>; the package is still called
  # ObsidianClaudeBrainServer. The Dockerfile copies it to /app.
  bundle=/app/ObsidianClaudeBrainServer_App.resources/Skills
  tmp="$(mktemp -d)"
  container="$(docker create "$image")"
  trap 'docker rm -f "$container" >/dev/null; rm -rf "$tmp"' EXIT
  docker cp "$container:$bundle" "$tmp/Skills"
  swift_tree="$tmp/Skills"
fi

if ! diff -r "$go_tree" "$swift_tree"; then
  echo "::error::internal/skills/builtin differs from Swift's Resources/Skills. Copy the change across: both servers must serve the same skills." >&2
  exit 1
fi
echo "internal/skills/builtin matches Swift ($(find "$go_tree" -type f | wc -l | tr -d ' ') files)."
