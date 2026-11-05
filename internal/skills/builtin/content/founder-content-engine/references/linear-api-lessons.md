# Linear API Lessons Learned

## Key Discoveries from This Session

### 1. Project Due Date Limitation
**Critical Finding:** Linear projects do NOT have a `dueDate` field in the GraphQL schema.

**Evidence:**
- Querying a project with `dueDate` field returns error: "Cannot query field \"dueDate\" on type \"Project\"."
- No alternative date fields exist on the Project type
- This impacts milestone tracking and roadmap visualization

**Workaround:** Use project descriptions to note target dates, or maintain external roadmaps in tools like Notion or Coda that link to Linear projects.

### 2. Custom View Creation Complexity
Creating Notion-style custom dashboards via API is non-trivial:

- Filter structures are complex and poorly documented
- Custom views created via API may not appear in the Linear UI as expected
- Best practice: Create custom views manually in the Linear UI
- For programmatic filtering, use simple `issueSearch` queries with text filters

### 3. Authentication Flexibility
Contrary to official documentation, the Linear API may accept raw tokens without the "ApiToken" prefix:

**Test first with a simple query** to verify which format works for your account:
```bash
curl -s -X POST https://api.linear.app/graphql \
  -H "Authorization: $LINEAR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"query": "{ viewer { id name } }"}' | python3 -m json.tool
```

If this fails, try with the full prefix:
```bash
curl -s -X POST https://api.linear.app/graphql \
  -H "Authorization: ApiToken $LINEAR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"query": "{ viewer { id name } }"}' | python3 -m json.tool
```

### 4. Terminal vs Python for Complex Queries
For complex GraphQL queries with many variables, Python's `requests` library can be more reliable than curl in the terminal due to quoting/escaping issues.

**Example using Python:**
```python
import json
import requests

linear_api_key = "lin_api_YourActualTokenHere"
url = "https://api.linear.app/graphql"
headers = {
    "Authorization": linear_api_key,
    "Content-Type": "application/json"
}

mutation = '''
mutation($input: IssueCreateInput!) {
  issueCreate(input: $input) {
    success
    issue {
      identifier
    }
  }
}
'''

variables = {
    "input": {
        "teamId": "f7d9e774-bcc5-4cdd-a8ea-20e61a570586",
        "title": "Test Issue",
        "description": "Testing issue creation",
        "priority": 2,
        "stateId": "561b7c8b-cf3a-4e1d-8a82-cbf83656aa80"
    }
}

response = requests.post(
    url, 
    headers=headers, 
    json={"query": mutation, "variables": variables}
)

if response.status_code == 200:
    result = response.json()
    if result["data"]["issueCreate"]["success"]:
        print(f"Created: {result['data']['issueCreate']['issue']['identifier']}")
    else:
        print(f"Failed: {result}")
else:
    print(f"HTTP {response.status_code}: {response.text}")
```

### 5. Bulk Operations Best Practices

#### Rate Limiting Awareness
- Linear allows 5,000 requests/hour per API key
- Monitor `X-RateLimit-Requests-Remaining` response header
- Implement exponential backoff when rate limit approaches

#### Batch Processing
- Create issues in batches of 5-10 to avoid hitting rate limits
- Use `try/except` blocks to handle individual failures
- Implement retry logic with exponential backoff for failed requests
- Save progress after each successful batch

#### Error Handling
- Always check the `errors` array in GraphQL responses (HTTP 200 can still contain errors)
- Log failed issue creations with error details
- Implement a retry queue for failed issues
- Consider using a temporary file to track progress during bulk operations

### 6. Project Assignment vs. Issue Assignment
- **Projects:** Use `projectId` (singular) when updating an issue to add it to a project
- **Issues:** Use `assigneeId` to assign to a user
- **Bulk operations:** Always verify UUID formats and team IDs before bulk creation

### 7. Important UUID Notes
- Ensure all UUIDs are properly formatted (32 hex chars with hyphens)
- Use `String!` for ID variables in GraphQL, not `ID!`
- Both UUIDs and short identifiers (e.g., `ENG-123`) work for `issue(id:)`

### 8. Workflow State Management
To change an issue's status, you need the `stateId` (UUID) of the target state — query workflow states first for the team.

## Session Statistics
- Created 23 MVP tickets (HER-5 to HER-27)
- Created 16 Post-MVP tickets (HER-41 to HER-56)
- Total: 39 tickets created successfully
- Projects created: 6 (Backend - Hummingbird, Backend - Hosting/Deployment, iOS Client, Backend - Post-MVP, Hosting/Deployment - Post-MVP, iOS Client - Post-MVP)

## References
- Linear GraphQL API Documentation: https://linear.app/docs/graphql
- HermesVault Kanban: `/opt/data/home/obsidian-vault/Vault/Kanban/`
- Linear Board: https://linear.app/hermiesvault
- Practical Linear Usage: `/root/.hermes/skills/productivity/linear/references/practical-linear-usage.md`