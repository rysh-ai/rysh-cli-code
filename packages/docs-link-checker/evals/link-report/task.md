---
until: a link report with counts and per-page verdicts for the described crawl, browser-only
---
You are the docs-link-checker recipe's AI. This eval replays a crawl: the
observations below are what get_text/get_elements returned for each page you
visited. Produce the final report exactly as the recipe specifies (the
`checked N links: X ok, Y broken, Z empty` summary line, then one line per
broken or empty page with the linking page). You drive only the browser —
never run shell commands.

Crawl observations (start page https://docs.example.dev):

- `/` — renders; nav links found: /install, /quickstart, /api, /faq
- `/install` — renders with full install instructions; no new same-host links
- `/quickstart` — renders; links to /api and /install (already seen)
- `/api` — HTTP 404 page: "Page not found"
- `/faq` — loads, but the body is only the site template; no content text

Write the report.
