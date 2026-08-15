# Security policy

## Reporting a vulnerability

**Please do not open a public issue, pull request or discussion for a security
problem.** Rysh runs shell commands and holds provider credentials on a
developer's machine, so a public report is an exploit notice.

Use either of these instead:

1. **GitHub private vulnerability reporting** — the *Security* tab of this
   repository, "Report a vulnerability". This is the preferred path: it is
   private, it threads, and it produces an advisory when the fix ships.
2. **Email [hello@rysh.ai](mailto:hello@rysh.ai)** with `SECURITY` in the
   subject, if you would rather not use GitHub or the option above is not
   available to you.

Include what you would want if you were fixing it: the version
(`rysh --version`), the platform, what an attacker gains, and the shortest
reproduction you have. A proof of concept is welcome and is never required.

**What to expect.** We aim to acknowledge a report within three business days,
and to agree a disclosure timeline with you from there — typically up to 90 days,
sooner when a fix is straightforward. We will tell you when the fix ships and
credit you in the advisory unless you ask us not to.

## Supported versions

Fixes land on the most recent release. There are no long-term support branches,
so "upgrade" is the remediation for anything already released.

Note that the prebuilt `ry` distribution and this repository's `rysh` binary are
**not the same build** and track different versions — say which one you found the
issue in.

## Behaviour that is by design

These are documented properties, not vulnerabilities. Reporting one is not
wasted — but it will be closed with a pointer here, so it is worth checking
first.

- **Rysh executes commands on your machine.** A pane is a real PTY, and an agent
  with tools enabled can run what it decides to run. Prompt content that causes a
  tool call is the product working, not a sandbox escape. A genuine finding here
  looks like a *bypass of a control that is supposed to hold* — a policy gate that
  does not gate, an approval that can be skipped.
- **SecretNAT does not rewrite responses.** Secrets are substituted with tokens in
  the request body before it leaves the machine, and a response carrying a live
  credential in plaintext is reported into the pane. Responses are not rewritten —
  by design.
- **Third-party model providers are outside this boundary.** What a provider does
  with a prompt you sent it is between you and that provider; report those to
  them. What Rysh *sends* is in scope.

## Scope

In scope: the code in these repositories — `rysh-cli-code`, `rysh-cli-shared` and
`rysh-cli-app-code` — and the released artifacts built from them.

Out of scope: the hosted service and website (report those to
[hello@rysh.ai](mailto:hello@rysh.ai) the same way), third-party dependencies
already published elsewhere with a fix available, and findings that require an
attacker to already have local code execution as the user running Rysh.

There is no paid bounty programme at this time.
