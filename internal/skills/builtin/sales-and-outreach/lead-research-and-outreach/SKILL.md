---
name: lead-research-and-outreach
version: 1.0
description: Comprehensive lead research and outreach preparation system. Finds target accounts, researches companies, identifies buying signals, summarizes opportunities, and drafts personalized outreach messages.
author: Hermes Agent
created: 2026-05-06
status: active
platforms: [local, discord, telegram, slack]
toolsets: [web, terminal, file, search]
dependencies: [web_search, read_file, write_file, execute_code, clarify]
---
# Lead Research and Outreach Prep System

## Overview
This skill provides end-to-end lead research and outreach preparation. It helps sales teams move from raw data to actionable outreach opportunities with human oversight.

## Key Features
- **Target Account Discovery**: Find relevant companies based on industry, location, or other criteria
- **Company Research**: Gather comprehensive company information (website, size, funding, products, etc.)
- **Buying Signal Detection**: Identify key signals like funding rounds, hiring trends, product launches
- **Opportunity Analysis**: Summarize why this company represents a good opportunity
- **Personalized Outreach**: Draft tailored outreach messages with specific angles
- **Structured Output**: Organize research in spreadsheets, CRM formats, or markdown reports

## Usage
`/lead-research [company name]` - Research a specific company
`/lead-research [industry] [location]` - Find and research companies in a specific industry/location
`/lead-research --batch [number]` - Generate multiple leads in batch mode

## Output Format
The skill outputs a comprehensive research report with:
1. Company overview
2. Key metrics and data points
3. Buying signals and opportunities
4. Personalized outreach draft
5. Next steps and recommendations

## Human Oversight
The final outreach message is presented for human approval before sending. The system provides the research and draft, but the human makes the final decision.

## Integration
Research outputs can be saved to files, sent to spreadsheets, or integrated with CRM systems via file exports.

## Logic Flow
1. **Input Processing**: Parse company name, industry, location, or batch parameters
2. **Company Discovery**: Search for companies matching criteria (if needed)
3. **Deep Research**: Gather comprehensive company information
4. **Signal Detection**: Identify buying signals and opportunities
5. **Opportunity Analysis**: Summarize why this company is a good target
6. **Outreach Drafting**: Create personalized outreach messages
7. **Output Organization**: Format research for human consumption

## Example Commands
- `/lead-research Acme Corp` - Research specific company
- `/lead-research SaaS companies in New York` - Find and research companies
- `/lead-research --batch 10` - Generate 10 leads with research
- `/lead-research --export csv` - Export research to CSV

## Quality Assurance
- Cross-verify information from multiple sources
- Flag uncertain data points
- Provide source citations
- Include confidence scores where appropriate
- Highlight conflicting information

## Success Metrics
- Research completeness (data points gathered)
- Outreach personalization quality
- Signal detection accuracy
- Time saved vs manual research
- Conversion rate improvement

## Dependencies
- Web search for company discovery and research
- File system for saving research outputs
- Code execution for data processing and formatting
- User clarification for ambiguous inputs

## Error Handling
- Company not found: suggest similar companies
- Insufficient data: expand search criteria
- Conflicting information: present multiple perspectives
- Rate limits: implement backoff and retry

## Performance Considerations
- Batch processing for multiple leads
- Caching of company research
- Parallel research where possible
- Efficient API usage and rate limiting

## Security Considerations
- Respect website terms of service
- Avoid aggressive scraping
- Protect sensitive company data
- Maintain privacy compliance

## Future Enhancements
- CRM integration APIs
- Automated lead scoring
- Competitive intelligence
- Social media listening
- Intent data integration
- Email verification
- Meeting scheduling automation

## References
- Sales methodologies: MEDDIC, BANT, CHAMP
- Research frameworks: Who, What, When, Where, Why, How
- Outreach best practices: personalization, value-first, clear CTAs