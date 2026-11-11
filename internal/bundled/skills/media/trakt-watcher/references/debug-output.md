# Debug Output for Trakt Watcher

When troubleshooting the Trakt Watcher script, you can add debug output to see the exact requests and responses. This helps identify issues like 400 Bad Request errors or other API problems.

## How to Add Debug Output

Modify the `check_watching_status` method to include print statements that show the request and response details.

### Original Method (Simplified)

```python
def check_watching_status(self):
    if not self.config['trakt_access_token']:
        print("ERROR: No access token available", file=sys.stderr)
        return None

    try:
        response = requests.get(f'{self.base_url}/users/me/watching', 
                               headers=self.headers, timeout=10)
        
        if response.status_code == 200:
            return response.json()
        elif response.status_code == 401:
            # Token expired, try to refresh
            if self.refresh_access_token():
                # Retry the request with new token
                response = requests.get(f'{self.base_url}/users/me/watching',
                                       headers=self.headers, timeout=10)
                if response.status_code == 200:
                    return response.json()
                else:
                    print(f"API Error after refresh: {response.status_code}", file=sys.stderr)
                    return None
            else:
                print("Failed to refresh token", file=sys.stderr)
                return None
        else:
            print(f"API Error: {response.status_code}", file=sys.stderr)
            return None
            
    except requests.exceptions.RequestException as e:
        print(f"Network error: {e}", file=sys.stderr)
        return None
```

### Debug Version

Add debug prints to show:
- The request being made (URL, headers)
- The response status and content
- When token refresh is attempted

```python
def check_watching_status(self):
    if not self.config['trakt_access_token']:
        print("ERROR: No access token available", file=sys.stderr)
        return None

    try:
        # DEBUG: Show request details
        print(f"DEBUG: Making request to {self.base_url}/users/me/watching", file=sys.stderr)
        print(f"DEBUG: Headers: {self.headers}", file=sys.stderr)
        
        response = requests.get(f'{self.base_url}/users/me/watching', 
                               headers=self.headers, timeout=10)
        
        # DEBUG: Show response details
        print(f"DEBUG: Received status {response.status_code}", file=sys.stderr)
        if response.status_code != 200:
            print(f"DEBUG: Response content: {response.text}", file=sys.stderr)
        
        if response.status_code == 200:
            return response.json()
        elif response.status_code == 401:
            print("DEBUG: Access token may be expired, attempting refresh", file=sys.stderr)
            # Token expired, try to refresh
            if self.refresh_access_token():
                print(f"DEBUG: Refresh successful, retrying request", file=sys.stderr)
                response = requests.get(f'{self.base_url}/users/me/watching',
                                       headers=self.headers, timeout=10)
                print(f"DEBUG: Retry received status {response.status_code}", file=sys.stderr)
                if response.status_code == 200:
                    return response.json()
                else:
                    print(f"DEBUG: API Error after refresh: {response.status_code}", file=sys.stderr)
                    return None
            else:
                print("DEBUG: Failed to refresh token", file=sys.stderr)
                return None
        else:
            print(f"DEBUG: API Error: {response.status_code}", file=sys.stderr)
            return None
            
    except requests.exceptions.RequestException as e:
        print(f"DEBUG: Network error: {e}", file=sys.stderr)
        return None
```

## When to Use Debug Output

- The script fails with a `[SILENT]` output and you need to see what's happening
- You get a 400 Bad Request or other non-401 error
- You want to verify that the Authorization header is correctly formatted
- You're debugging token refresh issues

## How to Enable

Simply replace the `check_watching_status` method in `references/trakt_watcher.py` with the debug version above.

## Notes

- Debug output goes to stderr, so it won't interfere with the normal notification output (stdout).
- Remove the debug output once the issue is resolved to keep the script clean.
- For persistent issues, check the logs or consider re-authenticating with Trakt to get fresh tokens.