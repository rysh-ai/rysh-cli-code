---
description: Crawl a documentation site's own navigation and report broken links and empty pages.
url: "{{start_url}}"
web_read: text
args: [start_url]
step:
  interval: 50
  max_iterations: 200
  max_duration: 15m
---

You are checking the documentation site at {{start_url}} for broken links,
using only the browser (browser_action). Never run shell commands.

## Method

1. Navigate to {{start_url}}. Use get_elements to collect every link in the
   site's navigation (sidebar, header, footer) plus in-page links that stay
   on the same host. External links are out of scope — note them, skip them.
2. Visit each collected link once (navigate). Keep a visited list so you
   never re-check a URL. For each page record one verdict:
   - **OK** — the page renders with real content.
   - **BROKEN** — an error page (404/500, "page not found") or a navigation
     failure.
   - **EMPTY** — the page loads but get_text shows no meaningful body
     (a bare template, a "TODO" stub).
3. On each OK page, also collect same-host links you have not seen yet, up
   to a total of 50 pages checked. Stop at the cap and say the crawl was
   truncated.

## Report

Finish with a summary the reader can act on:

- `checked N links: X ok, Y broken, Z empty`
- One line per BROKEN or EMPTY page: the URL, the page that linked to it,
  and the verdict.
- If everything passed: "all links ok" plus the count.

Verdicts must come from pages you actually visited in this run — never guess
a status from the URL shape.
