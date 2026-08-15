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

Two builds exist and they are **not the same binary**:

| | this repository | the prebuilt distribution |
| --- | --- | --- |
| binary name | `rysh` | `ry` |
| licence | Apache-2.0 | proprietary |
| how you get it | `go install`, below | Homebrew / curl, below |

**The open-source build, from this repository:**

```sh
go install github.com/rysh-ai/rysh-cli-code/cmd/rysh@latest
```

The binary lands in `$(go env GOPATH)/bin` as `rysh`. Requires Go 1.25.3 or newer.
To build from the parent repo instead:

```sh
git clone --recursive https://github.com/rysh-ai/rysh-cli-parent
cd rysh-cli-parent && make install
```

**The prebuilt distribution.** These are faster, and on macOS and Linux they are
the supported path — but they install `ry`, a **proprietary** build that is not
this repository and tracks a different version. Use them if you want the packaged
product; use `go install` above if you want the open-source one.

```sh
brew install rysh-ai/rysh/ry
```

```sh
curl -fsSL https://packages.rysh.ai/install.sh | sh
```

APT and RPM repositories are served from the same host. The Windows binary is
CLI-only — WSL2 is the supported path on Windows.

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
(conversation, no tools). That is the whole model.

The [parent repo README](https://github.com/rysh-ai/rysh-cli-parent#readme) has
the keybindings, sessions, agents and humanoids.

## Layout

| Path | What |
| --- | --- |
| `cmd/rysh` | the main package — entry point and command surface |
| `internal/tui` | the terminal UI |
| `internal/actors` | workspace / tab / pane / agent actors, proto.actor over NATS |
| `internal/vterm` | terminal emulation, including a vt10x fork with scrollback |
| `internal/provider` | LLM provider adapters |
| `action/` | the `setup-rysh` composite GitHub Action |

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
