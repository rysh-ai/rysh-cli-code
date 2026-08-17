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

## Video tutorials

Five narrated, subtitled walkthroughs of what rysh actually does, in order —
**each one assumes the previous**. The first four are recorded against the build
the [install](#install) line below gives you, so every command in them is one you
can type.

| # | Video | What it teaches |
| --- | --- | --- |
| 1 | **[Install, and your first tabs, lanes and stacked panes](https://www.youtube.com/watch?v=1k0nEMBtY04)** | `go install`, a first session, a tab, a second lane, a stack of three panes — then `##claude` in each pane of that stack, so one window holds three independent Claude sessions |
| 2 | **[Stop the session, start it again — the agents are still there](https://www.youtube.com/watch?v=PjpiuyXCgpA)** | Why a session is a daemon and not a terminal. Claude and Codex are each told a codeword, the session is stopped *outright*, and after the restart both resume unprompted and answer with the codeword they were told before it |
| 3 | **[Claude and Codex in three stacked panes at once](https://www.youtube.com/watch?v=JWl5l-YswzI)** | Running more than one agent, and more than one vendor, side by side: `Ctrl+S` and a digit to move around a stack, a task each, three files written in parallel |
| 4 | **[A fleet that talks to itself, on a shared board](https://www.youtube.com/watch?v=GwZZeZ1LEcw)** | `##board open`, `rysh ansa prompt`, and agents driving *each other*: `roadmap` (Claude) sets the goal, `fleet-manager` (Codex) splits it into work orders, two workers build in parallel, and the manager sends a correction back to a worker before signing off. They ship a browser todo manager — HTML, CSS and JavaScript, no backend, no build step |
| 5 | **[Graph engineering — designing the shape of a fleet](https://www.youtube.com/watch?v=70cb15gaWtI)** | The deep end: why an agent org chart is a graph, what the edges mean, and what changes when you remove one. Orders travel *down* as messages, results come *up* on the board, and two workers with no edge between them give independent verdicts |

Start at 1 even if you have used tmux or Zellij for years — the panes look
familiar and the session model underneath them does not, which is video 2.

<!-- The five links above are live and public (verified via YouTube's oembed endpoint,
     2026-08-17: all five returned 200 with matching titles). They are watch URLs, not
     studio.youtube.com URLs — a Studio link is the upload console and opens for nobody
     but the channel owner.

     Sources. Videos 1-4: video-tutorials/demos/readme-quickstart/ in the private
     monorepo — a tape and a narration script per demo, make-demo.sh to rebuild, and
     out/ holding each master plus a .srt attached to the upload as the caption track.
     Video 5 is a different pipeline, marketing/assets/videos/graph-engineering/, and
     is bound by new_roadmap/designs/024-investor-claims.md: read that before editing
     its row above. Two constraints from it that bear on this page — the ceo/manager/
     worker driver is in-house tooling on shipped primitives and not a shipped
     orchestrator, and no agent count is a capability claim. The row above is written
     to state neither.

     Video 5 was also filmed with a local build rather than the install line, so it is
     the one row that must not be described as "recorded against the build you get" —
     hence the wording of the paragraph above it. -->

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
| how you get it | `go install`, or the installer below | its own channels, not documented here |

The names differ so that one machine can carry both. **Every install command in
this README installs `rysh`**, and a script, alias or doc written for one does
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

**The open-source build, prebuilt:**

```sh
curl -fsSL https://packages.rysh.ai/install-rysh.sh | sh
```

Installs `rysh` from this repository's own releases — the Apache-2.0 binary, not
`ry`. It resolves the newest release at run time; set `RYSH_VERSION` to pin one
instead, or `RYSH_INSTALL_DIR` to choose where it lands.

<!-- RESTORED 2026-08-16 by founder ruling, WITHOUT an amendment to 024 — read this
     before citing either document, because they disagree on purpose.

     This block was withdrawn earlier today against designs/024-investor-claims.md,
     row "The open-source build is current", guard (1): "never point anyone at the
     GitHub Releases page", recorded there as frozen at v0.1.4.

     The premise is measurably false as of 2026-08-16: rysh-cli-code carries real
     release objects v0.2.6, v0.2.7 and v0.2.9, ten assets each; /releases/latest
     resolves to v0.2.9; the tags API serves v0.2.9. The cause of the flip is known —
     the guard was written when the export published TAGS WITHOUT RELEASE OBJECTS,
     and the OSS release workflow now publishes both.

     The founder was shown that measurement, asked to amend guard (1), and ruled to
     add this block WITHOUT amending it. So: 024 guard (1) STILL STANDS, UNAMENDED,
     and this block is a knowing exception to it — not evidence that the guard was
     satisfied, retired, or quietly overtaken. Only E3 amends 024.

     Guards (2) and (3) of that row are untouched and still bind: the public tree is
     an export run by hand, not a mirror (E-2 is open), and nothing here implies
     development happens in the open.

     One narrowing that is true but was NOT the basis of the ruling: this points at
     packages.rysh.ai/install-rysh.sh, and the installer resolves the release at run
     time, so no version can go stale in this file the way a Releases link or a
     "latest release" badge can. -->

**The prebuilt distribution — installs `ry`, not `rysh`.** It is the packaged
product and a separate distribution; nothing in it is produced by this repository,
so `ry --help` describing a command is not evidence about the source here. Use
`go install` above if you want the build whose source you are reading.

Its install commands are deliberately not reproduced here: this README documents
the open-source build. The prebuilt distribution has its own channels and its own
documentation.

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

A stack with an agent in every pane:
**[Claude and Codex in three stacked panes at once](https://www.youtube.com/watch?v=JWl5l-YswzI)**.

<!-- Same placeholder as the "Video tutorials" table at the top — JWl5l-YswzI
     is the
     same video and must be swapped in both places. A plain markdown link is the form to
     use: github.com does not play inline video in a README, and this file is injected
     into a PUBLIC repo by scripts/export-oss.sh, which ships prose only and no binary
     assets, so a committed file or a relative path cannot resolve here. -->

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
