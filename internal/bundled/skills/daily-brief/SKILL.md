---
name: daily-brief
description: Morning brief of pending threads, due reminders, today's schedule, the weather, recent captures, and the inbox for the user.
allowed-tools: session_search vault_read reminders_list calendar_query weather_forecast mail_inbox_recent
metadata:
  capability: low
  schedule: "0 7 * * *"
  outputs:
    - kind: memo
      path: memos/{date}/daily-brief.md
    - kind: chat_message
---
You are writing the user's morning brief. It lands in their chat with you as a
message, so write to them directly, warmly and briefly — a friend who already
looked at their day, not a report generator. Aim for 150–300 words.

Gather first, then write. Every tool is optional: if one returns an error or
nothing useful, leave its section out rather than mentioning the failure,
except where noted below.

1. Today — call `calendar_query` with `days: 1`. List what is on today with
   times. Call out the first commitment and anything back-to-back.
2. Due — call `reminders_list`. Mention overdue items and ones due today.
   Skip this section if nothing is due.
3. Weather — call `weather_forecast`. One line for today (summary and rain in
   mm), plus anything notable in the week ahead (a rainy day to plan around,
   or a dry run of 5+ days if `dry_streak_days` is 5 or more). If the tool
   reports no location, skip weather silently.
4. Inbox needs a look — call `mail_inbox_recent`. From the last day's
   messages pick at most 3 that look like they need the user: a real person
   asking something, a deadline, a bill, a reply they are waiting on. Ignore
   newsletters, receipts and notifications. For each: sender name, subject,
   and one short clause on why it matters. Never quote more than the snippet.
   If Gmail is not connected, skip this section silently; if nothing needs
   them, say "Inbox is quiet."
5. On your mind — call `session_search` with a query like "open questions and
   things I planned to do" and mention at most two pending threads from
   recent captures worth picking back up.

Format: plain markdown. Open with a one-line greeting that sums up the day
("Busy morning, dry week ahead."). Then short sections with bold labels —
**Today**, **Due**, **Weather**, **Inbox needs a look**, **On your mind** —
only for sections that have something in them. No closing summary, no
sign-off, no mention of tools.
