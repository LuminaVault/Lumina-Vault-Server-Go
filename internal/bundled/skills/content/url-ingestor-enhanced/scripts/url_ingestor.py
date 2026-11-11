#!/usr/bin/env python3
"""
Universal URL Ingestion Script - Standard Library Version

This script captures ANY URL from any platform (Discord, Telegram, Slack, chat)
and automatically adds it to the Obsidian vault with proper classification and formatting.
Uses only Python standard library.
"""

import os
import sys
import json
import time
import hashlib
import logging
import subprocess
from pathlib import Path
from datetime import datetime
from urllib.parse import urlparse, parse_qs, quote
import re
import email
import urllib.request
import urllib.error
from http.client import HTTPException
import traceback

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - __main__ - %(levelname)s - %(message)s',
    handlers=[
        logging.StreamHandler(sys.stdout),
        logging.FileHandler('/tmp/url_ingestor.log')
    ]
)
logger = logging.getLogger(__name__)

# Configuration from environment
OBSIDIAN_VAULT_PATH = os.getenv('OBSIDIAN_VAULT_PATH', '/opt/data/obsidian-vault/Vault')
OBSIDIAN_VAULT_NAME = os.getenv('OBSIDIAN_VAULT_NAME', 'Vault')
MAX_CONTENT_LENGTH = int(os.getenv('MAX_CONTENT_LENGTH', '10000'))
EXTRACTION_TIMEOUT = int(os.getenv('EXTRACTION_TIMEOUT', '30'))

# State file
STATE_FILE = os.path.join(os.path.expanduser('~'), '.hermes', 'state', 'url_ingestor_state.json')

# URL patterns
URL_PATTERN = r'https?://[^\s<>]+|www\.[^\s<>]+|ftp://[^\s<>]+'

# Classification categories and keywords
CLASSIFICATION_KEYWORDS = {
    'AI': ['ai', 'ml', 'llm', 'gpt', 'claude', 'hermes', 'agent', 'openai', 'anthropic', 'deepseek', 'gemini', 'mistral', 'perplexity', 'cursor', 'windsurf', 'sora', 'dalle', 'stable diffusion'],
    'Development': ['swift', 'ios', 'xcode', 'apple', 'uikit', 'swiftui', 'vapor', 'hummingbird', 'appstore', 'ipa', 'macos', 'objective-c', 'wwdc', 'sf symbols', 'core data', 'swiftdata', 'realm', 'firebase'],
    'Stocks': ['stock', 'ticker', 'amd', 'googl', 'zeta', 'hims', 'rdw', 'smr', 'elf', 'oust', 'portfolio', 'earnings', 'buy', 'sell', 'market', 'invest', 'cathie wood', 'tesla', 'nvda', 'celh', 'msft', 'meta', 'amzn', 'apple'],
    'Health': ['hims', 'weight-loss', 'glp-1', 'telehealth', 'biotech', 'eli lilly', 'novo', 'ozempic', 'wegovy', 'pfizer', 'moderna', 'abbvie', 'roche', 'novartis', 'j&j', 'fitness', 'nutrition'],
    'Technology': ['google', 'amazon', 'microsoft', 'startup', 'saas', 'tech', 'api', 'cloud', 'aws', 'azure', 'meta', 'nvidia', 'intel', 'qualcomm', 'chip', 'semiconductor', 'gpu', 'hardware', 'software'],
    'Business': ['revenue', 'profit', 'startup', 'funding', 'acquihire', 'ipo', 'valuation', 'billion', 'acquires', 'merger', 'biz dev', 'partnership', 'investment round', 'series a', 'series b', 'vc'],
    'News': ['breaking', 'news', 'report', 'announcement', 'update', 'press release', 'official', 'confirmed', 'leaked'],
    'Entertainment': ['movies', 'music', 'games', 'celebrities', 'netflix', 'hulu', 'disney', 'spotify'],
    'Science': ['scientific', 'space', 'physics', 'chemistry', 'biology', 'discovery', 'research', 'innovation']
}

class URLIngestionError(Exception):
    """Custom exception for URL ingestion errors"""
    pass

class URLIngestor:
    def __init__(self):
        self.state = self.load_state()
        logger.info(f"URLIngestor initialized. State file: {STATE_FILE}")
        
    def load_state(self) -> dict:
        """Load state from file"""
        try:
            with open(STATE_FILE, 'r') as f:
                state = json.load(f)
                logger.debug(f"Loaded state: {len(state['processed_urls'])} URLs processed")
                return state
        except (FileNotFoundError, json.JSONDecodeError):
            logger.info("State file not found, creating new state")
            return {
                'processed_urls': {},
                'last_processed': None,
                'platform_counters': {
                    'discord': 0,
                    'telegram': 0,
                    'slack': 0,
                    'chat': 0
                }
            }
    
    def save_state(self):
        """Save state to file"""
        state_dir = os.path.dirname(STATE_FILE)
        if not os.path.exists(state_dir):
            os.makedirs(state_dir, exist_ok=True)
        
        with open(STATE_FILE, 'w') as f:
            json.dump(self.state, f, indent=2, default=str)
        logger.debug(f"State saved: {len(self.state['processed_urls'])} URLs processed")
    
    def extract_urls(self, text: str) -> list:
        """Extract all URLs from text"""
        urls = re.findall(URL_PATTERN, text, re.IGNORECASE)
        # Clean URLs (remove trailing punctuation, etc.)
        cleaned_urls = []
        for url in urls:
            # Remove surrounding <>, quotes, etc.
            url = url.strip('<>"\':')
            # Remove trailing punctuation
            while url and url[-1] in ',.;:!?)]}':
                url = url[:-1]
            if url:
                cleaned_urls.append(url)
        logger.debug(f"Extracted {len(cleaned_urls)} URLs from text")
        return cleaned_urls
    
    def normalize_url(self, url: str) -> str:
        """Normalize URL for deduplication"""
        # Remove tracking parameters
        parsed = urlparse(url)
        query_params = parse_qs(parsed.query)
        # Remove common tracking parameters
        for param in ['utm_source', 'utm_medium', 'utm_campaign', 'utm_content', 'utm_term', 'fbclid', 'gclid']:
            if param in query_params:
                del query_params[param]
        # Rebuild query string
        new_query = '&'.join([f'{k}={v[0]}' if isinstance(v, list) else f'{k}={v}' 
                            for k, v in query_params.items() if v])
        # Rebuild URL
        normalized = parsed._replace(query=new_query).geturl()
        # Remove trailing slash
        if normalized.endswith('/'):
            normalized = normalized[:-1]
        logger.debug(f"Normalized URL: {url} -> {normalized}")
        return normalized
    
    def get_url_hash(self, url: str) -> str:
        """Generate a hash for URL deduplication"""
        normalized = self.normalize_url(url)
        url_hash = hashlib.sha256(normalized.encode()).hexdigest()[:16]
        logger.debug(f"URL hash for {url}: {url_hash}")
        return url_hash
    
    def fetch_url(self, url: str, timeout: int = 30) -> tuple:
        """Fetch URL content using urllib (standard library)"""
        try:
            # Add headers to avoid being blocked
            headers = {
                'User-Agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36',
                'Accept': 'text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8',
                'Accept-Language': 'en-US,en;q=0.5',
                'Accept-Encoding': 'gzip, deflate',
                'Connection': 'keep-alive',
                'Upgrade-Insecure-Requests': '1',
            }
            
            logger.info(f"Fetching {url} with timeout {timeout}")
            req = urllib.request.Request(
                url,
                data=None,
                headers=headers,
                method='GET'
            )
            
            with urllib.request.urlopen(req, timeout=timeout) as response:
                content_type = response.headers.get('Content-Type', '')
                html = response.read()
                
                # Decode based on content type
                if 'charset=' in content_type:
                    charset = content_type.split('charset=')[-1].split(';')[0].strip()
                    try:
                        html = html.decode(charset)
                    except (LookupError, UnicodeDecodeError):
                        html = html.decode('utf-8', errors='replace')
                else:
                    html = html.decode('utf-8', errors='replace')
                
                logger.debug(f"Fetched {url} successfully, length: {len(html)} chars")
                return html, response.headers
            
        except urllib.error.HTTPError as e:
            logger.error(f"HTTP Error {e.code} for {url}: {e.reason}")
            return None, None
        except urllib.error.URLError as e:
            logger.error(f"URL Error for {url}: {e.reason}")
            return None, None
        except Exception as e:
            logger.error(f"Error fetching {url}: {e}")
            logger.error(traceback.format_exc())
            return None, None
    
    def extract_article_content(self, url: str) -> tuple:
        """Extract article content using Jina AI via HTTP"""
        # Use Jina AI reader API
        jina_url = f"https://r.jina.ai/http://{url.split('://', 1)[1]}"
        
        try:
            # DEBUG: Check if EXTRACTION_TIMEOUT is accessible
            logger.debug(f"EXTRACTION_TIMEOUT value: {EXTRACTION_TIMEOUT}")
            logger.debug(f"Timeout type: {type(EXTRACTION_TIMEOUT)}")
            
            html, headers = self.fetch_url(jina_url, timeout=EXTRACTION_TIMEOUT)
            
            if not html:
                logger.warning(f"No HTML returned from {jina_url}")
                return None, None, None
            
            # Split into title and body (Jina format: Title\n\nBody...)
            parts = html.split('\n\n', 1)
            if len(parts) == 2:
                title, body = parts
            else:
                title = parts[0] if parts else "Untitled"
                body = ""
            
            # Clean title
            title = title.strip()
            
            logger.debug(f"Extracted article: {title[:50]}... (length: {len(body)} chars)")
            return title, body, url
        except Exception as e:
            logger.error(f"Error in extract_article_content: {e}")
            logger.error(traceback.format_exc())
            return None, None, None
    
    def extract_github_repo(self, url: str) -> tuple:
        """Extract GitHub repository information"""
        parsed = urlparse(url)
        path_parts = parsed.path.strip('/').split('/')
        
        if len(path_parts) < 2:
            logger.warning(f"Invalid GitHub URL format: {url}")
            return None, None, None
        
        owner, repo_name = path_parts[0], path_parts[1]
        
        # GitHub API URL
        api_url = f"https://api.github.com/repos/{owner}/{repo_name}"
        
        try:
            headers = {'User-Agent': 'Hermes'}
            if 'GITHUB_TOKEN' in os.environ:
                headers['Authorization'] = f'token {os.environ["GITHUB_TOKEN"]}'
            
            # Use urllib to make the request
            req = urllib.request.Request(
                api_url,
                data=None,
                headers=headers,
                method='GET'
            )
            
            logger.info(f"Fetching GitHub repo: {api_url}")
            with urllib.request.urlopen(req, timeout=EXTRACTION_TIMEOUT) as response:
                data = json.loads(response.read().decode('utf-8', errors='replace'))
                
                title = data['name']
                description = data.get('description', '')
                stars = data['stargazers_count']
                language = data.get('language', '')
                repo_url = data['html_url']
                
                # Get README
                readme_url = f"{api_url}/readme"
                readme_req = urllib.request.Request(
                    readme_url,
                    data=None,
                    headers=headers,
                    method='GET'
                )
                
                try:
                    with urllib.request.urlopen(readme_req, timeout=30) as readme_response:
                        readme_data = json.loads(readme_response.read().decode('utf-8', errors='replace'))
                        
                        readme_content = ""
                        if readme_data.get('encoding') == 'base64':
                            readme_content = base64.b64decode(readme_data['content']).decode('utf-8', errors='replace')
                        else:
                            readme_content = readme_data.get('content', '')
                except:
                    readme_content = ""
                
                # Combine description and README
                content = f"{description}\n\n---\n\n{readme_content}"
                logger.debug(f"Extracted GitHub repo: {title}")
                return title, content, repo_url
                
        except Exception as e:
            logger.error(f"Error extracting GitHub repo {url}: {e}")
            logger.error(traceback.format_exc())
            return None, None, None
    
    def extract_youtube_video(self, url: str) -> tuple:
        """Extract YouTube video information"""
        # Extract video ID
        video_id = None
        if 'youtube.com' in url:
            query = urlparse(url).query
            video_id = parse_qs(query).get('v', [None])[0]
        elif 'youtu.be' in url:
            video_id = url.split('/')[-1]
        
        if not video_id:
            logger.warning(f"Could not extract video ID from {url}")
            return None, None, None
        
        # Try to get video info using yt-dlp if available
        try:
            result = subprocess.run(
                ['yt-dlp', '--get-title', '--get-description', '--skip-download', url],
                capture_output=True,
                text=True,
                timeout=30
            )
            if result.returncode == 0:
                output = result.stdout.strip()
                lines = output.split('\n')
                if len(lines) >= 2:
                    title = lines[0]
                    description = '\n'.join(lines[1:])
                    logger.debug(f"Extracted YouTube video: {title}")
                    return title, description, url
        except:
            pass
        
        # Fallback to simple extraction
        title = f"YouTube Video: {video_id}"
        description = f"Video ID: {video_id}\nURL: {url}"
        logger.debug(f"Simple YouTube extraction: {title}")
        return title, description, url
    
    def extract_generic_page(self, url: str, html: str) -> tuple:
        """Extract title and main content from generic web page"""
        # Try to extract title
        title_match = re.search(r'<title>(.*?)</title>', html, re.IGNORECASE | re.DOTALL)
        title = title_match.group(1).strip() if title_match else "Untitled"
        
        # Try to extract main content (simplified)
        # Remove script/style/iframe tags
        clean_html = re.sub(r'<(script|style|iframe)[^>]*>.*?</\1>', '', html, flags=re.DOTALL | re.IGNORECASE)
        # Extract text
        text = re.sub(r'<[^>]+>', ' ', clean_html)
        text = re.sub(r'\s+', ' ', text).strip()
        
        # Limit content length
        content = text[:MAX_CONTENT_LENGTH]
        
        logger.debug(f"Extracted generic page: {title[:50]}... (length: {len(content)} chars)")
        return title, content
    
    def classify_content(self, title: str, content: str, url: str) -> str:
        """Classify content by topic"""
        text = f"{title} {content}"[:1000].lower()
        
        # Check each category in order
        for category, keywords in CLASSIFICATION_KEYWORDS.items():
            for keyword in keywords:
                if keyword in text:
                    logger.debug(f"Classified as {category}: {title[:30]}...")
                    return category
        
        # Default to "Other" if no match
        logger.debug(f"No match found, classifying as Other: {title[:30]}...")
        return "Other"
    
    def sanitize_filename(self, title: str, max_length: int = 120) -> str:
        """Sanitize title for filename"""
        # Remove illegal filesystem characters
        sanitized = re.sub(r'[<>:"/\\|?*]', '_', title)
        # Replace multiple spaces with single hyphen
        sanitized = re.sub(r'\s+', '-', sanitized)
        # Remove leading/trailing hyphens
        sanitized = sanitized.strip('-')
        # Truncate to max length
        if len(sanitized) > max_length:
            sanitized = sanitized[:max_length-3] + '...'
        logger.debug(f"Sanitized filename: {title[:30]}... -> {sanitized}")
        return sanitized
    
    def create_markdown_file(self, title: str, content: str, url: str, topic: str, 
                           source: str, captured_at: str) -> str:
        """Create markdown file with proper frontmatter"""
        # Create directory if it doesn't exist
        raw_dir = os.path.join(OBSIDIAN_VAULT_PATH, 'Raw', topic)
        os.makedirs(raw_dir, exist_ok=True)
        
        # Sanitize filename
        filename = self.sanitize_filename(title)
        filepath = os.path.join(raw_dir, f"{filename}.md")
        
        # Avoid filename collisions
        counter = 1
        original_filepath = filepath
        while os.path.exists(filepath):
            filepath = os.path.join(raw_dir, f"{filename}_{counter}.md")
            counter += 1
        
        # Create markdown content
        frontmatter = f"""---
classification: {topic}
source: {url}
captured_at: {captured_at}
original_content: true
source_platform: {source}
---

# {title}

{content}

*Originally captured from {url} on {captured_at}*
"""
        
        # Write file
        with open(filepath, 'w', encoding='utf-8') as f:
            f.write(frontmatter)
        
        logger.info(f"Created file: {filepath}")
        return filepath
    
    def trigger_compilation(self):
        """Trigger knowledge base compilation"""
        try:
            # Check if compile_wiki.py exists in multiple locations
            possible_paths = [
                os.path.join(OBSIDIAN_VAULT_PATH, 'scripts', 'compile_wiki.py'),
                os.path.join(os.path.expanduser('~'), '.hermes', 'scripts', 'compile_wiki.py'),
                '/opt/data/home/.hermes/scripts/compile_wiki.py'
            ]
            
            compile_script = None
            for path in possible_paths:
                if os.path.exists(path):
                    compile_script = path
                    break
            
            if compile_script:
                logger.info(f"Triggering compilation with {compile_script}")
                result = subprocess.run(
                    [sys.executable, compile_script, '--root', OBSIDIAN_VAULT_PATH],
                    capture_output=True,
                    text=True,
                    timeout=300
                )
                if result.returncode == 0:
                    logger.info(f"Compilation successful: {result.stdout}")
                else:
                    logger.error(f"Compilation failed: {result.stderr}")
            else:
                logger.warning("compile_wiki.py not found, skipping compilation")
                
        except Exception as e:
            logger.error(f"Error triggering compilation: {e}")
            logger.error(traceback.format_exc())
    
    def process_url(self, url: str, source: str = 'chat') -> bool:
        logger.info(f"Processing URL: {url} (source: {source})")
        
        url_hash = self.get_url_hash(url)
        
        # Check if already processed
        if url_hash in self.state['processed_urls']:
            logger.info(f"URL already processed: {url}")
            return False
        
        try:
            logger.info(f"Processing URL: {url} (source: {source})")
            
            # Determine URL type and extract content
            title = None
            content = None
            extracted_url = url
            
            # Check URL type
            if 'github.com' in url:
                title, content, extracted_url = self.extract_github_repo(url)
            elif 'youtube.com' in url or 'youtu.be' in url:
                title, content, extracted_url = self.extract_youtube_video(url)
            else:
                # Default to article extraction
                title, content, extracted_url = self.extract_article_content(url)
            
            # If extraction failed, try generic extraction
            if not title or not content:
                html, headers = self.fetch_url(url, timeout=EXTRACTION_TIMEOUT)
                if html:
                    title, content = self.extract_generic_page(url, html)
                    extracted_url = url
            
            # If still no content, skip
            if not title or not content:
                logger.warning(f"Could not extract content from {url}")
                return False
            
            # Classify content
            topic = self.classify_content(title, content, url)
            
            # Create markdown file
            captured_at = datetime.utcnow().isoformat() + 'Z'
            filepath = self.create_markdown_file(
                title=title,
                content=content[:MAX_CONTENT_LENGTH],
                url=extracted_url,
                topic=topic,
                source=source,
                captured_at=captured_at
            )
            
            # Update state
            self.state['processed_urls'][url_hash] = {
                'url': url,
                'title': title,
                'topic': topic,
                'filepath': filepath,
                'source': source,
                'captured_at': captured_at
            }
            self.state['platform_counters'][source] = self.state['platform_counters'].get(source, 0) + 1
            self.state['last_processed'] = captured_at
            
            # Save state
            self.save_state()
            
            # Trigger compilation if new content was added
            self.trigger_compilation()
            
            logger.info(f"Successfully processed {url} -> {topic}/{filename}")
            return True
            
        except Exception as e:
            logger.error(f"Error processing {url}: {e}")
            logger.error(traceback.format_exc())
            return False
    
    def process_urls(self, urls: list, source: str = 'chat') -> dict:
        """Process multiple URLs"""
        results = {
            'total': len(urls),
            'success': 0,
            'failed': 0,
            'skipped': 0,
            'details': []
        }
        
        for url in urls:
            if not url:
                continue
            
            # Check if already processed
            url_hash = self.get_url_hash(url)
            if url_hash in self.state['processed_urls']:
                results['skipped'] += 1
                results['details'].append({'url': url, 'status': 'skipped', 'reason': 'already_processed'})
                continue
            
            try:
                success = self.process_url(url, source)
                if success:
                    results['success'] += 1
                    results['details'].append({'url': url, 'status': 'success', 'topic': self.state['processed_urls'][url_hash]['topic']})
                else:
                    results['failed'] += 1
                    results['details'].append({'url': url, 'status': 'failed', 'reason': 'extraction_failed'})
            except Exception as e:
                results['failed'] += 1
                results['details'].append({'url': url, 'status': 'failed', 'reason': str(e)})
        
        return results

def main():
    """Main function"""
    import argparse
    
    parser = argparse.ArgumentParser(description='Universal URL Ingestion Script')
    parser.add_argument('--url', help='Single URL to process')
    parser.add_argument('--urls-file', help='File containing URLs (one per line)')
    parser.add_argument('--source', default='chat', 
                       choices=['discord', 'telegram', 'slack', 'chat', 'other'],
                       help='Source platform')
    parser.add_argument('--test', action='store_true', help='Test mode (no file creation)')
    
    args = parser.parse_args()
    
    # Initialize ingestor
    ingestor = URLIngestor()
    
    # Process URLs
    if args.url:
        # Single URL
        urls = [args.url]
    elif args.urls_file:
        # Multiple URLs from file
        try:
            with open(args.urls_file, 'r') as f:
                urls = [line.strip() for line in f if line.strip()]
        except FileNotFoundError:
            logger.error(f"File not found: {args.urls_file}")
            return
    else:
        # No URLs provided
        logger.error("No URLs provided. Use --url or --urls-file")
        return
    
    # Process in test mode if requested
    if args.test:
        logger.info("Test mode: No files will be created")
        # Just simulate processing
        for url in urls[:5]:  # Process first 5 for test
            print(f"Would process: {url}")
        return
    
    # Process URLs
    results = ingestor.process_urls(urls, source=args.source or 'chat')
    
    # Print summary
    print(f"\nURL Ingestion Summary:")
    print(f"Total: {results['total']}")
    print(f"Success: {results['success']}")
    print(f"Failed: {results['failed']}")
    print(f"Skipped: {results['skipped']}")
    
    if results['details']:
        print("\nDetails:")
        for detail in results['details'][:10]:  # Show first 10
            if detail['status'] == 'success':
                print(f"  ✅ {detail['url']} -> {detail.get('topic', 'Unknown')}")
            else:
                print(f"  ❌ {detail['url']} -> {detail.get('reason', 'Failed')}")

if __name__ == '__main__':
    main()