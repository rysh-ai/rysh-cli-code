package actors

import (
	"errors"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/progname"
)

// handlePromptCommandInfo answers "##prompt" typed anywhere other than a .rysh
// script, and supplies the ##help entry.
//
// ##prompt is the one command word in the table that is NOT executed here. It
// is a `rysh script` builtin: running it means waiting for a whole agentic turn
// to finish, and every handler in this table runs inside the WorkspaceActor's
// message loop, where blocking would stall every pane in the session. So the
// transpiler compiles `##prompt <text>` into a call to `rysh prompt`, which
// does the waiting from its own process by watching the pane's phase events.
//
// The entry exists anyway because the alternative is worse: without it,
// `##prompt` in a pane produces "unknown command" and a list of near-misses,
// which is actively misleading about a command that ##help would otherwise
// never mention. Failing with an explanation beats failing with a guess.
func (w *WorkspaceActor) handlePromptCommandInfo(out *strings.Builder) error {
	o := ryshWriter(out)
	o.Header("##prompt is a rysh-script command, not a pane command")
	o.Rule()
	o.Row("  In a .rysh script it sends a prompt and BLOCKS until the turn ends:")
	o.Row("")
	o.Row("      ##prompt refactor the parser")
	o.Row("      echo \"the agent said: $RYSH_OUT\"")
	o.Row("")
	o.Row("  Elsewhere:")
	o.Row(progname.Rewrite("    at the keyboard   switch the pane to prompt mode and type"))
	o.Row(progname.Rewrite("    from a shell      rysh prompt -- '<text>'"))
	o.Row(progname.Rewrite("    headless / CI     rysh run \"<text>\"   (throwaway session)"))
	o.Rule()
	return errors.New("##prompt is a rysh-script command; use prompt mode, or `rysh prompt` from a shell")
}
