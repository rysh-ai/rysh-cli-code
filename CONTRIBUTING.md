# Contributing

Issues and patches are both welcome. Two things about this repository are
unusual, and knowing them first will save you time.

## This repository is generated

`rysh-cli-code`, `rysh-cli-shared` and `rysh-cli-app-code` are **one-way exports**
of a canonical tree that is developed elsewhere. Each release rebuilds these repos
from that tree, so a commit pushed straight here would be overwritten by the next
export rather than kept.

That does not make contributions pointless — it changes where they land:

- **A pull request is read, reviewed and applied to the canonical tree.** It
  reappears here in the next export, as part of a commit that is not yours. You
  are credited in the pull request thread and in the release notes; the git
  history of this repo is not a record of authorship, and cannot be.
- **An issue is the cheaper path for anything you have not already written.**
  Especially for structural changes: the shape of a fix often has to account for
  code that is not exported, and it is better to find that out before you build
  it.

## Which repository

| You want to change | Repository |
| --- | --- |
| the CLI, the TUI, panes, agents, the command surface | `rysh-cli-code` |
| the agentic loop, providers, tools, SecretNAT, wire types | `rysh-cli-shared` |
| the desktop app (Electron + Vite renderer) | `rysh-cli-app-code` |

If you are not sure, open the issue against `rysh-cli-code` and it will be moved.

## Building and testing

The three repos are wired together by
[`rysh-cli-parent`](https://github.com/rysh-ai/rysh-cli-parent), which carries the
Go workspace and the Makefile. Clone that, not this repo alone:

```sh
git clone --recurse-submodules https://github.com/rysh-ai/rysh-cli-parent
cd rysh-cli-parent && make build
```

Within a single Go module you can also work directly:

```sh
go build ./...
go test ./...
go vet ./...
```

The app repo is npm, and its checks are typecheck and vitest:

```sh
npm ci
npm run typecheck
npm run test:run
```

A change that does not pass `go vet` (or `npm run typecheck`) will not be
applied, so run it before you open the pull request.

## What a good patch looks like

- **A test that failed before the change.** Not a test added alongside a fix, but
  one that reproduces the bug first. A test that never went red proves nothing
  about the fix underneath it.
- **One concern per pull request.** A refactor bundled with a fix takes several
  times as long to review and is usually split anyway.
- **The existing style of the file you are in** — naming, error handling, comment
  density. This codebase comments the *why*, not the *what*.
- **No new dependency without saying why in the description.** The CLI is
  installed with `go install` by people who did not pick its dependency tree.

## Licensing of contributions

This project is Apache-2.0 (see [LICENSE](LICENSE) and [NOTICE](NOTICE)), and
contributions are accepted under that same licence — inbound equals outbound. Do
not send code you cannot license that way, including code carrying another
project's copyright.

Files under `internal/vterm/vt10x` are a fork of
[`github.com/hinshun/vt10x`](https://github.com/hinshun/vt10x) and remain **MIT**;
their SPDX headers say so, and a patch there stays MIT.

## Security

Do not open a public issue for a vulnerability. [`SECURITY.md`](SECURITY.md) has
the private reporting path.
