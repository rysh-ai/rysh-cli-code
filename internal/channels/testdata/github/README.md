# GitHub webhook fixtures (synthetic)

These payloads are **written by hand to GitHub's documented webhook payload
schema** (issue_comment, pull_request_review_comment, issues, ping). They were
**not captured from a live GitHub App or repository**: gap04 (root-order-074)
builds against a local fake GitHub only, with no real App, no token and no `gh`
call. Only the fields the `github` channel adapter reads are present; every
id, login, URL and repository is invented (`acme/widgets`, `alice`, `mallory`).

Tests copy a fixture, change one field, and sign the result with a throwaway
secret generated inside the test.
