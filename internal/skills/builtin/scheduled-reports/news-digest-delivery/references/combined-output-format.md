## Combined Output Format

The combined output should include a clear header indicating it's a combined digest, followed by sections for each script's output.

### Example

```markdown
# News Digest - Combined Report

## Go News Digest
### Generated on: 2026-05-09 17:04:22

#### Go 1.24 Released
- Date: 2024-01-15
- URL: https://go.dev/blog/go1.24

---

## Swift News Digest
### Generated: 2026-05-09 17:04:24

### === Swift Forums ===

**Swift Forums**: Implementing a HeartbeatActor. Should I consider executorPreference
https://forums.swift.org/t/implementing-a-heartbeatactor-should-i-consider-executorpreference/86616
Published: Unknown date

...
```

### Key Elements

- Header with title and timestamp
- Clear separation between different script outputs (use `---` or similar)
- Preserve original formatting from each script where possible
- Keep the combined message under Discord's 2000-character limit; if too long, split into multiple messages