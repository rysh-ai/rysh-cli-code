// SPDX-License-Identifier: Apache-2.0

package main

// `rysh board send | ack | inbox` — the board as a message bus (design 050,
// polling-stream).
//
//   - send puts a COMMAND on the board, addressed to one agent. The courier
//     (design 050 §5.2) delivers it into that agent's own session — a claude's
//     inbox socket, `codex queue` — beside the human's keyboard, never typed
//     into the pane. That is the whole point: the composer belongs to the human.
//   - ack is the recipient saying "got it", on the command's own thread. A
//     reply on that thread counts as an ack too.
//   - inbox is an on-demand READ of the commands addressed to me. It does not
//     follow, wait or poll, by founder ruling (2026-10-11): the courier already
//     delivers every command, so an agent reads the board for context, never
//     to watch it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// boardSendArgs is the parsed form of send / ack / inbox. Kept apart from
// boardArgs so the status-post grammar cannot grow command-only flags by
// accident: `board post --to` is refused, not quietly accepted.
type boardSendArgs struct {
	sub   string
	as    string
	sess  string
	board string

	// send
	to       string
	cmdID    string // send: --id; ack: the positional id
	file     string
	threadID string
	text     string // send: the order; ack: --note

	// inbox
	all     bool
	jsonOut bool
}

func isBoardSendSub(sub string) bool {
	return sub == "send" || sub == "ack" || sub == "inbox"
}

// parseBoardSendArgs parses everything after `board <send|ack|inbox>`. Flags
// are extracted by name from anywhere (the F-19 rule), and a leftover
// positional is refused rather than ignored.
func parseBoardSendArgs(sub string, rest []string) (boardSendArgs, error) {
	b := boardSendArgs{sub: sub}
	usage := progname.Rewrite(boardUsage)

	rest, b.sess = extractStringFlag(rest, "--session")
	rest, boardFlag := extractStringFlag(rest, "--board")
	if boardFlag == "" {
		rest, boardFlag = extractStringFlag(rest, "--fleet")
	}
	if boardFlag != "" {
		if err := msg.ValidateBoardID(boardFlag); err != nil {
			return b, fmt.Errorf("%v\n%s", err, usage)
		}
		b.board = msg.NormalizeBoardID(boardFlag)
	}
	rest, b.as = extractStringFlag(rest, "--as")

	var body []string
	if i := indexOf(rest, "--"); i >= 0 {
		body, rest = rest[i+1:], rest[:i]
	}

	switch sub {
	case "send":
		rest, b.to = extractStringFlag(rest, "--to")
		rest, b.cmdID = extractStringFlag(rest, "--id")
		rest, b.file = extractStringFlag(rest, "--file")
		rest, b.threadID = extractStringFlag(rest, "--thread")
		if len(rest) > 0 {
			return b, fmt.Errorf("rysh board send: put the message after --, got %q\n%s",
				strings.Join(rest, " "), usage)
		}
		if b.to == "" {
			return b, fmt.Errorf("rysh board send needs a recipient: --to <pane-id|name>\n%s", usage)
		}
		if b.cmdID != "" {
			if err := msg.ValidateCmdID(b.cmdID); err != nil {
				return b, err
			}
		}
		b.text = strings.TrimSpace(strings.Join(body, " "))
		if b.text == "" && b.file == "" {
			return b, fmt.Errorf("rysh board send needs a message after --, or --file <path>\n%s", usage)
		}

	case "ack":
		rest, b.text = extractStringFlag(rest, "--note")
		if len(body) > 0 {
			rest = append(rest, body...)
		}
		if len(rest) != 1 {
			return b, fmt.Errorf("rysh board ack takes exactly one command id\n%s", usage)
		}
		b.cmdID = rest[0]
		if err := msg.ValidateCmdID(b.cmdID); err != nil {
			return b, err
		}

	case "inbox":
		rest, b.all = extractBoolFlag(rest, "--all")
		rest, b.jsonOut = extractBoolFlag(rest, "--json")
		if len(rest) > 0 || len(body) > 0 {
			return b, fmt.Errorf("rysh board inbox takes no positional arguments, got %q\n%s",
				strings.Join(append(rest, body...), " "), usage)
		}
	}
	return b, nil
}

// checkSendFile makes --file an absolute path to a regular, readable file.
// The recipient is told to "read it in full"; a path that is not there turns a
// work order into a dead end the recipient discovers mid-turn.
func checkSendFile(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("--file %s: %v", path, err)
	}
	if strings.HasPrefix(abs, "/dev/") {
		return "", fmt.Errorf("--file %s: a device is not a message file", path)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("--file %s: %v", path, err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("--file %s: not a regular file", path)
	}
	return abs, nil
}

func runBoardSendCmd(cfg config.Config, b boardSendArgs) error {
	if b.as == "" {
		b.as = os.Getenv("RYSH_PANE")
	}
	if b.as == "" {
		return errors.New(progname.Rewrite(
			"rysh board " + b.sub + ": who are you? pass --as <pane-id>, or run inside a pane where $RYSH_PANE is set"))
	}
	store, sess, err := resolveBoardSession(cfg, b.sess)
	if err != nil {
		return err
	}

	switch b.sub {
	case "send":
		req := &msg.MsgCLIBoardPost{
			AsPaneID: b.as, Kind: msg.BoardKindCmd, Text: b.text,
			ThreadID: b.threadID, Board: b.board, To: b.to, CmdID: b.cmdID,
		}
		if b.file != "" {
			abs, err := checkSendFile(b.file)
			if err != nil {
				return err
			}
			req.File = abs
			if req.Text == "" {
				req.Text = "Read " + abs + " in full."
			}
		}
		return printBoardResponse(cli.BoardSendRequest(store, sess, req))

	case "ack":
		me, boardID, reply, err := readMyBoard(store, sess, b)
		if err != nil {
			return err
		}
		it, ok := board.FindCommand(reply, me, b.cmdID)
		if !ok {
			return fmt.Errorf("no command %s addressed to you on board %s (see `%s`)",
				b.cmdID, boardID, progname.Rewrite("rysh board inbox --all"))
		}
		if it.Answered {
			fmt.Printf("command %s was already answered; nothing posted\n", it.CmdID)
			return nil
		}
		note := strings.TrimSpace(b.text)
		if note == "" {
			note = "ack " + it.CmdID
		}
		return printBoardResponse(cli.BoardSendRequest(store, sess, &msg.MsgCLIBoardPost{
			AsPaneID: me, Kind: msg.BoardKindAck, Text: note,
			ThreadID: it.ThreadID, Board: boardID, CmdID: it.CmdID,
		}))

	case "inbox":
		me, boardID, reply, err := readMyBoard(store, sess, b)
		if err != nil {
			return err
		}
		items := board.Inbox(reply, me)
		if !b.all {
			open := items[:0:0]
			for _, it := range items {
				if !it.Answered {
					open = append(open, it)
				}
			}
			items = open
		}
		if b.jsonOut {
			enc, err := json.MarshalIndent(map[string]any{"board": boardID, "pane": me, "commands": items}, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(enc))
			return nil
		}
		fmt.Print(renderInbox(boardID, items, b.all))
		return nil
	}
	return fmt.Errorf("unknown board subcommand %q", b.sub)
}

// readMyBoard resolves the caller to a full pane uuid and its board (unless
// --board named one), then asks the recorder for that board. A recorder that
// does not answer is an error, never an empty inbox — the same rule tail keeps.
func readMyBoard(store *session.Store, sess string, b boardSendArgs) (string, string, *board.QueryReply, error) {
	resp, err := cli.BoardResolve(store, sess, b.as)
	if err != nil {
		return "", "", nil, err
	}
	if !resp.OK {
		return "", "", nil, fmt.Errorf("%s", resp.Error)
	}
	me, boardID := resp.ID, strings.TrimSpace(strings.SplitN(resp.Output, "\n", 2)[0])
	if b.board != "" {
		boardID = b.board
	}
	q := boardID
	if q == msg.DefaultBoardID {
		q = ""
	}
	reply, err := cli.BoardTail(store, sess, q, board.Query{})
	if err != nil {
		if errors.Is(err, board.ErrNoRecorder) {
			return "", "", nil, boardTailNoRecorderError(err)
		}
		return "", "", nil, err
	}
	return me, boardID, reply, nil
}

func printBoardResponse(resp *msg.MsgCLIResponse, err error) error {
	if err != nil {
		return err
	}
	if out := strings.TrimRight(resp.Output, "\n"); out != "" {
		fmt.Println(out)
	}
	if !resp.OK {
		if resp.Error != "" {
			return fmt.Errorf("%s", resp.Error)
		}
		return errors.New("board request failed")
	}
	return nil
}

// renderInbox prints the reader's commands, newest first. "the recorder
// answered" wording follows tail: an empty inbox must be distinguishable from
// a recorder that never replied (which is an error, not this output).
func renderInbox(boardID string, items []board.InboxItem, all bool) string {
	var sb strings.Builder
	scope := "open"
	if all {
		scope = "all"
	}
	fmt.Fprintf(&sb, "INBOX | board %s | %d %s command(s)\n", boardID, len(items), scope)
	if len(items) == 0 {
		sb.WriteString("\nthe recorder answered: nothing addressed to you")
		if !all {
			sb.WriteString(" is open (--all shows answered ones)")
		}
		sb.WriteString("\n")
		return sb.String()
	}
	for _, it := range items {
		state := "open"
		if it.Answered {
			state = "answered " + boardTailClock(it.AckTS)
		}
		fmt.Fprintf(&sb, "\n● %s  from %s  %s  [%s]\n", it.CmdID, it.From, boardTailClock(it.TS), state)
		fmt.Fprintf(&sb, "  %s\n", it.Text)
		if it.File != "" {
			fmt.Fprintf(&sb, "  file: %s\n", it.File)
		}
		fmt.Fprintf(&sb, "  thread: %s\n", it.ThreadID)
	}
	return sb.String()
}
