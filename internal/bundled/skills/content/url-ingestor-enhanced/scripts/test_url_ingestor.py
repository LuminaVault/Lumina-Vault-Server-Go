#!/usr/bin/env python3
"""
Test script for URL Ingestor Enhanced
Tests various URL types and extraction methods
"""

import sys
import os
import asyncio

# Add the skill directory to the path
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from url_ingestor import URLIngestor

async def test_url_ingestor():
    """Test the URL ingestor with various URL types"""
    ingestor = URLIngestor()
    
    test_urls = [
        # Article URLs
        ("https://techcrunch.com/2026/05/05/ai-breakthrough/", "Article"),
        ("https://example.com/some-interesting-article", "Article"),
        
        # GitHub repositories
        ("https://github.com/apple/swift-algorithms", "GitHub Repo"),
        ("https://github.com/vapor/vapor", "GitHub Repo"),
        
        # YouTube videos
        ("https://www.youtube.com/watch?v=example", "YouTube Video"),
        ("https://youtu.be/dQw4w9WgXcQ", "YouTube Video"),
        
        # Generic web pages
        ("https://en.wikipedia.org/wiki/Artificial_intelligence", "Wikipedia"),
        ("https://stackoverflow.com/questions/54717868/swift-5-0-release-date", "Stack Overflow"),
    ]
    
    print("=" * 70)
    print("Testing URL Ingestor Enhanced")
    print("=" * 70)
    
    results = []
    for url, url_type in test_urls:
        print(f"\nTesting {url_type}: {url}")
        print("-" * 70)
        
        try:
            # Test extraction
            if 'github.com' in url:
                title, content, extracted_url = ingestor.extract_github_repo(url)
            elif 'youtube.com' in url or 'youtu.be' in url:
                title, content, extracted_url = ingestor.extract_youtube_video(url)
            else:
                # Use async extraction for articles
                loop = asyncio.get_event_loop()
                title, content, extracted_url = loop.run_until_complete(
                    ingestor.extract_article_content(url)
                )
            
            if title and content:
                print(f"✅ Successfully extracted")
                print(f"   Title: {title[:100]}...")
                print(f"   Content length: {len(content)} chars")
                print(f"   Topic: {ingestor.classify_content(title, content, url)}")
                results.append(('success', url_type, url))
            else:
                print(f"❌ Extraction failed")
                results.append(('failed', url_type, url))
                
        except Exception as e:
            print(f"❌ Error: {e}")
            results.append(('error', url_type, url))
    
    # Summary
    print("\n" + "=" * 70)
    print("Test Results Summary")
    print("=" * 70)
    print(f"Total tests: {len(test_urls)}")
    print(f"Success: {sum(1 for r in results if r[0] == 'success')}")
    print(f"Failed: {sum(1 for r in results if r[0] == 'failed')}")
    print(f"Errors: {sum(1 for r in results if r[0] == 'error')}")
    
    # Detailed results
    print("\nDetailed Results:")
    for status, url_type, url in test_urls:
        status_icon = "✅" if status == 'success' else "❌" if status == 'failed' else "❌"
        print(f"  {status_icon} {url_type}: {url}")

if __name__ == '__main__':
    asyncio.run(test_url_ingestor())