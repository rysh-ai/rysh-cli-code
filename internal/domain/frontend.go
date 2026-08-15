// SPDX-License-Identifier: Apache-2.0

package domain

// Front-end capabilities — what each rysh front-end can actually *render*.
//
// rysh has two front-ends over one daemon: the terminal UI (`rysh`) and the
// desktop app (`rysh-cli-app`, which also serves the same React renderer to a
// browser). They drive the identical actor tree over the identical NATS
// subjects, and a workspace layout is expressed in relative flex weights — not
// pixels — so any layout one produces is renderable by the other.
//
// What differs is not the session, it is the RENDERER. The desktop app is a
// superset: it embeds a real Chromium view, a three-column email client and a
// threaded chat client. The terminal has a native email client of its own but
// cannot paint a live web page or a threaded chat UI in a cell grid.
//
// So front-end identity is a CAPABILITY question, not an ownership question.
// A session created by the app opens fine in the terminal; the panes whose
// surfaces the terminal cannot paint degrade to a labelled placeholder. That
// mirrors the rule the browser renderer already follows for its own missing
// Electron surfaces (see internal/web/server_api.go's capability map and
// internal/web/webpane.go): the capability degrades VISIBLY, never silently.

// Front-end names. These are the same values carried by a session record's
// Source field, which records which front-end CREATED a session — provenance
// used to describe degradations, never to deny access.
const (
	FrontendCLI = "cli"
	FrontendApp = "app"
)

// FrontendCaps is the render surface set of one front-end.
type FrontendCaps struct {
	// Frontend is FrontendCLI or FrontendApp.
	Frontend string
	// EmbeddedBrowser paints a live web page inside a web-mode pane (the app's
	// Electron WebContentsView, or the server-side Chromium the browser
	// renderer streams frames from). Without it a web pane shows its binding
	// as a placeholder — the pane's browser automation still runs, driven by
	// the CLI-owned headless Chromium (`##web headless on`).
	EmbeddedBrowser bool
	// EmailClient renders a humanoid's email pane as a three-column inbox. Both
	// front-ends have one: the terminal's lives in internal/tui/email_view.go
	// and speaks the same structured NATS request/reply as the app's.
	EmailClient bool
	// ChatClient renders a humanoid's chat pane (WhatsApp) as a threaded
	// conversation with a reading pane. Without it the pane falls back to the
	// humanoid's plain text buffer, which is readable but not interactive.
	ChatClient bool
	// FileBrowser is the graphical workspace file browser. The terminal browses
	// files with ordinary shell commands instead.
	FileBrowser bool
	// NativeDialogs is OS-native chrome — folder pickers and the like — which
	// only the packaged desktop app has.
	NativeDialogs bool
}

// CapsFor returns the render capabilities of a front-end. Anything that is not
// the desktop app is treated as the terminal, matching NormalizeSource's rule
// that only "app" is explicitly recognised.
func CapsFor(frontend string) FrontendCaps {
	if frontend == FrontendApp {
		return FrontendCaps{
			Frontend:        FrontendApp,
			EmbeddedBrowser: true,
			EmailClient:     true,
			ChatClient:      true,
			FileBrowser:     true,
			NativeDialogs:   true,
		}
	}
	return FrontendCaps{
		Frontend: FrontendCLI,
		// The terminal has a real email client (internal/tui/email_view.go) —
		// this is genuine parity, not a fallback.
		EmailClient: true,
	}
}

// MissingVersus lists, in human-readable form, the surfaces `other` can render
// that c cannot. It is empty when c is at least as capable as other, so the
// desktop app opening a terminal session produces no notes at all.
//
// Each note names the degradation AND the way to keep working, because a note
// that only says "unavailable" sends the user hunting.
func (c FrontendCaps) MissingVersus(other FrontendCaps) []string {
	var notes []string
	if other.EmbeddedBrowser && !c.EmbeddedBrowser {
		notes = append(notes, "web panes show their url instead of a live page — drive them with `##web headless on`, or attach the desktop app to see the browser")
	}
	if other.EmailClient && !c.EmailClient {
		notes = append(notes, "email panes show the humanoid's plain text buffer instead of an inbox")
	}
	if other.ChatClient && !c.ChatClient {
		notes = append(notes, "chat panes show the humanoid's plain text buffer instead of threaded conversations")
	}
	if other.FileBrowser && !c.FileBrowser {
		notes = append(notes, "the graphical file browser is not available — browse with shell commands")
	}
	if other.NativeDialogs && !c.NativeDialogs {
		notes = append(notes, "native folder pickers are not available — pass paths explicitly (e.g. `##workspace cwd <path>`)")
	}
	return notes
}
