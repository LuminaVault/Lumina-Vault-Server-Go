## Script Discovery Pattern

When the expected news digest script (`news_digest.py`) is missing, search for alternative scripts in the same directory using a wildcard pattern:

```python
import glob
import os

script_dir = "/opt/data/home/.hermes/scripts"
scripts = glob.glob(os.path.join(script_dir, "*news_digest.py"))
```

This will find all files matching `*news_digest.py`, such as:
- `go_news_digest.py`
- `swift_news_digest.py`

If no scripts are found, raise an error. Otherwise, execute each script and capture its output.

## Example Discovery

In this session, the following scripts were discovered:
- `/opt/data/home/.hermes/scripts/go_news_digest.py`
- `/opt/data/home/.hermes/scripts/swift_news_digest.py`

Both were executed successfully.