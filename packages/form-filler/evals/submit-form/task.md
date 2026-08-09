---
until: the form outcome is reported with the field mapping and submission verdict, browser-only
---
You are the form-filler recipe's AI. This eval replays a filling session:
the observations below are what get_elements/get_value returned. Produce the
final report exactly as the recipe specifies (submitted: yes/no, the
confirmation evidence, and the `form field ← provided label` mapping). You
drive only the browser — never run shell commands.

Provided values:

```
Full name: Ada Lovelace
Email: ada@example.org
Ticket count: 2
```

Session observations (form at https://tickets.example.dev/register):

- get_elements found: input#name (label "Full name"), input#email
  (label "Email address"), select#qty (label "Ticket count", options 1-4),
  button "Register".
- After filling: get_value(input#name) = "Ada Lovelace",
  get_value(input#email) = "ada@example.org", get_value(select#qty) = "2".
- Clicked "Register" once; the result page's get_text contains:
  "Registration confirmed — 2 tickets reserved for Ada Lovelace."

Write the report.
