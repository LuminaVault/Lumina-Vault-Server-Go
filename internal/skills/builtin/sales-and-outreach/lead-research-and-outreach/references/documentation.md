# Lead Research System Documentation

## Overview
The lead research system automates the process of finding, researching, and preparing outreach for potential customers. It combines web research, data analysis, and personalized messaging to help sales teams work more efficiently.

## Components

### 1. Lead Research Script (`scripts/lead_research.py`)
- Researches individual companies
- Gathers comprehensive company information
- Generates research reports
- Exports data to CSV

### 2. Batch Research Script (`scripts/lead_research_batch.py`)
- Discovers multiple companies in a niche
- Researches each company comprehensively
- Generates personalized outreach for each lead
- Outputs batch reports and summary CSVs

### 3. Templates (`references/templates.yaml`)
- Defines research report structure
- Provides outreach message templates
- Specifies CSV export formats

### 4. Configuration (`references/config.yaml`)
- Niche definitions and criteria
- Search parameters and filters
- Output format specifications

## Workflow

### Single Lead Research
```
Input → Company Name
  ↓
Discovery → (Optional) Find similar companies
  ↓
Research → Gather company data from web sources
  ↓
Analysis → Identify opportunities and signals
  ↓
Outreach → Generate personalized message
  ↓
Output → Research report + outreach draft
```

### Batch Lead Generation
```
Input → Niche + Quantity
  ↓
Discovery → Find companies matching niche criteria
  ↓
Research → Process each company
  ↓
Analysis → Evaluate opportunities
  ↓
Outreach → Create personalized messages
  ↓
Output → Multiple reports + summary CSV
```

## Data Sources

The system uses multiple web sources to gather comprehensive company information:

1. **Company Websites**: Official information and product details
2. **News Sources**: Recent announcements and press releases
3. **Social Media**: LinkedIn, Twitter for company updates
4. **Funding Databases**: Crunchbase, PitchBook for financial data
5. **Job Listings**: Hiring trends and growth indicators

## Output Formats

### Markdown Reports
- Comprehensive research documentation
- Structured for easy reading and editing
- Includes next steps and recommendations

### CSV Exports
- Structured data for spreadsheet analysis
- Compatible with CRM systems
- Includes key metrics and outreach angles

### JSON Data
- Machine-readable format for API integration
- Contains all raw research data
- Ready for further processing

## Quality Assurance

### Data Verification
- Cross-references multiple sources
- Flags conflicting information
- Provides source citations

### Personalization Quality
- Uses company-specific details
- References recent events and achievements
- Tailors value proposition to company needs

### Human Review
- All outreach messages require human approval
- Research reports include confidence scores
- Flagged items need manual verification

## Success Metrics

### Efficiency Metrics
- **Time per lead**: vs manual research
- **Research completeness**: % of data points gathered
- **Automation rate**: % of process automated

### Effectiveness Metrics
- **Response rate**: % of outreach that gets responses
- **Meeting rate**: % that leads to sales meetings
- **Conversion rate**: % that become customers

## Integration Points

### CRM Systems
- CSV import for lead data
- API endpoints for direct integration
- Webhook support for real-time updates

### Email Platforms
- Mailgun, SendGrid, Amazon SES integration
- Template-based email sending
- Tracking and analytics

### Analytics Tools
- Google Analytics for web research
- Mixpanel for user behavior tracking
- Custom dashboards for performance metrics

## Future Development

### Phase 2 Enhancements
- **AI Signal Detection**: Automatically identify buying signals
- **Competitive Intelligence**: Track competitor activities
- **Intent Data**: Integrate third-party intent signals
- **Email Verification**: Verify contact information automatically
- **Meeting Scheduling**: Direct calendar integration

### Phase 3 Enhancements
- **Predictive Scoring**: AI-powered lead scoring
- **Multi-channel Outreach**: Beyond email to social, phone, etc.
- **Account-Based Marketing**: Personalized campaigns for target accounts
- **Sales Intelligence**: Advanced analytics and insights

## Getting Started

1. **Install the skill**: `hermes skills install lead-research-and-outreach`
2. **Configure niches**: Edit `references/config/niches.yaml`
3. **Customize templates**: Edit `references/templates/outreach.yaml`
4. **Test with sample**: Run `python3 scripts/lead_research.py "Test Company"`
5. **Scale up**: Use batch mode for production leads

## Support
For issues and enhancements, contact the Hermes Agent support team or contribute to the skill repository.

---
*Last updated: 2026-05-06*
*Skill Version: 1.0*