// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// RunCI is `rysh run --hosted` (boat gap #5): start a run on the hosted
// runner, send the prompt to its pane, follow its events, and map the outcome
// to `rysh run`'s exit codes. The run is stopped on every exit (done, error,
// timeout, Ctrl-C) unless Keep. Fail-closed like `rysh run`: a pane that
// needs a person is gate-blocked (3), not something to wait on.
//
// A turn longer than the prompt route's wait (50 s) is followed through the
// event stream: the pane going back to idle is done (its output is not
// captured then; the run's pane still has it while the run lives).

// Exit codes, the same as `rysh run` (cmd/rysh/run_cmd.go).
const (
	ExitDone        = 0
	ExitError       = 1
	ExitPartial     = 2
	ExitGateBlocked = 3
	ExitBudget      = 4
	ExitTimeout     = 5
)

// promptWaitSeconds is the most the prompt route waits for a turn.
const promptWaitSeconds = 50

// RunOptions is one `rysh run --hosted` invocation.
type RunOptions struct {
	Prompt   string
	Timeout  time.Duration
	Provider string // anthropic | gemini | openai
	Model    string
	LLMKey   string // the caller's own model key: request body only
	Keep     bool   // leave the run running at exit
	JSON     bool   // print one result line on stdout instead of the output
}

// RunResult is the outcome; with JSON it is the stdout line.
type RunResult struct {
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
	RunID    string `json:"run_id,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Output   string `json:"output,omitempty"`
}

type promptReply struct {
	res PromptResult
	err error
}

// RunCI runs o.Prompt on a new hosted run. stdout gets the turn's output (or
// the JSON line); stderr gets progress and the reason for a failure.
func RunCI(parent context.Context, c *Client, o RunOptions, stdout, stderr io.Writer) RunResult {
	ctx, cancel := context.WithTimeout(parent, o.Timeout)
	defer cancel()
	res := RunResult{}
	finish := func(status string, code int, detail, output string) RunResult {
		res.Status, res.ExitCode, res.Detail, res.Output = status, code, detail, output
		if detail != "" {
			fmt.Fprintf(stderr, "rysh run --hosted: %s\n", detail)
		}
		if o.JSON {
			b, _ := json.Marshal(res)
			fmt.Fprintln(stdout, string(b))
		} else if output != "" {
			fmt.Fprint(stdout, output)
			if !strings.HasSuffix(output, "\n") {
				fmt.Fprintln(stdout)
			}
		}
		return res
	}
	ended := func() RunResult {
		if parent.Err() != nil {
			return finish("cancelled", ExitError, "cancelled", "")
		}
		return finish("timeout", ExitTimeout, fmt.Sprintf("no outcome within --timeout %v", o.Timeout), "")
	}

	kp, err := NewKeyPair()
	if err != nil {
		return finish("error", ExitError, "could not make a key: "+err.Error(), "")
	}
	run, err := c.Start(ctx, StartOptions{Name: "ci", LLMProvider: o.Provider, LLMModel: o.Model, LLMKey: o.LLMKey}, kp.Public)
	if err != nil {
		if ctx.Err() != nil {
			return ended()
		}
		return finish("error", ExitError, "could not start a run: "+scrub(err.Error(), []string{o.LLMKey}), "")
	}
	res.RunID = run.ID
	fmt.Fprintf(stderr, "rysh run --hosted: run %s started\n", run.ID)
	if o.Keep {
		defer fmt.Fprintf(stderr, "rysh run --hosted: run %s kept (stop it with ##hosted stop %s)\n", run.ID, run.ID)
	} else {
		defer func() {
			sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer scancel()
			if _, err := c.Stop(sctx, run.ID); err != nil {
				fmt.Fprintf(stderr, "rysh run --hosted: could not stop run %s: %v\n", run.ID, err)
			}
		}()
	}

	pane, err := firstPane(ctx, c, run.ID)
	if err != nil {
		if ctx.Err() != nil {
			return ended()
		}
		return finish("error", ExitError, "the run has no pane: "+err.Error(), "")
	}

	evCtx, evCancel := context.WithCancel(ctx)
	defer evCancel()
	evCh := make(chan Event, 64)
	evErr := make(chan error, 1)
	go func() {
		for evCtx.Err() == nil {
			err := c.Events(evCtx, run.ID, func(e Event) error {
				select {
				case evCh <- e:
					return nil
				case <-evCtx.Done():
					return evCtx.Err()
				}
			})
			if evCtx.Err() != nil {
				return
			}
			if err != nil {
				evErr <- err
				return
			}
			// "reconnect" or a clean close: open a new stream.
			select {
			case <-time.After(500 * time.Millisecond):
			case <-evCtx.Done():
				return
			}
		}
	}()

	prCh := make(chan promptReply, 1)
	go func() {
		r, err := c.Prompt(ctx, run.ID, pane, o.Prompt, promptWaitSeconds)
		prCh <- promptReply{r, err}
	}()

	turnRunning, sawBusy, streamLost := false, false, false
	for {
		select {
		case <-ctx.Done():
			return ended()
		case pr := <-prCh:
			if pr.err != nil {
				if ctx.Err() != nil {
					return ended()
				}
				return finish("error", ExitError, "the prompt failed: "+scrub(pr.err.Error(), []string{o.LLMKey}), "")
			}
			if pr.res.Status == "timeout" {
				turnRunning = true
				if streamLost {
					return finish("error", ExitError, "the event stream was lost while the turn was still running", "")
				}
				continue
			}
			return finish(pr.res.Status, exitFor(pr.res.ExitCode), pr.res.Detail, pr.res.Output)
		case err := <-evErr:
			streamLost = true
			fmt.Fprintf(stderr, "rysh run --hosted: event stream lost: %v\n", err)
			if turnRunning {
				return finish("error", ExitError, "the event stream was lost while the turn was still running", "")
			}
		case e := <-evCh:
			switch e.Name {
			case "needs_you":
				var ny struct{ Pane, Attention string }
				_ = json.Unmarshal(e.Data, &ny)
				return finish("gate_blocked", ExitGateBlocked, "a pane needs a person ("+orWord(ny.Attention, "attention")+"); nobody can answer in --hosted", "")
			case "end":
				var end struct{ State, Reason string }
				_ = json.Unmarshal(e.Data, &end)
				return finish("error", ExitError, "the run ended before the turn did: "+end.State+" ("+orWord(end.Reason, "no reason given")+")", "")
			case "panes":
				var ps struct{ Panes []Pane }
				_ = json.Unmarshal(e.Data, &ps)
				for _, p := range ps.Panes {
					if p.ID != pane {
						continue
					}
					if p.NeedsYou {
						return finish("gate_blocked", ExitGateBlocked, "a pane needs a person ("+orWord(p.Attention, "attention")+"); nobody can answer in --hosted", "")
					}
					switch {
					case p.Status != "idle" && p.Status != "error":
						sawBusy = true
					case turnRunning && sawBusy && p.Status == "error":
						return finish("error", ExitError, "the turn ended with an error", "")
					case turnRunning && sawBusy && p.Status == "idle":
						return finish("done", ExitDone, "the turn ran longer than the prompt wait; its output was not captured", "")
					}
				}
			}
		}
	}
}

// firstPane waits for the run's session to have a pane.
func firstPane(ctx context.Context, c *Client, runID string) (string, error) {
	var last error
	for {
		panes, err := c.Panes(ctx, runID)
		if err == nil && len(panes) > 0 {
			return panes[0].ID, nil
		}
		if err != nil {
			last = err
		}
		select {
		case <-ctx.Done():
			if last == nil {
				last = ctx.Err()
			}
			return "", last
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func exitFor(code int) int {
	if code >= ExitDone && code <= ExitTimeout {
		return code
	}
	return ExitError
}

func orWord(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
