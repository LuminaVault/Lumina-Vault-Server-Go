# Encoding and File I/O Best Practices

When appending YouTube video titles to the markdown file, special characters in titles can cause issues if not handled properly. This is especially important when using shell commands like `echo` which interpret certain characters.

## Common Problematic Characters

- **Dollar signs (`$`)** - Interpreted as shell variable start
- **Backslashes (`\`)** - Escape character
- **Newlines** - Can break line structure
- **Quotes (`"`, `'`)** - Can interfere with shell quoting

## The Pitfall: Using `echo` Directly

**Problematic approach:**
```bash
echo "2026-05-05 - [$HIMS Hired Amazon's AI Engineering Chief as CTO. Here's the $296M Nobody Noticed.](URL)" >> file
```

The dollar signs will be interpreted as shell variables, mangling the title.

## Recommended Solutions

### 1. Use Python File I/O (Preferred)

```python
with open(file_path, 'a', encoding='utf-8') as f:
    f.write(f"{date} - [{title}]({url})\n")
```

Python handles encoding properly and avoids shell interpretation issues.

### 2. Use `printf` Instead of `echo`

```bash
printf "2026-05-05 - [%s](%s)\n" "$TITLE" "$URL" >> file
```

`printf` doesn't interpret backslashes or dollar signs like `echo` does.

### 3. Proper Shell Quoting

```bash
echo "2026-05-05 - [$TITLE](URL)" >> file
```

But this still has limitations with complex titles.

## Real Example from Session

During a cron run, the title `$HIMS Hired Amazon's AI Engineering Chief as CTO. Here's the $296M Nobody Noticed.` was initially appended with mangled dollar signs. The file had to be patched to restore the correct title.

## Best Practice

Always use Python file I/O for appending to the YouTube links file, especially when titles contain special characters. This ensures data integrity and avoids encoding-related corruption.

## Verification

After appending, verify the file by:
- Checking the last few lines for correct formatting
- Searching for known problematic characters to ensure they appear correctly
- Running a quick duplicate check to ensure no data was lost

## Related Issues

- See `oembed-api.md` for title fetching via YouTube's oEmbed endpoint
- See `fallback-mechanism.md` for overall fallback strategy