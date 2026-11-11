---
name: x-post-ingestor
version: 1.1
description: |
  Ingest content from X/Twitter posts and extract URLs to the vault.
  Output filename convention: <YYYY-MM-DD>-<author>-<short-title>.md under Vault/raw/<category>/.
  LLM-driven steps (classification, summarization) should call provider `nous`
  model `arcee-ai/trinity-large-thinking` (free). Keep all other reasoning on the default model.
author: Hermes Agent
depends:
  - superpowers
  - terminal
  - file
  - messaging

params:
  - name: post_url
  - name: vault_path

exec: |
  import os
  import re
  import requests
  from hermes_tools import terminal, send_message, read_file, write_file, patch
  from bs4 import BeautifulSoup
  import time
  
  url = skill.Params.post_url or ""
  vault = skill.Params.vault_path or "Vault"
  
  if not url:
      print("Error: No URL provided")
      exit(1)
  
  print(f"Fetching X post: {url}")
  
  headers = {
      'User-Agent': 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36'
  }
  
  try:
      response = requests.get(url, headers=headers, timeout=30)
      response.raise_for_status()
      html = response.text
  except Exception as e:
      print(f"Error fetching post: {e}")
      exit(1)
  
  soup = BeautifulSoup(html, 'html.parser')
  
  # Extract URLs
  urls = []
  for link in soup.find_all('a', href=True):
      href = link['href']
      if href.startswith('http') and not href.startswith('https://t.co/'):
          urls.append(href)
  
  if not urls:
      print("No URLs found in the post")
      exit(0)
  
  # Ingest each URL.
  # Canonical vault root: /opt/data/home/obsidian-vault/<vault>/raw/<category>/
  vault_base = f"/opt/data/home/obsidian-vault/{vault}/"
  raw_dir = os.path.join(vault_base, "raw", "web")
  os.makedirs(raw_dir, exist_ok=True)

  def _slugify(text, max_words=8):
      import unicodedata
      s = unicodedata.normalize("NFKD", text or "").encode("ascii", "ignore").decode()
      s = re.sub(r"[^A-Za-z0-9\s-]", " ", s).lower().strip()
      words = [w for w in re.split(r"\s+", s) if w][:max_words]
      return "-".join(words) or "post"

  def _author_from_post_url(post_url):
      m = re.search(r"(?:x|twitter|fixupx)\.com/([A-Za-z0-9_]+)/status/", post_url or "")
      return m.group(1) if m else "unknown"

  def _unique(path):
      if not os.path.exists(path):
          return path
      base, ext = os.path.splitext(path)
      for i in range(2, 1000):
          c = f"{base}-{i}{ext}"
          if not os.path.exists(c):
              return c
      return path

  author = _author_from_post_url(url)

  for url in urls:
      try:
          response = requests.get(url, headers=headers, timeout=30)
          response.raise_for_status()
          content = response.text
          
          page_soup = BeautifulSoup(content, 'html.parser')
          
          title_tag = page_soup.find('title')
          if title_tag:
              title = title_tag.string or title_tag.text
          else:
              h1 = page_soup.find('h1')
              title = h1.string if h1 else "Untitled"
          
          title = re.sub(r'\s+', ' ', title).strip()
          
          content_div = None
          for selector in ['main', 'article', 'div.content', 'div.post', 'div.entry']:
              content_div = page_soup.find(selector)
              if content_div:
                  break
          
          if not content_div:
              text = page_soup.get_text()
          else:
              for tag in content_div(['script', 'style', 'nav', 'footer', 'aside']):
                  tag.decompose()
              text = content_div.get_text()
          
          text = re.sub(r'\s+', ' ', text).strip()

          date_part = time.strftime("%Y-%m-%d")
          filename = f"{date_part}-{_slugify(author, 4)}-{_slugify(title, 8)}.md"
          filepath = _unique(os.path.join(raw_dir, filename))

          frontmatter = f"""---
title: {title}
author: {author}
date: {date_part}
ingested_at: {time.strftime("%Y-%m-%dT%H:%M:%S")}
source: {url}
categories: [X Post, Social Media]
tags: [Twitter, X]
status: uncompiled
---

"""
          with open(filepath, 'w', encoding='utf-8') as f:
              f.write(frontmatter)
              f.write(text)
          
          print(f"✅ Ingested {url} to {filepath}")
          send_message(target="discord:YOUR_DISCORD_CHANNEL_ID", message=f"✅ Ingested URL from X post: {title}")
          
      except Exception as e:
          print(f"Error ingesting {url}: {e}")
          continue
  
  print("Done!")