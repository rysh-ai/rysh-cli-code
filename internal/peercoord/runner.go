// SPDX-License-Identifier: Apache-2.0

package peercoord

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/rysh-ai/rysh-cli-code/internal/agentenv"
	"github.com/rysh-ai/rysh-cli-code/internal/peer"
)

type RunnerSpec struct {
	CommandContext                                                                                 string
	CommandToken                                                                                   string
	KeySecret                                                                                      string
	LogPath                                                                                        string
	Socket, Execution, Token, Pane, Directory, Session, RyshDir, Binary, Runner, Permission, Model string
	MaxTurns                                                                                       int
	MaxBudgetUSD                                                                                   float64
	Deadline                                                                                       time.Time
}
type TurnResult struct {
	CostUSD      float64
	Conversation string
	Err          error
}

// RunAgent is a local, interactive supervisor. The provider CLI runs a structured
// turn; the pane stays open between turns and can pause/resume automatic delivery.
// No shared transcript, pane subscription, or remote shell channel is involved.
func RunAgent(path string) error {
	var spec RunnerSpec
	data, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	if e = json.Unmarshal(data, &spec); e != nil {
		return e
	}
	if spec.Execution == "" || spec.Token == "" || spec.Pane != os.Getenv("RYSH_PANE") {
		return fmt.Errorf("peer agent must run in its assigned pane")
	}
	lock, e := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = lockFile(lock); e != nil {
		return fmt.Errorf("this execution already has a supervisor")
	}
	defer unlockFile(lock)
	// Persistent launch marker prevents an accidentally duplicated shell message
	// from restarting an already finished or uncertain execution.
	marker, e := os.OpenFile(path+".started", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return fmt.Errorf("execution already started; reconcile instead of relaunching")
	}
	_ = marker.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	control := func(op string, extra Control) (ControlReply, error) {
		extra.Execution = spec.Execution
		extra.Token = spec.Token
		extra.Pane = spec.Pane
		extra.Op = op
		return Call(ctx, spec.Socket, extra)
	}
	fmt.Printf("Peer agent %s · %s\nWorktree: %s\nType :pause to take over, :resume to resume delivery, :quit to stop.\n", spec.Execution, spec.Runner, spec.Directory)
	var hello ControlReply
	for i := 0; i < 20; i++ {
		hello, e = control("hello", Control{})
		if e == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if e != nil {
		return e
	}
	if hello.Stop {
		return fmt.Errorf("execution no longer authorized")
	}
	conversation := hello.Conversation
	lines := make(chan string, 8)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var turnCancel context.CancelFunc
	var finished <-chan TurnResult
	var delivery string
	launch := func(next ControlReply) {
		delivery = next.DeliveryID
		turnCtx, cancel := context.WithDeadline(ctx, spec.Deadline)
		turnCancel = cancel
		ch := make(chan TurnResult, 1)
		finished = ch
		turnSpec := spec
		if spec.MaxBudgetUSD > 0 {
			turnSpec.MaxBudgetUSD = next.BudgetRemaining
		}
		go func(prompt, session string) {
			defer cancel()
			ch <- RunTurn(turnCtx, turnSpec, session, prompt, func(id string) error { _, err := control("conversation", Control{Conversation: id}); return err }, os.Stdout)
		}(next.Prompt, conversation)
	}
	lastContact := time.Now()
	exitReason := "local owner stopped agent"
	defer func() {
		if turnCancel != nil {
			turnCancel()
			if finished != nil {
				select {
				case <-finished:
				case <-time.After(8 * time.Second):
					exitReason = "uncertain: runner termination unconfirmed"
				}
			}
		}
		exitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = Call(exitCtx, spec.Socket, Control{Execution: spec.Execution, Token: spec.Token, Pane: spec.Pane, Op: "exit", Error: exitReason})
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case line := <-lines:
			switch strings.TrimSpace(line) {
			case ":quit":
				return nil
			case ":pause":
				_, e = control("pause", Control{})
				if turnCancel != nil {
					turnCancel()
				}
				fmt.Println("Automatic delivery paused. Once the current turn stops, type a message to continue manually. :resume re-enables peer delivery.")
			case ":resume":
				_, e = control("resume", Control{})
				if e == nil {
					fmt.Println("Automatic delivery resumed")
				}
			default:
				if finished != nil {
					fmt.Println("Wait for the current turn, or :pause to interrupt it.")
					continue
				}
				manual, err := control("input", Control{Text: line})
				if err != nil {
					fmt.Println("Peer:", err)
					continue
				}
				launch(manual)
			}
		case result := <-finished:
			finished = nil
			turnCancel = nil
			if result.Conversation != "" {
				conversation = result.Conversation
			}
			detail := ""
			if result.Err != nil {
				detail = result.Err.Error()
				fmt.Println("Runner:", detail)
			}
			receipt := Control{DeliveryID: delivery, Error: detail, CostUSD: result.CostUSD}
			// Retain the exact receipt locally until the daemon confirms it. A lost
			// acknowledgement never starts another turn over an unconfirmed delivery.
			for i := 0; i < 5; i++ {
				_, e = control("finish", receipt)
				if e == nil {
					break
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(time.Second):
				}
			}
			if e != nil {
				exitReason = "turn completion could not be confirmed"
				return e
			}
		case <-ticker.C:
			ping, err := control("ping", Control{})
			if err != nil {
				if time.Since(lastContact) > 15*time.Second {
					if turnCancel != nil {
						turnCancel()
					}
					exitReason = "listener disconnected; execution stopped for reconciliation"
					return err
				}
				continue
			}
			lastContact = time.Now()
			if ping.Stop {
				exitReason = "task cancelled, leadership changed, or enrolled runtime expired"
				if turnCancel != nil {
					turnCancel()
				}
				return nil
			}
			if finished != nil || ping.Paused {
				continue
			}
			next, err := control("next", Control{})
			if err != nil {
				fmt.Println("Peer:", err)
				continue
			}
			if next.Stop {
				exitReason = "enrolled turn limit reached"
				return nil
			}
			if next.Done {
				exitReason = "result submitted"
				return nil
			}
			if next.Prompt == "" {
				continue
			}
			launch(next)
		}
	}
}
func RunnerArgs(spec RunnerSpec, conversation, prompt string) ([]string, io.Reader, error) {
	if spec.Runner == "codex" {
		args := []string{"exec"}
		if conversation != "" {
			args = append(args, "resume")
		}
		args = append(args, "--json", "-c", "approval_policy=\"never\"")
		if spec.Permission == "workspace" {
			args = append(args, "-c", "sandbox_mode=\"workspace-write\"", "-c", "sandbox_workspace_write.network_access=true")
		} else {
			if !filepath.IsAbs(spec.Socket) || filepath.Clean(spec.Socket) == string(filepath.Separator) {
				return nil, nil, fmt.Errorf("Codex review requires an absolute peer control socket")
			}
			// Read-only Codex otherwise denies the AF_UNIX connection to the
			// local listener. Permit only this capability-checked socket, with
			// the network proxy active and no external domains allowed. An
			// ephemeral proxy port lets independent sessions coexist.
			profile := fmt.Sprintf(`{extends=":read-only",network={enabled=true,proxy_url="http://127.0.0.1:0",enable_socks5=false,domains={},unix_sockets={%q="allow"}}}`, spec.Socket)
			args = append(args, "-c", `default_permissions="rysh_peer_review"`, "-c", "permissions.rysh_peer_review="+profile, "-c", "features.network_proxy=true")
		}
		args = append(args, "-c", "features.multi_agent=false")
		if spec.Model != "" {
			args = append(args, "--model", spec.Model)
		}
		if conversation != "" {
			args = append(args, conversation)
		}
		args = append(args, "-")
		return args, strings.NewReader(prompt), nil
	}
	if spec.Runner == "claude" {
		args := []string{"--print", "--verbose", "--output-format", "stream-json", "--permission-mode", "dontAsk"}
		allowed := "Read,Grep,Glob,Bash(" + spec.Binary + " peer *)"
		if spec.Permission == "workspace" {
			args[5] = "acceptEdits"
			allowed = ",Read,Grep,Glob,Edit,Write,Bash"
			allowed = strings.TrimPrefix(allowed, ",")
		}
		args = append(args, "--allowedTools", allowed, "--tools", "Read,Edit,Write,Bash,Grep,Glob")
		if conversation == "" {
			args = append(args, "--session-id", uuid.NewString())
		} else {
			args = append(args, "--resume", conversation)
		}
		if spec.Model != "" {
			args = append(args, "--model", spec.Model)
		}
		if spec.MaxBudgetUSD > 0 {
			args = append(args, "--max-budget-usd", fmt.Sprint(spec.MaxBudgetUSD))
		}
		return args, strings.NewReader(prompt), nil
	}
	return nil, nil, fmt.Errorf("unsupported runner")
}
func RunTurn(ctx context.Context, spec RunnerSpec, conversation, prompt string, onConversation func(string) error, out io.Writer) TurnResult {
	args, input, e := RunnerArgs(spec, conversation, prompt)
	if e != nil {
		return TurnResult{Err: e}
	}
	cmd := exec.CommandContext(ctx, spec.Runner, args...)
	cmd.Dir = spec.Directory
	cmd.Stdin = input
	env := []string{}
	// The credential scrub (agentenv) first; the peer-specific filtering below.
	for _, v := range agentenv.ForRunner(os.Environ(), spec.Runner) {
		key, _, _ := strings.Cut(v, "=")
		if key == spec.KeySecret || strings.HasPrefix(key, "CLAUDE_CODE") || key == "CLAUDECODE" || strings.HasPrefix(key, "RYSH_PEER_") {
			continue
		}
		if key == "RYSH_PANE" || key == "RYSH_SESSION" || key == "RYSH_DIR" {
			continue
		}
		env = append(env, v)
	}
	env = append(env, "RYSH_PANE="+spec.Pane, "RYSH_SESSION="+spec.Session, "RYSH_DIR="+spec.RyshDir, "RYSH_BIN="+spec.Binary, "RYSH_PEER_SOCKET="+spec.Socket, "RYSH_PEER_CONTEXT="+spec.CommandContext, "RYSH_PEER_EXECUTION="+spec.Execution)
	cmd.Env = env
	configureProcess(cmd)
	cmd.WaitDelay = 3 * time.Second
	pipe, e := cmd.StdoutPipe()
	if e != nil {
		return TurnResult{Err: e}
	}
	logPath := spec.LogPath
	if logPath == "" {
		logPath = filepath.Join(os.TempDir(), "rysh-peer-"+spec.Execution+".jsonl")
	}
	log, e := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return TurnResult{Err: e}
	}
	defer log.Close()
	var outputMu sync.Mutex
	cmd.Stderr = &lockedWriter{mu: &outputMu, w: out}
	if e = cmd.Start(); e != nil {
		return TurnResult{Err: fmt.Errorf("runner launch failed: %w", e)}
	}
	result := TurnResult{Conversation: conversation}
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		line := append([]byte{}, scanner.Bytes()...)
		_, _ = log.Write(append(line, '\n'))
		var ev struct {
			CostUSD   float64 `json:"total_cost_usd"`
			Type      string  `json:"type"`
			ThreadID  string  `json:"thread_id"`
			SessionID string  `json:"session_id"`
			Result    string  `json:"result"`
			IsError   bool    `json:"is_error"`
			Item      struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Command string `json:"command"`
			} `json:"item"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		id := ev.ThreadID
		if id == "" {
			id = ev.SessionID
		}
		if id != "" && id != result.Conversation {
			if e = onConversation(id); e != nil {
				_ = cmd.Cancel()
				result.Err = e
				break
			}
			result.Conversation = id
		}
		var text string
		if ev.Type == "item.completed" {
			text = ev.Item.Text
			if text == "" && ev.Item.Command != "" {
				text = "$ " + ev.Item.Command
			}
		}
		if ev.Type == "assistant" {
			for _, c := range ev.Message.Content {
				if c.Type == "text" {
					text += c.Text + "\n"
				} else if c.Type == "tool_use" {
					text += "[tool] " + c.Name + "\n"
				}
			}
		}
		if ev.Type == "result" {
			result.CostUSD = ev.CostUSD
			if ev.IsError {
				result.Err = fmt.Errorf("provider reported a failed turn")
			}
			text = ev.Result
		}
		if text != "" {
			outputMu.Lock()
			fmt.Fprintln(out, peer.Display(text))
			outputMu.Unlock()
		}
	}
	if e = cmd.Wait(); e != nil && result.Err == nil {
		result.Err = fmt.Errorf("runner exited: %w", e)
	}
	if scanner.Err() != nil && result.Err == nil {
		result.Err = scanner.Err()
	}
	if result.Conversation == "" && result.Err == nil {
		result.Err = fmt.Errorf("runner did not report its conversation ID")
	}
	return result
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}
