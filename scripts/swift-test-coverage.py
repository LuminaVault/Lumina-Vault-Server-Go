#!/usr/bin/env python3
"""Check that every Swift test file has been accounted for (Go migration gate P7).

A Swift test file counts as accounted for when one of these holds:
  - a Go test file (*_test.go) names it,
  - a parity scenario (parity/scenarios*/) names it,
  - docs/migration/test-waivers.md waives it, with a reason and the owner's sign-off.

Those three are the only inputs besides the Swift tests themselves. Plans are never
read: a plan naming a test doesn't port it.

"Names it" means the file's base name appears as a whole word, as in
"Ports Tests/AppTests/Kanban/RankStringTests.swift" or "RankStringTests". A word
followed by "/" is a directory, so that citation doesn't also name AppTests.swift.
Citations aren't checked for meaning: a comment saying a test was not ported would
still count, so don't write one.

A Swift file with no test in it (a helper) needs no citation.

    python3 scripts/swift-test-coverage.py --swift-tests PATH   # or SWIFT_TESTS=PATH
    python3 scripts/swift-test-coverage.py --count              # summary only, exit 0

PATH is the Swift repo's Tests/AppTests, at the commit in parity/SWIFT_IMAGE.

Exit status: 0 nothing uncited, 1 uncited files or a malformed waiver, 2 could not run.
"""
import argparse
import os
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
WAIVERS = ROOT / "docs/migration/test-waivers.md"

# Swift Testing's @Test, and the XCTest-style names. The plan's accounting counts both.
TEST = re.compile(r"^\s*(?:@\w+(?:\([^)]*\))?\s+)*(?:@Test\b|func test\w*\s*\()", re.M)
COMMENT = re.compile(r"^\s*//.*$", re.M)
# A whole word not followed by "/". Without the \b the match could back off a letter
# and "AppTests/" would yield "AppTest".
WORD = re.compile(r"\b\w+\b(?!/)")
SIGN_OFF = re.compile(r"^\S.*\s\d{4}-\d{2}-\d{2}$")


def swift_test_files(root):
    """Maps each Swift file that contains tests, relative to root, to its test count."""
    tests = {}
    for path in root.rglob("*.swift"):
        count = len(TEST.findall(COMMENT.sub("", path.read_text(errors="replace"))))
        if count:
            tests[path.relative_to(root).as_posix()] = count
    return tests


def citing_files():
    """Yields the Go test files and parity scenario files, and nothing else."""
    for dirpath, dirnames, filenames in os.walk(ROOT):
        # What the go tool ignores. The dot also keeps other worktrees out of a run from
        # the main checkout: .worktrees/ holds their test files.
        dirnames[:] = [d for d in dirnames if not d.startswith((".", "_")) and d not in ("testdata", "vendor")]
        for name in filenames:
            if name.endswith("_test.go"):
                yield Path(dirpath) / name
    for scenarios in (ROOT / "parity").glob("scenarios*"):
        for path in scenarios.rglob("*"):
            if path.is_file():
                yield path


def cited_words():
    words = set()
    for path in citing_files():
        words.update(WORD.findall(path.read_text(errors="replace")))
    return words


def parse_waivers(text):
    """Returns (waived paths, problems). Table rows only; code fences are documentation."""
    waived, problems = set(), []
    fenced = False
    for number, line in enumerate(text.splitlines(), 1):
        stripped = line.strip()
        if stripped.startswith("```"):
            fenced = not fenced
        if fenced or not stripped.startswith("|"):
            continue
        cells = [cell.strip() for cell in stripped.strip("|").split("|")]
        if cells[0] == "Swift test file" or set(cells[0]) <= set("-: "):
            continue
        if len(cells) != 3:
            problems.append(f"line {number}: want 3 cells, got {len(cells)}")
            continue
        swift_file, reason, sign_off = cells[0].strip("`"), cells[1], cells[2]
        if not swift_file.endswith(".swift"):
            problems.append(f"line {number}: {swift_file}: want a path ending in .swift")
        elif not reason:
            problems.append(f"line {number}: {swift_file}: no reason")
        elif not SIGN_OFF.match(sign_off):
            problems.append(f"line {number}: {swift_file}: sign-off must be a name and a YYYY-MM-DD date")
        else:
            waived.add(swift_file)
    return waived, problems


def main():
    parser = argparse.ArgumentParser(description="Report Swift test files with no Go test, parity scenario or waiver.")
    parser.add_argument("--swift-tests", default=os.environ.get("SWIFT_TESTS"), help="Swift Tests/AppTests (default: $SWIFT_TESTS)")
    parser.add_argument("--count", action="store_true", help="print the summary only and exit 0, for a report that must not fail")
    args = parser.parse_args()

    if not args.swift_tests:
        print("swift-test-coverage: set --swift-tests or SWIFT_TESTS to the Swift repo's Tests/AppTests", file=sys.stderr)
        return 2
    swift_root = Path(args.swift_tests)
    if not swift_root.is_dir():
        print(f"swift-test-coverage: {swift_root} is not a directory", file=sys.stderr)
        return 2
    tests = swift_test_files(swift_root)
    # A wrong path must not read as "nothing uncited".
    if not tests:
        print(f"swift-test-coverage: no Swift tests under {swift_root}", file=sys.stderr)
        return 2
    if not WAIVERS.is_file():
        print(f"swift-test-coverage: {WAIVERS} is missing", file=sys.stderr)
        return 2

    words = cited_words()
    waived, problems = parse_waivers(WAIVERS.read_text())
    cited = {f for f in tests if Path(f).stem in words}
    uncited = sorted(set(tests) - cited - waived)
    summary = (
        f"{len(uncited)} of {len(tests)} Swift test files uncited ({sum(tests[f] for f in uncited)} of {sum(tests.values())} tests); "
        f"{len(cited)} cited, {len((waived & set(tests)) - cited)} waived"
    )
    if args.count:
        print(summary)
        return 0

    for path in uncited:
        print(f"{path} ({tests[path]})")
    for path in sorted(waived - set(tests)):
        print(f"warning: waiver for {path}, which has no tests under {swift_root}", file=sys.stderr)
    for problem in problems:
        print(f"malformed waiver: {problem}", file=sys.stderr)
    print(summary)
    return 1 if uncited or problems else 0


if __name__ == "__main__":
    sys.exit(main())
