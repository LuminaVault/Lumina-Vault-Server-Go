#!/usr/bin/env bash
# HER-229 — regenerate the LuminaVaultCollection Bruno collection from
# api/openapi.yaml (HER-224; moved from LuminaVaultServer, migration D6). Run on demand; not part of
# `swift build`.
#
# Requires Node.js + Bruno CLI (`npm i -g @usebruno/cli`).
# Reference: https://docs.usebruno.com/converters/openapi-to-bruno
#
# Behaviour:
#   - Reads $REPO_ROOT/api/openapi.yaml.
#   - Writes into $LUMINAVAULT_COLLECTION_PATH; when unset, the sibling
#     ../LuminaVaultCollection beside this repo, else the historical
#     ~/Projects/ObsidianClaudeBrain/LuminaVaultCollection.
#   - `bru import openapi` overwrites generated request files; manual
#     additions and the environments/ folder are preserved (any path
#     not matching a generated operation name is left alone).
#
# Workflow:
#   1. Edit api/openapi.yaml.
#   2. make bruno-regen
#   3. cd $LUMINAVAULT_COLLECTION_PATH && git diff
#   4. Commit + push both repos.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
SPEC="${REPO_ROOT}/api/openapi.yaml"
# The collection is normally checked out beside this repo, so look there
# before falling back to the historical location. An explicit
# LUMINAVAULT_COLLECTION_PATH always wins.
if [ -n "${LUMINAVAULT_COLLECTION_PATH:-}" ]; then
  TARGET="${LUMINAVAULT_COLLECTION_PATH}"
elif [ -d "$(dirname "${REPO_ROOT}")/LuminaVaultCollection" ]; then
  TARGET="$(dirname "${REPO_ROOT}")/LuminaVaultCollection"
else
  TARGET="${HOME}/Projects/ObsidianClaudeBrain/LuminaVaultCollection"
fi

if [ ! -f "${SPEC}" ]; then
  echo "error: OpenAPI spec not found at ${SPEC}" >&2
  exit 1
fi

if [ ! -d "${TARGET}" ]; then
  echo "error: LuminaVaultCollection not found at ${TARGET}" >&2
  echo "hint: clone git@github.com:LuminaVault/LuminaVaultCollection.git there" >&2
  echo "      or set LUMINAVAULT_COLLECTION_PATH to its absolute path" >&2
  exit 1
fi

if ! command -v bru >/dev/null 2>&1; then
  echo "error: bru CLI not installed" >&2
  echo "hint: npm install -g @usebruno/cli" >&2
  exit 1
fi

DEST="${TARGET}/LuminaVaultServer"

echo "→ regenerating Bruno collection"
echo "    spec:   ${SPEC}"
echo "    target: ${DEST}"

# bru CLI v3 (3.0.x) refuses to import when the --output collection dir already
# exists and is non-empty ("Output directory is not empty"), which broke the
# old in-place `--output "${TARGET}"` workflow. Work around it: import into a
# fresh temp dir, then rsync the generated request folders back into the real
# collection. The hand-maintained environments/ folder is excluded so its
# baseUrl/token vars survive regeneration.
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

# --collection-format is required from bru CLI v4: the default flipped to
# `opencollection`, which emits .yml instead of .bru. Without this flag a
# regen deletes every .bru in the collection and replaces it with a parallel
# set of .yml — a 400-file diff that looks like a catastrophe and forces
# everyone's Bruno app onto v4. Ask for the format the collection is actually
# in. (v3 has no such flag; see the guard below, which covers both.)
BRU_FORMAT_ARGS=()
if bru import openapi --help 2>/dev/null | grep -q -- '--collection-format'; then
  BRU_FORMAT_ARGS=(--collection-format bru)
fi

bru import openapi \
  --source "${SPEC}" \
  --output "${TMP_DIR}" \
  --collection-name "LuminaVaultServer" \
  --group-by tags \
  "${BRU_FORMAT_ARGS[@]+"${BRU_FORMAT_ARGS[@]}"}"

GENERATED="${TMP_DIR}/LuminaVaultServer"
if [ ! -d "${GENERATED}" ]; then
  echo "error: bru produced no LuminaVaultServer collection under ${TMP_DIR}" >&2
  exit 1
fi

# The rsync below runs with --delete against a checked-in collection, so a
# generator that silently changes output format would wipe it. Refuse rather
# than sync: a regen that produces no .bru files has not produced this
# collection, whatever else it produced.
GENERATED_BRU_COUNT="$(find "${GENERATED}" -type f -name '*.bru' | wc -l | tr -d ' ')"
if [ "${GENERATED_BRU_COUNT}" -eq 0 ]; then
  echo "error: bru produced no .bru files — refusing to sync over ${DEST}" >&2
  echo "hint: bru CLI $(bru --version 2>/dev/null | head -1) may have changed its default" >&2
  echo "      output format. Check \`bru import openapi --help\` for a format flag." >&2
  find "${GENERATED}" -type f | sed 's|^|      generated: |' | head -5 >&2
  exit 1
fi

mkdir -p "${DEST}"
# --delete prunes operation folders dropped from the spec; --exclude keeps the
# manual environments/ folder untouched (bru would otherwise overwrite it).
rsync -a --delete --exclude 'environments' "${GENERATED}/" "${DEST}/"

# Bruno may emit a space after an empty optional query value. Keep generated
# requests deterministic and compatible with git diff --check on every host.
find "${DEST}" -type f -name '*.bru' -exec perl -pi -e 's/[ \t]+$//' {} +

echo
echo "✓ regenerated Bruno collection at ${DEST}"
echo "  preserved: ${DEST}/environments"
echo "  next: cd ${TARGET} && git diff && git commit + push"
