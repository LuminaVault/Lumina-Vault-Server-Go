# Fetching YouTube Video Titles via oEmbed API

When enriching YouTube links with actual video titles, the YouTube Data API is the standard approach. However, it requires an API key and has quota limits. An alternative is YouTube's oEmbed API, which provides a simple, no-auth endpoint for retrieving basic video metadata including title.

## oEmbed API Endpoint

```
https://www.youtube.com/oembed?url=<YouTube URL>&format=json
```

## Implementation Example

```python
import requests
import json

def get_youtube_title(video_id):
    url = f"https://www.youtube.com/oembed?url=https://www.youtube.com/watch?v={video_id}&format=json"
    try:
        response = requests.get(url, timeout=10)
        if response.status_code == 200:
            data = response.json()
            return data['title']
        return None
    except Exception as e:
        print(f"Error fetching title: {e}")
        return None
```

## Advantages of oEmbed

- **No API key required** - simple public endpoint
- **No quota limits** - can make unlimited requests
- **Fast and lightweight** - returns minimal metadata
- **Reliable** - stable endpoint from YouTube

## Limitations

- **Basic metadata only** - title, author, thumbnail, dimensions
- **No view count, description, etc.** - not a full replacement for Data API
- **Rate limiting possible** - though no official limits, aggressive usage may trigger blocks

## Usage in YouTube Link Ingestor

When a new YouTube link is found, call `get_youtube_title(video_id)` to retrieve the title. Then format the entry as:

```
YYYY-MM-DD - [Video Title](https://www.youtube.com/watch?v=VIDEO_ID)
```

## Encoding Considerations

Special characters in titles (dollar signs, ampersands, quotes, etc.) need proper shell escaping when appending to files. Use Python's `shlex` or careful string formatting to avoid corruption.

### Example of the Pitfall

Attempting to echo a title with dollar signs directly can cause issues:

```bash
echo "2026-05-05 - [$HIMS Hired Amazon...](URL)" >> file
```

The `$` character is interpreted as a shell variable, mangling the title. Always use proper quoting or Python file I/O.

## Error Handling

- If title fetch fails, fall back to using the video ID or "YouTube Video" as placeholder
- Implement retry logic with exponential backoff for transient failures
- Consider caching titles to reduce API calls for frequently seen videos

## Testing

Verify title fetching works correctly by testing with various video types (music, educational, news, etc.) and ensuring special characters are handled properly.

## Alternatives

- **YouTube Data API**: Full metadata but requires API key and has quotas
- **Scraping**: HTML parsing but fragile to YouTube layout changes
- **Third-party services**: Additional dependencies and potential costs

The oEmbed API provides an excellent balance of simplicity and functionality for the YouTube Link Ingestor use case.