# Swift test waivers

`scripts/swift-test-coverage.py` (`make swift-test-coverage`) requires every Swift test file
to be named in a Go test or a parity scenario. A file that shouldn't be ported goes in the
table below instead. The script parses this file, so keep the table's shape.

Each row needs all three cells:

- **Swift test file**: the path under the Swift repo's `Tests/AppTests`, ending in `.swift`.
- **Reason**: why no port is needed. A cell can't contain `|`.
- **Owner sign-off**: the owner's name and the date, as `name YYYY-MM-DD`. Only the owner
  fills this in. An agent proposes a waiver in its PR body and leaves the row out.

A row with a missing cell fails the script. For example, this row (in a code fence, so it
isn't read) has the right shape:

```
| `Support/Example.swift` | Why no port is needed. | name 2026-01-01 |
```

## Waivers

| Swift test file | Reason | Owner sign-off |
|---|---|---|
