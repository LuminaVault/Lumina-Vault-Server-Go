---
name: plan
description: Plan mode for Hermes — inspect context, write a markdown plan into the active workspace's `.hermes/plans/` directory, and do not execute the work.
version: 1.0.0
author: Hermes Agent
license: MIT
metadata:
  hermes:
    tags: [planning, plan-mode, implementation, workflow]
    related_skills: [writing-plans, subagent-driven-development]
---

# Plan Mode

Use this skill when the user wants a plan instead of execution.

## Core behavior

For this turn, you are planning only.

- Do not implement code.
- Do not edit project files except the plan markdown file.
- Do not run mutating terminal commands, commit, push, or perform external actions.
- You may inspect the repo or other context with read-only commands/tools when needed.
- Your deliverable is a markdown plan saved inside the active workspace under `.hermes/plans/`.

## Output requirements

Write a markdown plan that is concrete and actionable.

Include, when relevant:
- Goal
- Current context / assumptions
- Proposed approach
- Step-by-step plan
- Files likely to change
- Tests / validation
- Risks, tradeoffs, and open questions

If the task is code-related, include exact file paths, likely test targets, and verification steps.

### Cross-platform readiness check (fanapi + fandemicapp-ios)

When the feature spans both the Go backend and the iOS frontend, inspect the backend **first**:

1. Check the database schema for tables, columns, or enum types that already support the feature.
2. Check API request/response models and handlers to see if the endpoint already accepts or returns the data.
3. Check existing DB query functions for ordering, batching, or attachment logic.
4. **For third-party API integrations**, verify whether the external API is user-local (e.g., IBKR Client Portal on localhost) or cloud-reachable. If local, a global backend env var cannot serve multiple users — decide early between per-user proxy URLs (backend reaches proxy) or device-direct calls (iOS reaches proxy).
5. **For background jobs**, check if the project uses ad-hoc `Task.sleep(nanoseconds:)` loops. Plan migration to `Task.sleep(for:)` and verify whether the job context provides `Request` (it usually does not), which may require refactoring services to accept `Client` instead.

If the backend already supports the capability, explicitly call this out in the plan and scope the work to frontend-only. Only include backend steps if the inspection reveals actual gaps.

## Save location

Save the plan with `write_file` under:
- `.hermes/plans/YYYY-MM-DD_HHMMSS-<slug>.md`

Treat that as relative to the active working directory / backend workspace. Hermes file tools are backend-aware, so using this relative path keeps the plan with the workspace on local, docker, ssh, modal, and daytona backends.

If the runtime provides a specific target path, use that exact path.
If not, create a sensible timestamped filename yourself under `.hermes/plans/`.

## Interaction style

- If the request is clear enough, write the plan directly.
- If no explicit instruction accompanies `/plan`, infer the task from the current conversation context.
- If it is genuinely underspecified, ask a brief clarifying question instead of guessing.
- After saving the plan, reply briefly with what you planned and the saved path.
