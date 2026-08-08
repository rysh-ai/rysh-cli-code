// The `##llm select` picker payload: WorkspaceActor → attached front-ends,
// pushed on pane.<paneID>.llm.picker.
//
// The daemon does not drive the picker. It answers a bare `##llm select` with
// everything a front-end needs to run the interaction itself — the activatable
// models, why the rest are not activatable, the scopes a selection can bind at,
// and whether each model's provider already has a key — and then goes back to
// its mailbox. Whatever the user picks comes back as an ordinary `##` command
// (`##<scope> model <ref>`, `##secret new <NAME> <value>`), so every existing
// guard applies unchanged and nothing new can bind a model behind their back.
//
// This is what keeps an arrow-key picker compatible with the constraint that
// forced the numbered menu in the first place (see actors.cmdLLMSelect): a `##`
// command runs on the WorkspaceActor's mailbox goroutine and cannot block
// waiting for an answer. Here it never waits — it publishes and returns.
//
// A front-end that ignores this push loses nothing: the numbered menu is still
// printed, and `##llm select <n>` still works.
package msg

// LLMPickerModel is one activatable row of the picker.
type LLMPickerModel struct {
	Ref      string `json:"ref"`      // "<provider>/<name>" as declared in .rysh/llms
	Model    string `json:"model"`    // provider API model id
	Provider string `json:"provider"` // rysh provider family serving it
	Current  bool   `json:"current"`  // is the session default right now
	// KeyMissing reports that this model's provider family has no API key in
	// reach — neither ##secret nor the daemon environment — so a selection would
	// fail to authenticate on the next prompt. KeyName is the variable to set
	// (empty when the family needs no key, e.g. ollama).
	KeyMissing bool   `json:"key_missing,omitempty"`
	KeyName    string `json:"key_name,omitempty"`
}

// MsgLLMPickerOpen asks the front-end to open the interactive model picker.
type MsgLLMPickerOpen struct {
	PaneID string `json:"pane_id"`
	// Models are the activatable rows, in the same order the numbered menu
	// prints them, so a user reading either sees one list.
	Models []LLMPickerModel `json:"models"`
	// Blocked are the registry entries that cannot be activated, each already
	// rendered with its reason. Shown but never selectable.
	Blocked []string `json:"blocked,omitempty"`
	// Scopes are the binding levels offered in step two, broadest first. The
	// daemon sends them rather than the front-end hard-coding the hierarchy.
	Scopes []LLMPickerScope `json:"scopes"`
	// InEffect labels the model serving this pane right now, for the header.
	InEffect string `json:"in_effect,omitempty"`
}

// LLMPickerScope is one binding level offered in step two of the picker.
type LLMPickerScope struct {
	Name string `json:"name"` // "session", "workspace", "tab", "lane", "stack", "pane"
	// Hint is the one-line explanation shown beside the name — how far the
	// binding reaches, in the user's terms.
	Hint string `json:"hint"`
	// Command is the `##` command that binds the chosen model at this scope,
	// missing only the model ref. The front-end appends the ref and submits it,
	// so the scope hierarchy stays defined in one place: the daemon.
	Command string `json:"command"`
}
