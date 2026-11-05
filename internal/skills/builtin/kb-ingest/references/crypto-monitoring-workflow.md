# Crypto Markets Monitoring Workflow

This document outlines the workflow for monitoring crypto markets and Web3 intelligence sources, as implemented in the May 9, 2026 ingestion session.

## Objective
Automatically ingest latest crypto-related news, market analysis, and protocol updates from authoritative sources into the Obsidian knowledge base.

## Sources Monitored
- **CoinDesk** - Primary source for crypto news, policy, and markets
- **The Block** - Institutional-grade crypto news and research  
- **Aave** - DeFi protocol updates and technical documentation
- **Santiment** - On-chain analytics and market data

## Content Extraction Strategy

### Primary Method: jina.ai
For most websites, especially JavaScript-heavy sites and news platforms:
```bash
curl -s "https://r.jina.ai/http://{URL}"
```

### Topic-Based Organization
Content is categorized into appropriate raw subdirectories:

- **Tech/DeFi:** Protocol updates, technical deep-dives, DeFi developments
- **Tech/General:** Programming, cybersecurity, blockchain tech
- **Business:** Market analysis, price movements, financial news
- **AI:** AI agent developments, machine learning applications in crypto

## Sample Ingestion Results

### Regulatory & Policy
- Aave's $71M ETH recovery from North Korea hack (court ruling)
- Senate Banking Committee Clarity Act hearing
- SEC Chair Atkins' new onchain market rules
- World Liberty Financial security analysis

### Market Analysis
- Coinbase stock rebound amid altcoin surge
- Bitcoin price action (above $80K, then correction)
- Arbitrum DAO governance decision on Aave recovery
- Institutional interest in perp DEXs

### DeFi Protocols
- Aave V3 comprehensive overview
- Aave Pro V4 institutional features
- Aave App for mainstream savings

## Key Success Factors

1. **Robust extraction:** jina.ai handled paywalls and JavaScript rendering effectively
2. **Topic-based filing:** Content properly categorized for easy retrieval
3. **Comprehensive coverage:** Multiple sources provided diverse perspectives
4. **Proper metadata:** All files include source URLs, timestamps, and content types
5. **Vault compatibility:** Files written to existing raw subdirectories without structure conflicts

## Lessons Learned

- The actual vault uses topic-based subdirectories (`AI`, `Tech`, `Business`) not type-based (`web/`, `pdfs/`)
- Always test vault writability before bulk ingestion
- Have a fallback plan (temporary directory) for permission issues
- jina.ai is reliable for most news sites, including CoinDesk and The Block
- Crypto content spans multiple subdirectories (Tech, Business, AI)