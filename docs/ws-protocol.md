# The `/ws` protocol

The contract between the Go server in `rysh-cli/internal/web` and every client that
speaks to it: the React renderer in `rysh-cli-app/src` (which is also the browser UI —
`vite.web.config.ts` builds it into `internal/web/static`), the mobile client, and any
future consumer such as the VS Code extension.

**This document is enforced.** `internal/web/ws_protocol_test.go` extracts the real wire
surface from the server's own source and fails when it stops matching
`internal/web/testdata/ws_protocol.golden.json` or when a name here goes missing. If you
are reading this because that test failed, go to
[§6 Changing the protocol](#6-changing-the-protocol).

Why it exists: on 2026-08-11 commit `a712be3` added `source_width` and `source_height`
to `webpane_frame`. The TypeScript `WebPaneFrame` kept its four fields and silently
dropped the two, so the web pane had no way to hit-test forwarded input against the
image it was displaying. Nothing failed, nothing warned. A Go server and several
TypeScript clients share no type system, so the only thing that can notice a one-sided
change is a test that reads the server's source — which is what guards this file now.

---

## 1. Transport

| | |
|---|---|
| Endpoint | `GET /ws` (`server.go:313`), upgraded by `gorilla/websocket` |
| Auth | the same login that guards `/api/*` and `/fs/*` — `Authorization: Bearer <jwt>` or `?token=<jwt>` on the URL (`auth.go:209`) |
| Query flag | `?stream=1` marks a **content-plane client** (see §1.2) |
| Inbound frame limit | 32 MB — `browser_result` carries a base64 screenshot and the old 64 KB limit closed the socket (`hub.go:200-204`) |
| Keepalive | server pings; the client's pong resets the read deadline |

### 1.1 Envelope

Every frame in both directions is a JSON object with a `type` discriminator.

**Server → client** — `type` plus a `data` payload:

```json
{ "type": "webpane_frame", "data": { "pane_id": "p3", "...": "..." } }
```

`snapshot` is the one message with an extra top-level key, `layout_only` (bool).

**Client → server** — always the literal type `"command"`; the verb is `data.action`
and its arguments are `data.params` (`hub.go:225-236`):

```json
{ "type": "command", "data": { "action": "submit_input", "params": { "text": "ls", "mode": "shell" } } }
```

A frame whose `type` is not `"command"` is **silently ignored**. So is a command whose
`action` matches no case, and any command whose `params` fail to unmarshal — the server
never NAKs. A client cannot tell a typo from a valid no-op, which is the second reason
this document exists.

### 1.2 Who receives what

- **`?stream=1` (content plane)** — receives `pane_output`, `pane_vt`, `pane_content`,
  `pipeline_output`, `webpane_frame`. Seeded on connect with a layout-only `snapshot`
  followed by `pane_content` batches (`seed.go`), because one 17 MB snapshot could not
  be written inside the write deadline over a tunnel.
- **Non-stream clients** receive the full `snapshot`.
- Everything else is broadcast to all clients, except `completion_result` and
  `clipboard_content`, which are returned **only to the client that asked**. A broadcast
  would leak one user's input line, or a whole pane buffer, to every connected viewer.

---

## 2. Server → client messages

29 messages. Types are the JSON types on the wire.

### 2.1 Panes and content

**`snapshot`** — the full workspace. Envelope carries `layout_only` (bool) beside
`type` and `data`; `data` is the Go `msg.WorkspaceSnapshot`, whose fields are governed
by that type, not by this document.

**`pane_content`** — seed batches for a stream client.

| field | type |
|---|---|
| `panes` | array of pane seed objects (`seed.go` `paneSeed`: `pane_id`, `output`, `ai_output`, `rysh_output`, `chat_output`, `external_output`, `mode_outputs`, `prompt_history`, `shell_history`) |

**`pane_output`** — one chunk of pane output.

| field | type | notes |
|---|---|---|
| `pane_id` | string | |
| `mode` | string | which output plane the chunk belongs to |
| `text` | string | |
| `turn_id` | string | the orchestrator run a chat chunk belongs to; empty for non-conversation output |

**`pane_vt`** — the VT screen of an interactive pane. **Three build sites, and they do
not all send the same fields.** Only these three are guaranteed:

| field | type |
|---|---|
| `pane_id` | string |
| `raw_mode` | bool |
| `remote_interactive` | bool |

These six are **optional** — the clearing frame pushed when a pane stops being
interactive (`server.go:1991`) omits them, and a client that dereferences them
unconditionally will read `undefined`:

| field | type |
|---|---|
| `vt_screen` | array of string |
| `vt_cursor_row` | int |
| `vt_cursor_col` | int |
| `remote_vt_screen` | array of string (may be `null`) |
| `remote_vt_cursor_row` | int |
| `remote_vt_cursor_col` | int |

**`pipeline_output`** — `tab_id` (string), `text` (string).

### 2.2 Web pane (server-side headless browser)

**`webpane_frame`** — one rendered frame.

| field | type | notes |
|---|---|---|
| `pane_id` | string | |
| `url` | string | |
| `title` | string | |
| `screenshot` | string | base64 JPEG, quality 55 |
| `source_width` | float64 | **hit-test against this, not the displayed `<img>` width** |
| `source_height` | float64 | 0 when the server could not determine a size — do not divide by it unchecked |

`source_width`/`source_height` are the browser viewport the screenshot was taken at.
Forwarded input (`webpane_input`) is mapped per-axis from the client's display size into
these, so a client that does not send its own display size, or ignores these, puts
clicks in the wrong place. This is the pair that drifted.

**`webpane_error`** — `pane_id` (string), `error` (string). Pushed on **every** web-pane
failure path, including a malformed `webpane_input`, so a dropped event is always
visible rather than silently lost.

**`web_activate`** — `pane_id`, `url`, `profile` (all string). Ask the *desktop* client to
show its embedded view.
**`web_deactivate`** — `pane_id` (string).
**`web_prompt`** — `pane_id` (string), `prompt` (string).
**`import_cookies`** — `profile` (string), `cookies` (array).

### 2.3 Approvals and browser actions

**`approval_request`** — a pane's gated tool is waiting on a human.

| field | type |
|---|---|
| `pane_id` | string |
| `request` | object — the Go `msg.MsgApprovalRequest` |

Answered by the `approval_response` command (§3.3).

**`approval_error`** — `pane_id` (string), `request_id` (string), `error` (string). Pushed
on **every** rejected `approval_response`: a malformed payload, a missing `pane_id`, or a
decision the server does not recognise. The answering client is whichever one has the
dialog open, so like `webpane_error` this is a broadcast; correlate on `request_id`.

A dropped approval used to be silent, which on a phone is indistinguishable from a slow
one — the dialog closes, the user believes they answered, and the tool blocks until
`waitForApproval`'s five-minute timeout turns it into a rejection.

**`browser_action`** — a pane's `browser_action` tool wants the client's browser to do
something.

| field | type |
|---|---|
| `pane_id` | string |
| `request` | object — the Go `msg.MsgBrowserActionRequest` |

Answered by the `browser_result` command, which the server republishes on the pane's
`browser.response` subject.

### 2.4 Completion

**`completion_result`** — reply to `completion_get`, sent to the requesting client only.

| field | type |
|---|---|
| `request_id` | string — correlates with the request |
| `pane_id` | string |
| `candidates` | array of `tui.ShellCompletionCandidate` |

### 2.5 Agents, humanoids, shares

**`agent_list`** — `data` is the agent array (`msg.MsgAgentListReply.Agents`).
**`humanoid_list`** — `data` is the humanoid array.
**`share_list`** — `data` is the share array.

Their element fields are governed by the Go types, not by this document.

### 2.6 Email and WhatsApp (humanoid inboxes)

| message | fields |
|---|---|
| `email_list` | `humanoid_name` (string), `emails` (array), `err` (string) |
| `email_detail` | `humanoid_name` (string), `email` (object), `err` (string) |
| `email_inbox_changed` | `humanoid_name` (string) |
| `whatsapp_list` | `humanoid_name` (string), `messages` (array), `err` (string) |
| `whatsapp_detail` | `humanoid_name` (string), `message` (object), `err` (string) |
| `whatsapp_inbox_changed` | `humanoid_name` (string) |

`err` is a string, empty on success — not an absent key.

### 2.7 Control dashboard and pairing (design 005)

**`control_status`** — `control` (bool, whether this server accepts mutating control
actions), `session` (string).

**`pairing_list`** — `humanoid_name` (string), `pending` (array), `allowlist` (array),
and **optionally** `channel` (string): the reply to the `pairing_list` command carries
it, the variant forwarded from NATS does not.

**`pairing_request`**, **`pairing_qr`**, **`pairing_status`** — `data` is the
corresponding Go message (`msg.MsgChannelPairRequest`, `MsgChannelPairQR`,
`MsgChannelPairStatus`).

### 2.8 Clipboard

**`clipboard_content`** — reply to `clipboard_copy` (§3.11), sent to the requesting
client **only**, for the same reason `completion_result` is: a pane buffer holds whatever
that pane printed, and a broadcast would hand one viewer's copy to every other connected
client.

| field | type | notes |
|---|---|---|
| `request_id` | string | echoed from the request; correlate on it |
| `pane_id` | string | echoed from the request |
| `source` | string | the buffer actually read, with the default resolved — a request that omitted `source` gets `"output"` back, not `""` |
| `text` | string | the buffer contents, capped (see below) |
| `truncated` | bool | true when `text` is a tail of a larger buffer |
| `err` | string | empty on success; set instead of failing silently |

`text` is capped at **256 KB**, matching `seedBatchMaxBytes` and for the same reason:
gorilla's 10s write deadline applies to the whole write, and over a tunnel measured at
95 KB/s a larger message misses it and takes the connection down. A pane buffer has no
such bound — one was measured at 682 KB — so the cap is not optional. When it bites, the
**tail** survives: recent output is what someone copying from a pane is asking for.

### 2.9 Agents board

**`board_result`** — reply to `board_get` (§3.12), sent to the requesting client
**only**. The privacy argument is stronger here than for `clipboard_content`: a board id
names a **fleet**, and the reply carries the `pane_id` the asking window keys by — so a
broadcast would paint one fleet's board into another fleet's pane, in a window that had
asked about something else entirely.

| field | type | notes |
|---|---|---|
| `request_id` | string | echoed from the request; correlate on it |
| `pane_id` | string | echoed from the request |
| `board` | string | the board actually read, **with the pane's meta resolved** — an empty or unusable `board.id` comes back as `session`, not as `""` |
| `threads` | array | thread objects; **absent entirely when `error` is set** |
| `roster` | array | `{pane_id, persona, ts}` for each agent that announced itself |
| `stats` | object | `{threads, provisional, posts, duplicates, evicted, unknown_version}` — counters for the WHOLE board, so a windowed answer can say what it is not showing |
| `filtered` | int | threads dropped by `since` |
| `withheld` | int | threads dropped by `limit` |
| `roster_reconciled` | bool | false ⇒ the roster is served **as recorded** and may list panes that have since closed |
| `error` | string | set instead of an answer; never alongside one |
| `no_recorder` | bool | the recorder did not answer, as opposed to a bug in the request |

A thread is `{key, root, replies, provisional}`; `root` is `null` while the thread is
**provisional** (replies that arrived before their root — expected, not an error, because
thread ids are minted agent-side with no round trip). A post is
`{pane_id, persona, kind, text, ts, thread_id?, to_persona?, to_pane_id?}`. `pane_id` is
the identity: a persona is unique per **lane**, not per session, so two agents may
legitimately display the same name.

**`error` and `threads` are mutually exclusive, and a client must keep them that way.**
An unanswered query rendered as an empty thread list is a clean, confident, empty board —
which is exactly what a *quiet* board looks like. Only one of those two is a reason to go
find the daemon, and a client that flattens them cannot tell you which one you are
looking at.

---

## 3. Client → server commands

86 actions. All are sent as `{"type":"command","data":{"action":…,"params":…}}`. An
action with no parameters takes no `params` at all.

### 3.1 Layout — tabs, panes, focus

| action | params |
|---|---|
| `create_tab`, `create_pane`, `create_pane_down`, `create_stacked_pane`, `close_pane` | — |
| `focus_next_tab`, `focus_prev_tab` | — |
| `focus_tab_index` | `index` (int) |
| `move_tab` | `direction` (string) |
| `set_tab_orientation` | `orientation` (string): `toggle` \| `vertical` \| `horizontal` |
| `switch_workspace` | `direction` (string: `next` \| `prev`) or `index` (int) |
| `focus_next_pane`, `focus_prev_pane`, `focus_pane_left`, `focus_pane_right`, `focus_pane_up`, `focus_pane_down` | — |
| `focus_pane_by_id` | `id` (string) |
| `resize_pane`, `resize_pane_width`, `resize_pane_height` | `delta` (int) |
| `equalize_panes`, `equalize_horizontal`, `equalize_vertical`, `equalize_all` | — |
| `swap_pane`, `toggle_pipeline_mode` | — |
| `rename_pane` | `title` (string) |
| `rename_tab` | `title` (string) |
| `rename_lane` | `name` (string) |
| `stacked_pane_next`, `stacked_pane_prev` | — |
| `stacked_pane_select` | `index` (int) |
| `stacked_pane_move` | `direction` (string) |

### 3.2 Input and pane state

**`submit_input`** — `text` (string), `mode` (string), `pane_id` (string, optional: pins
the input to the pane whose box it was typed in rather than the daemon's active pane).

**`raw_key_input`** — `pane_id` (string), `data` (string). Raw keystrokes to a pane's PTY.

**`pane_resize`** — `pane_id` (string), `rows` (int), `cols` (int). This is a size
**claim, not a command**: one pane may be visible in several viewports, and it sizes its
single PTY to the smallest claim. The server tags the claim with a per-connection client
id and withdraws every claim when the socket closes — without that, a closed browser
window would clamp its panes for the life of the daemon. Send it, or the PTY stays at
80x24 and `vim` fills 24 rows regardless of the pane's real height.

**`pane_clear_output`** — `pane_id` (string).
**`pane_native_mode`** — `pane_id` (string), `action` (string).
**`agentic_cancel`** — `pane_id` (string).
**`remote_forward_command`** — `command_type` (string), `payload` (string).

### 3.3 Approvals and browser results

**`approval_response`** — releases a tool waiting on `approval_request`.

| param | type |
|---|---|
| `pane_id` | string |
| `request_id` | string |
| `decision` | string — `yes` \| `yes_always` \| `no` \| `no_with_explanation` \| `choice_selected` |
| `reason` | string — the rejection text for `no_with_explanation`; the chosen index, as a string, for `choice_selected` |

Those five are the whole vocabulary (Go: `msg.ApprovalDecision`), and the server now
**refuses** anything else with an `approval_error` rather than forwarding it. It has to:
the pane echo counts any non-rejection as approved, while the orchestrator's switch takes
its default branch and does not run the tool — so an unknown string produced a pane that
said `[approved]` and a tool that never ran.

**`browser_result`** — `pane_id` (string), `request_id` (string), `success` (bool),
`result` (any JSON), `error` (string), `screenshot` (string, base64). The frame that
made the 32 MB read limit necessary.

### 3.4 Web pane

All seven take `pane_id` (string) and accept `url` and `profile` (string) from the shared
envelope; `pane_id` is required and the command is dropped without it.

| action | meaningful params |
|---|---|
| `webpane_open` | `pane_id`, `url`, `profile` |
| `webpane_navigate` | `pane_id`, `url` |
| `webpane_back`, `webpane_forward`, `webpane_reload`, `webpane_close` | `pane_id` |

**`webpane_input`** — one forwarded input event. Every failure answers with
`webpane_error`.

| param | type | notes |
|---|---|---|
| `pane_id` | string | |
| `kind` | string | `click` \| `key` \| `scroll` \| `move` |
| `x`, `y` | float64 | in the client's own display space |
| `display_width`, `display_height` | float64 | the size the client rendered the JPEG at — the server maps `x`/`y` per axis from these into `source_width`/`source_height` |
| `button` | string | |
| `key` | string | |
| `modifiers` | array of string | |
| `delta_x`, `delta_y` | float64 | scroll |

The client must send `display_width`/`display_height` from the element it actually drew
into. Omitting them, or measuring the frame's natural size instead, is the same class of
bug as ignoring `source_width`.

### 3.5 Completion

**`completion_get`** — `request_id` (string), `pane_id` (string), `shell_pid` (int),
`token` (string), `line` (string), `cwd` (string), `is_first_token` (bool). Answered by
`completion_result` to this client only; bash completion is bounded at ~400 ms.

### 3.6 Agents

| action | params |
|---|---|
| `agent_list` | — |
| `agent_create` | `name` (string), `system_prompt` (string) |
| `agent_delete`, `agent_activate`, `agent_deactivate` | `name` (string) |
| `agent_prompt` | `agent_name` (string), `prompt` (string), `source_pane_id` (string) |
| `agent_register_output` | `agent_name` (string), `pane_id` (string), `pane_name` (string) |
| `agent_unregister_output` | `agent_name` (string), `pane_id` (string) |

### 3.7 Humanoids

| action | params |
|---|---|
| `humanoid_list` | — |
| `humanoid_create` | `name` (string), `system_prompt` (string) |
| `humanoid_delete`, `humanoid_activate`, `humanoid_deactivate` | `name` (string) |
| `humanoid_prompt` | `humanoid_name` (string), `prompt` (string), `source_pane_id` (string) |
| `humanoid_channel_start`, `humanoid_channel_stop` | `humanoid_name` (string), `channel_type` (string) |
| `humanoid_set_governance` | `humanoid_name` (string), `mode` (string) |
| `humanoid_set_reply_mode` | `humanoid_name` (string), `channel_type` (string), `mode` (string) |

### 3.8 Email and WhatsApp

| action | params |
|---|---|
| `email_list`, `email_refresh` | `humanoid_name` (string), `count` (int), `search` (string) |
| `email_read` | `humanoid_name` (string), `uid` (int) |
| `email_focus` | `humanoid_name` (string), `uid` (int), `message_id` (string), `thread_id` (string), `subject` (string), `from` (string), `body` (string), `listing` (bool) |
| `whatsapp_list`, `whatsapp_refresh` | `humanoid_name` (string), `count` (int) |
| `whatsapp_read` | `humanoid_name` (string), `id` (string) |
| `whatsapp_focus` | `humanoid_name` (string), `id` (string), `from` (string), `body` (string), `listing` (bool) |

### 3.9 Sharing

| action | params |
|---|---|
| `share_list` | — |
| `share_entity` | `entity_id` (string), `entity_type` (string), `mode` (string) |
| `unshare_entity` | `entity_id` (string) |

### 3.10 Control dashboard and pairing

Mutating actions are gated on control mode inside `handleControlCommand`.

| action | params |
|---|---|
| `control_status` | — |
| `pairing_list` | `humanoid_name` (string), `channel` (string) |
| `pairing_approve` | `humanoid_name` (string), `channel` (string), `code` (string) |
| `pairing_allow` | `humanoid_name` (string), `channel` (string), `sender_id` (string) |

### 3.11 Clipboard

**`clipboard_copy`** — copy one pane buffer out to this client. Answered by
`clipboard_content` (§2.8), to the asking client only.

| param | type | notes |
|---|---|---|
| `request_id` | string | **required** — no correlation id, no reply |
| `pane_id` | string | **required** |
| `source` | string | which buffer; empty means `output` |
| `max_bytes` | int | ask for **less** than the 256 KB ceiling; it cannot raise it. 0 means "as much as you will give me" |

`source` is a closed set: `output`, `ai_output`, `rysh_output`, `chat_output`,
`external_output`, `vt_screen`, `remote_vt_screen`. The two VT sources are the screen
lines joined with `\n`. The set is closed deliberately — the pane snapshot this reads
from also carries provider names, upstream URLs and pane metadata, and none of that
should become readable just because it shares a struct. An unrecognised `source` comes
back as an `err`, not as empty text.

Unlike every other command here, a `clipboard_copy` that fails **answers**: an unknown
source, a pane that does not reply, an unexpected reply — each returns a
`clipboard_content` with `err` set. Silence is reserved for the two genuinely
unanswerable cases, a missing `request_id` or a missing `pane_id`. A copy button that
does nothing and says nothing is a bug report nobody can act on.

#### What round-trips, and what does not

**Out of a pane (new, this command).** `clipboard_copy` → `clipboard_content`. This is
the direction that did not exist: before it, a remote client could type into a pane but
had no way to get a command's output back.

**Into a pane (already worked, incidentally).** There is no clipboard command for this
and none is needed. A client intercepts a native paste and forwards the bytes to the PTY
as `raw_key_input` with base64 `data` (`rysh-cli-app/src/components/PaneBox.tsx`), or as
`remote_forward_command` with `command_type: "raw_keystroke"` for a remote share. Both
predate this work.

**Not a round trip.** Copy and paste are not inverses here, and no client should present
them as a symmetric pair:

- Paste goes to a pane's **PTY**, as keystrokes. Copy reads a pane's **buffer**, as text.
  Pasting what you copied re-types it into the shell; it does not restore anything.
- Paste only lands in a pane that is interactive (`raw_mode` or `remote_interactive`).
  Copy works on any pane.
- Neither direction touches the **system clipboard of the machine the server runs on**.
  The server hands text to a client, and the client decides whether to write its own
  clipboard — the only clipboard a browser or phone can write, and only from a user
  gesture. Writing the host's clipboard is the AI-facing `clipboard` tool
  (`internal/tools/clipboard.go`), a separate surface with a different trust story. Do
  not describe `clipboard_copy` as "copying to the clipboard"; it copies *to the client*.
- There is **no selection** server-side. The daemon holds buffers, not a cursor range;
  the TUI's mouse selection lives in the TUI process and the browser's lives in the DOM.
  A client wanting "copy what I highlighted" reads its own selection and never calls
  this command. `clipboard_copy` answers "give me this pane's output", which is the
  question a phone with no mouse can actually ask.

### 3.12 Agents board

**`board_get`** — read one agents board. Answered by `board_result` (§2.9), to the asking
client only.

| param | type | notes |
|---|---|---|
| `request_id` | string | **required** — no correlation id, no reply |
| `pane_id` | string | the board pane asking; echoed back so the answer can be routed to it |
| `board` | string | the pane's `board.id` **meta, forwarded verbatim**; empty or unusable resolves to the session board |
| `since` | int | unix millis; keep only threads with activity at or after it |
| `limit` | int | bound on **threads**, keeping the most recent; 0 means the server default (200) |

`board` is resolved **on the server** (`msg.BoardIDFromMeta`), not by the client. This is
deliberate: the terminal UI resolves the same pane through the same function, and two
copies of that rule would drift *invisibly* — both would return a valid board id, so
nothing would error and nothing would be empty. The two surfaces would simply show
different boards for one pane, each looking correct on its own.

The bound is on threads rather than posts because that is the unit the board evicts by;
a post count would split threads from their replies and manufacture orphans.

This is a **pull**. The board has no per-board change notification for the server to
forward, so a client that wants a live view polls. The alternative — a subscription in
the web server — would be a third copy of the board in a process that is not its
recorder.

Why it exists at all: an `agents-board` pane is **shell-less**. It has no PTY, no VT
screen and no output buffer, so there is nothing in the pane snapshot for a renderer to
draw — `pane.output` for one of these holds whatever stale text was last written near it.
The terminal UI does not need this command because it builds the board itself from a
store it subscribes to directly. Every other client does.

---

## 4. Known asymmetries

Real properties of the protocol, not defects to be tidied away. A client that assumes
otherwise breaks in production, not in CI.

1. **`pane_vt` is not one shape.** Six of its nine fields are absent from the clearing
   frame. Treat them as optional.
2. **`pairing_list` is not one shape.** `channel` is present on the command reply, absent
   on the NATS-forwarded variant.
3. **Unknown commands are silent.** No error, no ack. A renamed action fails as
   "nothing happened".
4. **Malformed params are silent** in the same way — every handler `return`s on an
   unmarshal error. The three exceptions are `webpane_input`, which answers
   `webpane_error`; `approval_response`, which answers `approval_error`; and
   `clipboard_copy`, which answers `clipboard_content` with `err` set. New commands
   should follow those three rather than the silent majority.
5. **Only `completion_result` and `clipboard_content` are replies.** Everything else
   server-side is a broadcast or a content-plane push; `request_id` correlation exists
   only there and on `approval_response` / `browser_result`.

---

## 5. What the golden-vector test does and does not guarantee

`TestWSProtocolGoldenVector` parses this package and compares the extracted surface with
`testdata/ws_protocol.golden.json`. `TestWSProtocolSpecCoverage` checks that every
extracted name appears in this file.

**Guarded** — a message type or command action added, removed or renamed; a field added,
removed or renamed in any payload built as a map literal; a field silently becoming
optional by being dropped at one of several build sites.

**Not guarded** — the interior of payloads whose `data` is a Go value rather than a map
literal (`snapshot`, `agent_list`, `humanoid_list`, `share_list`, `pairing_request`,
`pairing_qr`, `pairing_status`, and the nested `request` of `approval_request` /
`browser_action`). Those are recorded in the golden as the Go expression that produces
them, so the **set** of such messages is guarded, but their fields follow the Go type.
Changing `msg.MsgApprovalRequest` will not fail this test.

**Also not guarded** — the TypeScript clients themselves. Nothing here proves
`rysh-cli-app` reads a field it is sent; it proves that a server-side change cannot pass
review without the spec being updated, which is what did not happen for `a712be3`.

---

## 6. Changing the protocol

The test failing is the design working. Make it green the legitimate way — the whole
point is that step 1 cannot be skipped.

**Adding a new message type or command.** `clipboard_copy` / `clipboard_content` (§2.8,
§3.11) is the worked example: it was added by this exact route, and the guard caught it
in both directions before a line of the spec was written.

1. **Write the server code**: for a command, a `case "your_action":` in the `switch
   action` of `handleCommand`, `handleClientCommand`, `handleControlCommand` or
   `handleWebPaneCommand`, with its `params` as an anonymous struct with `json` tags. For
   a message, a `map[string]interface{}{"type": "your_message", "data": …}`. The
   extractor finds both by shape, so no registration step exists or is needed.
2. **Document it here** — a row in the right subsection of §2 or §3, with every field
   name and its type. §5 lists what the test can and cannot see; if your payload is a Go
   value, say which Go type owns its fields.
3. **Re-record the vector**:
   ```sh
   cd rysh-cli && UPDATE_WS_GOLDEN=1 GOWORK=off go test ./internal/web/ -run TestWSProtocol
   ```
4. **Verify**: `GOWORK=off go test ./internal/web/...` — both tests green. If
   `TestWSProtocolSpecCoverage` still fails, step 2 is incomplete; it names each
   undocumented field.
5. **Update the clients** in the same change. `rysh-cli-app/src/types.ts` for the type,
   `rysh-cli-app/src/hooks/useWebSocket.ts` for the server → client mapping, then
   `make build-frontend` to re-bundle into `internal/web/static`. Nothing in Go will tell
   you if you skip this — that is the failure this whole document exists to prevent.

**Renaming or removing** anything: same steps, plus check every client for the old name
first. There is no deprecation channel and no error frame — an old client sending a
removed action gets silence.

**If the diff surprises you**, the extractor found a change you did not intend. Read it
before re-recording; `UPDATE_WS_GOLDEN=1` will happily bless a mistake.
