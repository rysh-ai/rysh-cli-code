# rysh-cli-code

The [Rysh](https://github.com/rysh-ai/rysh-cli-parent) CLI: an agentic terminal
multiplexer written in Go. Tabs, panes, splits, and `vim`/`htop` working exactly
as you expect — except every pane is also an agent that can answer prompts and
call tools.

This repository holds the CLI itself. It depends on
[`rysh-cli-shared`](https://github.com/rysh-ai/rysh-cli-shared).

**Building or contributing? Start at
[`rysh-cli-parent`](https://github.com/rysh-ai/rysh-cli-parent)** — it carries the
Makefile, the Go workspace, and CI, and wires this module to a local checkout of
the shared one.

## Secrets stay on your machine

<!-- HOSTING DECISION PENDING — DO NOT replace this block with an <img> until a URL is settled.
     The GIF exists and is committed in the private monorepo at
       marketing/assets/videos/secretnat-readme-loop.gif
     but this README is injected into a PUBLIC repo by scripts/export-oss.sh, and that export
     copies prose only — it ships no binary assets. So a relative path cannot resolve here, and
     no public URL for this asset exists yet. rysh.ai already serves ~111 tutorial videos under
     /video-tutorials-assets/, so the hosting mechanism exists; using it needs a deploy, which is
     a founder gate. Substituting a guessed URL would render as a broken image on github.com,
     which is worse than this block. See new_roadmap/archive/epics-2026-08/epic01-launch-readiness.md. -->

SecretNAT is on by default. Secrets are substituted with tokens in the request body
before it leaves the machine, and a response carrying a live credential in plaintext
is reported into the pane. Responses are not rewritten — by design.

The mapping is reversible locally, so `##snat get <token>` hands you back the real
value in your own pane, and the model only ever saw the token.

## Install

Two binaries exist and they are **not interchangeable**:

| | this repository | the prebuilt distribution |
| --- | --- | --- |
| binary name | `rysh` | `ry` |
| how you get it | `go install` | Homebrew, curl, APT, RPM |

The names differ so that one machine can carry both. **Every command below
installs exactly one of them**, and a script, alias or doc written for one does
not run against the other — which is the whole reason the names differ. Check
what you have with `rysh version` or `ry version`.

This repository is the `rysh` half: Apache-2.0, with [LICENSE](LICENSE) and
[NOTICE](NOTICE) beside the source you are reading.

### macOS and Linux

**The open-source build, from source:**

```sh
go install github.com/rysh-ai/rysh-cli-code/cmd/rysh@latest
```

The binary lands in `$(go env GOPATH)/bin` as `rysh`. Requires **Go 1.25.3 or
newer** — the floor declared in this module's `go.mod`.

<!-- WITHDRAWN 2026-08-16, pending an E3 ruling — do not restore without one.
     A prebuilt-download block stood here: this repository's own GitHub Releases carry
     tarballs and Linux packages built from this source, all named `rysh`. It was verified
     end to end (downloaded via the releases/latest alias and RUN: `rysh 0.2.7`), and it is
     the only prebuilt path that installs the OPEN-SOURCE binary rather than `ry`.
     It is withdrawn anyway. designs/024-investor-claims.md, row "The open-source build is
     current", carries guard (1): never point anyone at the GitHub Releases page, which it
     records as frozen at v0.1.4. That premise is now false — real release objects v0.2.6
     and v0.2.7 exist with full asset sets — but only E3 amends 024, and until it does the
     guard stands even though its premise does not. A guard we route around because we
     believe it is stale is not a guard. Restore this block when 024 says so. -->

**The prebuilt distribution — installs `ry`, not `rysh`.** This is the packaged
product, and it is the fastest way to get a working install. It is a separate
distribution with its own channels; nothing in it is produced by this repository,
so `ry --help` describing a command is not evidence about the source here. Use it
if you want the packaged product; use `go install` above if you want the build
whose source you are reading.

Its install commands are deliberately not reproduced here: this README documents
the open-source build. The prebuilt distribution has its own channels — Homebrew,
a curl installer, APT and RPM — and its own documentation.

### Windows

**WSL2 is the supported path.** Native Windows compiles and runs, but it cannot
open a pane — those are two different claims and both are true:

- `GOOS=windows GOARCH=amd64 go build ./...` succeeds — the windows/amd64 target
  builds clean from this source. This repository's `.goreleaser.yml` names that
  archive `cli-only` on purpose, so the artifact says what it is.
- It **cannot start a session.** Rysh's PTY layer has no ConPTY implementation, so
  `platform.PTYSupported` is `false` on Windows and every session-opening command
  is refused up front with WSL guidance — rather than starting a session and then
  failing on your first pane (`cmd/rysh/pty_preflight.go`).
- What it *can* do is the pane-less command set, including talking to a session
  running in WSL from the Windows side: `rysh send`, `rysh exec`, `rysh prompt`,
  `rysh list-sessions`, `rysh install`, `rysh eval`, `rysh doctor`.

For a real session, install WSL2 and use the Linux instructions inside it — the
Linux build runs unmodified:

```powershell
wsl --install
```

```sh
# inside WSL
go install github.com/rysh-ai/rysh-cli-code/cmd/rysh@latest   # rysh
```

Native panes need ConPTY behind the same seam; it is not implemented yet.

### Build from scratch

This module alone builds standalone — it carries no `replace` directive, so the
shared module resolves from its published version:

```sh
git clone https://github.com/rysh-ai/rysh-cli-code
cd rysh-cli-code
go build ./cmd/rysh          # -> ./rysh
```

From the superrepo, which is how to develop against both modules at once — its
`go.work` wires the shared module to the sibling checkout, so an edit there is
picked up without a release round-trip:

```sh
git clone --recursive https://github.com/rysh-ai/rysh-cli-parent
cd rysh-cli-parent
make build       # -> bin/rysh
make install     # -> ~/.local/bin/rysh   (override with PREFIX=)
make test        # both modules
```

Go 1.25.3 or newer either way.

## First run

```sh
export ANTHROPIC_API_KEY=sk-ant-...
rysh onboard --provider anthropic --key-env ANTHROPIC_API_KEY
rysh doctor
```

`onboard` validates the key, writes a project-local `rysh.config.yaml`, and opens
your session. `rysh --help` lists the full command surface.

Each pane has an input mode and `Esc Esc` cycles it: **shell** (a real PTY) →
**prompt** (goes to the LLM) → **rysh** (multiplexer commands) → **chat**
(conversation, no tools). Those four are enabled by default; `##mode list` shows
what a pane has and `##mode new <mode>` adds the rest.

The [parent repo README](https://github.com/rysh-ai/rysh-cli-parent#readme) has
the keybindings, sessions, agents and humanoids.

## Three tours

Commands starting with `##` are typed into a pane in **rysh** mode. From outside
the session, `rysh exec -- '##<cmd>'` runs the same thing and prints its output.

### 1. Create a session

```sh
rysh create work          # create and attach; add -d to leave it detached
rysh list-sessions        # what is running
rysh attach work          # come back to it later
rysh detach work          # leave it running
rysh stop work            # shut the daemon down
```

A session is a daemon plus its panes. It outlives your terminal, so closing the
window detaches rather than kills.

### 2. Tabs, lanes, panes and stacks

A **tab** holds **lanes** (columns); a lane holds **stacks**; a stack holds panes,
Zellij-style — one expanded, the rest collapsed to a title bar showing `[N/M]`.

```
##new tab                 a new tab
##new lane                a new lane (column) in the active tab
##new pane                a pane at the bottom of the active lane
##new grid 4              4 panes stacked in the active lane
##new grid 3x4            3 lanes x 4 panes, in the active tab
##new grid 2x3x4          2 tabs x 3 lanes x 4 panes
##new stack 4             4 more stacked panes in the active stack
```

Look at what you built, and move things around:

```
##tab list                ##lane list           ##pane list
##panegroup layout        the lane layout, whole
##move pane up|down|left|right      reorder in the stack, or cross to the next lane
##move pane <p> to-lane <lane>      put a live pane somewhere else
##tab name build          ##lane name left      ##pane name builder
```

<!-- VIDEO PENDING — the grid. A public URL drops in here as a plain markdown link;
     github.com does not play inline video in a README, so a link (or a GIF served from
     a public URL) are the only two forms that work. DO NOT commit the file: this README
     is injected into a PUBLIC repo by scripts/export-oss.sh, which ships prose only and
     no binary assets, so a relative path cannot resolve. A guessed URL renders as a dead
     link, which is worse than this block. The founder is providing the recording. -->

### 3. The agents board, fleets, and a fleet's own board

Panes can run an interactive **Claude** or **Codex**, and rysh remembers which, so
a session stop/start resumes the same conversation:

```
##claude                  run claude in this pane, resumed automatically
##codex                   run codex in this pane, same deal
##pane new --claude "start on the parser"    a new pane, already working
```

The **agents board** is where those panes talk to each other and to you:

```
##board open              open the board pane
##board post <text>       post a milestone
##board reply <thread> <text>
```

An agent inside a pane posts without stealing focus, and reads back:

```sh
rysh board post --as "$RYSH_PANE" -- 'parser wiring done'
rysh board tail --limit 20
```

`##board agent up` starts a **board agent** — a hidden pane running Claude that
sits in the path, routes messages between agents, and refuses a request it knows
is stale. `##board agent visible` draws it without moving your focus;
`##board agent invisible` puts it away while it keeps running.

A **fleet** is a named group of panes with a board of its own. Registering one
opens no panes — it is the cheap half:

```
##fleet register epic-07 --board epic-07
##fleet state epic-07 up
##fleet list              ##fleet show epic-07
##fleet forget epic-07    drop it; the panes keep running
```

```sh
rysh board tail --fleet epic-07     # that fleet's stream, not the session's
```

A fleet may be claudes, codexes, or both — the commands are the same either way.

<!-- VIDEOS PENDING — two of them, from the founder: (a) the agents board with several
     agent panes posting and a board agent routing; (b) a fleet coming up, working, and
     its own board read back with `rysh board tail --fleet`. Same rules as above: a public
     URL as a plain link, never a committed file and never a guessed URL. -->

## Layout

| Path | What |
| --- | --- |
| `cmd/rysh` | the main package — entry point and command surface |
| `internal/tui` | the terminal UI |
| `internal/actors` | workspace / tab / pane / agent actors, proto.actor over NATS |
| `internal/vterm` | terminal emulation, including a vt10x fork with scrollback |
| `internal/provider` | LLM provider adapters |
| `internal/platform` | host capabilities that change what rysh can do (e.g. PTY support) |
| `action/` | the `setup-rysh` composite GitHub Action |

## Contributing and security

**This repository is a one-way export** of a tree developed elsewhere, so a commit
pushed straight here is overwritten by the next export rather than kept. That does
not make patches pointless — [CONTRIBUTING.md](CONTRIBUTING.md) explains where one
actually lands.

**Found a vulnerability? Do not open a public issue, PR or discussion.** Rysh runs
shell commands and holds provider credentials on a developer's machine, so a public
report is an exploit notice. [SECURITY.md](SECURITY.md) has the private channels.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
