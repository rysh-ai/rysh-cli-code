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

## Install

```sh
go install github.com/rysh-ai/rysh-cli-code@latest
```

The binary lands in `$(go env GOPATH)/bin` as `rysh-cli-code`; rename it to
`rysh` for the short command. To build from source instead:

```sh
git clone --recursive https://github.com/rysh-ai/rysh-cli-parent
cd rysh-cli-parent && make install
```

Requires Go 1.25.3 or newer.

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
| `main.go`, `*_cmd.go` | command surface |
| `internal/tui` | the terminal UI |
| `internal/actors` | workspace / tab / pane / agent actors, proto.actor over NATS |
| `internal/vterm` | terminal emulation, including a vt10x fork with scrollback |
| `internal/provider` | LLM provider adapters |
| `action/` | the `setup-rysh` composite GitHub Action |

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
