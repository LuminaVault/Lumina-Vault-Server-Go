#!/usr/bin/env python3
"""
Trakt Watcher - Monitors Trakt.tv watching status and posts notifications

This script checks the user's current watching status on Trakt.tv and
compares it with the previous state to detect changes (started watching,
progress updates, stopped watching). It outputs notifications in a format
suitable for Hermes platform delivery.
"""

import os
import json
import time
import sys

import random
import time

try:
    import requests
except ImportError:
    print("ERROR: requests library is required. Please install: pip install requests", file=sys.stderr)
    sys.exit(1)

# Configuration - these are defaults from the skill, can be overridden
CONFIG = {
    'trakt_client_id': 'dc22389fbad6bb350eb7b3714d21a71d0a16c35cba461c375728d99f55c3de59',
    'trakt_access_token': None,
    'trakt_refresh_token': None,
    'trakt_client_secret': 'aa64d05b2812c0ad9e1a3430b199b933ea5bd0cb4671c45b65026c744fa827a3',
    'check_interval': 5
}

# State file paths
TOKEN_FILE = os.path.expanduser('~/.hermes/tokens/trakt.json')
STATE_FILE = os.path.expanduser('~/.hermes/trakt-watcher-state.json')

class TraktWatcher:
    def __init__(self):
        self.load_config()
        self.load_state()
        self.base_url = 'https://api.trakt.tv'
        self.headers = {
            'Content-Type': 'application/json',
            'Authorization': f'Bearer {self.config["trakt_access_token"]}',
            'trakt-api-version': '2',
            'trakt-api-key': self.config['trakt_client_id']
        }

    def load_config(self):
        """Load configuration from environment or default values"""
        # Try to load tokens from file
        if os.path.exists(TOKEN_FILE):
            try:
                with open(TOKEN_FILE, 'r') as f:
                    tokens = json.load(f)
                    self.config = {
                        'trakt_client_id': CONFIG['trakt_client_id'],
                        'trakt_access_token': tokens.get('access_token'),
                        'trakt_refresh_token': tokens.get('refresh_token'),
                        'trakt_client_secret': CONFIG['trakt_client_secret'],
                        'check_interval': CONFIG['check_interval']
                    }
            except (json.JSONDecodeError, IOError) as e:
                print(f"Warning: Could not load token file: {e}", file=sys.stderr)
                self.config = CONFIG.copy()
        else:
            self.config = CONFIG.copy()

    def load_state(self):
        """Load previous state from file"""
        if os.path.exists(STATE_FILE):
            try:
                with open(STATE_FILE, 'r') as f:
                    self.state = json.load(f)
            except (json.JSONDecodeError, IOError) as e:
                print(f"Warning: Could not load state file: {e}", file=sys.stderr)
                self.state = {'last_watching': None, 'last_check': 0}
        else:
            self.state = {'last_watching': None, 'last_check': 0}

    def save_state(self):
        """Save current state to file"""
        try:
            os.makedirs(os.path.dirname(STATE_FILE), exist_ok=True)
            with open(STATE_FILE, 'w') as f:
                json.dump(self.state, f, indent=2)
        except IOError as e:
            print(f"Warning: Could not save state file: {e}", file=sys.stderr)

    def refresh_access_token(self, max_retries=5):
        """Refresh the OAuth access token with exponential backoff and Retry-After support"""
        if not self.config['trakt_client_secret'] or not self.config['trakt_refresh_token']:
            print("ERROR: trakt_client_secret is not configured. Cannot refresh access token.", file=sys.stderr)
            return False

        for attempt in range(max_retries):
            try:
                data = {
                    'grant_type': 'refresh_token',
                    'refresh_token': self.config['trakt_refresh_token'],
                    'client_id': self.config['trakt_client_id'],
                    'client_secret': self.config['trakt_client_secret']
                }
                
                response = requests.post('https://trakt.tv/oauth/token', json=data, timeout=10)
                response.raise_for_status()
                
                tokens = response.json()
                self.config['trakt_access_token'] = tokens.get('access_token')
                self.config['trakt_refresh_token'] = tokens.get('refresh_token')
                
                # Save tokens to file
                token_data = {
                    'access_token': self.config['trakt_access_token'],
                    'refresh_token': self.config['trakt_refresh_token']
                }
                os.makedirs(os.path.dirname(TOKEN_FILE), exist_ok=True)
                with open(TOKEN_FILE, 'w') as f:
                    json.dump(token_data, f)
                
                # Update headers
                self.headers['Authorization'] = f'Bearer {self.config["trakt_access_token"]}'
                return True
                
            except requests.exceptions.HTTPError as e:
                if response.status_code == 429:
                    # Respect rate limiting
                    retry_after = int(response.headers.get('Retry-After', 1))
                    if attempt < max_retries - 1:
                        # Exponential backoff with jitter
                        backoff = min(retry_after * (2 ** attempt) + random.uniform(0, 1), 300)
                        print(f"Rate limited. Retrying in {backoff:.2f} seconds (attempt {attempt + 1}/{max_retries})", file=sys.stderr)
                        time.sleep(backoff)
                    else:
                        print(f"Max retries ({max_retries}) exceeded for rate limiting", file=sys.stderr)
                        return False
                else:
                    print(f"HTTP error: {e}", file=sys.stderr)
                    return False
            except requests.exceptions.RequestException as e:
                print(f"Network error: {e}", file=sys.stderr)
                # For network errors, use exponential backoff
                if attempt < max_retries - 1:
                    backoff = min(1 * (2 ** attempt) + random.uniform(0, 1), 300)
                    print(f"Network error, retrying in {backoff:.2f} seconds (attempt {attempt + 1}/{max_retries})", file=sys.stderr)
                    time.sleep(backoff)
                else:
                    print(f"Max retries ({max_retries}) exceeded for network errors", file=sys.stderr)
                    return False

    def check_watching_status(self):
        """Check the user's current watching status"""
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

    def generate_notification(self, current_watching):
        """Generate a notification based on current watching status"""
        if not current_watching:
            return None

        progress = current_watching.get('progress', 0)
        item = current_watching.get('item', {})
        show = item.get('show')
        movie = item.get('movie')
        
        if show:
            title = show.get('title')
            year = show.get('year')
            season = current_watching.get('season')
            episode = current_watching.get('episode')
            
            if episode:
                msg = f"📺 Watching {title} ({year}) - S{season:02d}E{episode:02d}"
                if progress > 0:
                    msg += f" ({progress}%)"
                return msg
            else:
                msg = f"📺 Watching {title} ({year})"
                if progress > 0:
                    msg += f" ({progress}%)"
                return msg
        elif movie:
            title = movie.get('title')
            year = movie.get('year')
            msg = f"🎬 Watching {title} ({year})"
            if progress > 0:
                msg += f" ({progress}%)"
            return msg
        
        return None

    def run(self):
        """Main execution method"""
        current_watching = self.check_watching_status()
        notification = None
        
        if current_watching:
            # Check if watching status has changed
            last_watching_type = self.state['last_watching'].get('type') if self.state['last_watching'] else None
            current_type = current_watching.get('type')
            
            # Check if it's a new item or progress update
            if (last_watching_type != current_type or 
                self.state['last_watching'] != current_watching):
                
                notification = self.generate_notification(current_watching)
                
                # Update state
                self.state['last_watching'] = current_watching
                self.state['last_check'] = time.time()
                self.save_state()
                
                # Return notification
                if notification:
                    return notification
                else:
                    # No notification generated, but state changed
                    return "[SILENT]"
            else:
                # No change in watching status
                return "[SILENT]"
        else:
            # Not watching anything or error occurred
            if self.state['last_watching'] is not None:
                # User stopped watching
                notification = "📺 Stopped watching"
                self.state['last_watching'] = None
                self.state['last_check'] = time.time()
                self.save_state()
                return notification
            else:
                return "[SILENT]"

if __name__ == '__main__':
    watcher = TraktWatcher()
    result = watcher.run()
    print(result)