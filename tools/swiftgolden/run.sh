#!/bin/sh
# Regenerates internal/chunkindex/testdata/swift_golden.json by running Swift's
# Indexing sources, unmodified apart from dropping `import Crypto`, on Linux.
#
#   tools/swiftgolden/run.sh <swift-repo> <linux-swift-image> <work-dir> [cases.json]
#
# <swift-repo> is LuminaVaultServer at the commit production runs. The image only
# needs a Swift 6.3 toolchain. Without cases.json the checked-in generator runs.
# With one (a large fuzz set) the output stays in <work-dir> and nothing in the
# repo changes: point LUMINA_SWIFT_GOLDEN at <work-dir>/golden.json and run the
# chunkindex tests.
set -eu
repo=$1; image=$2; work=$3; custom=${4:-}
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$work/src"
for f in ChunkIDs MarkdownChunker MarkdownHeadings Wikilinks WikilinkResolver; do
	sed 's/^import Crypto$//' "$repo/Sources/App/Indexing/$f.swift" >"$work/src/$f.swift"
done
cp "$here/shim_sha256.swift" "$work/src/"
cp "$here/harness.swift" "$work/src/main.swift"
if [ -n "$custom" ]; then cp "$custom" "$work/cases.json"; else python3 "$here/gen_cases.py" >"$work/cases.json"; fi
docker run --rm -v "$work:/work" -w /work/src --entrypoint /bin/sh "$image" -c \
	'swiftc -O -module-name swiftgolden *.swift -o /work/harness && /work/harness /work/cases.json /work/out.json'
python3 - "$work/cases.json" "$work/out.json" "$work/golden.json" <<'PY'
import json, sys
cases, outs = json.load(open(sys.argv[1])), json.load(open(sys.argv[2]))
assert len(cases) == len(outs)
json.dump([{"case": c, "want": o} for c, o in zip(cases, outs)], open(sys.argv[3], "w"), ensure_ascii=True, separators=(",", ":"), sort_keys=True)
PY
if [ -z "$custom" ]; then cp "$work/golden.json" "$here/../../internal/chunkindex/testdata/swift_golden.json"; fi
