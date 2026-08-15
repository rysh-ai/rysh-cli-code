// SPDX-License-Identifier: Apache-2.0

package agentic

import "fmt"

// BuildWebModeBlock renders the browser-automation protocol appended to a
// web-mode pane's per-turn env block. It gives Ask Rysh prompts the
// observe→act→verify discipline and tells the model which authenticated
// browser (profile) it is driving. Rendered per turn, so enabling/disabling
// web mode or switching profile is picked up on the next prompt.
func BuildWebModeBlock(profile, url string) string {
	return fmt.Sprintf(`## Web mode — browser automation

This pane drives an embedded browser through the browser_action tool.
Browser profile: %q (cookies and logins persist across sessions — the user
may already be authenticated). Last bound URL: %s

Automation protocol:
1. OBSERVE before acting: read the page first (get_text / get_elements /
   get_value; screenshot when layout matters). Never click or type into a
   selector you have not just seen in the page.
2. ACT with semantic actions (click, type, select, check, press_key,
   navigate). Prefer stable selectors: aria:/role:/testid:/text: over brittle
   CSS paths. Use execute_js only when no semantic action can do the job — it
   requires user approval.
3. VERIFY after each mutating step: re-read the relevant part of the page (or
   screenshot) and confirm the expected change before moving on. If an action
   appears to have no effect, re-observe — the page may still be loading; use
   the wait action rather than repeating the click.
4. AUTH: if a page shows a login form or logged-out state, STOP and tell the
   user — never guess or ask for credentials in chat. The user logs in once in
   this profile's browser and the session persists for future automations.
5. IRREVERSIBLE ACTIONS (submitting orders, sending messages, deleting data,
   payments): describe what is about to happen and ask the user before the
   final click unless they explicitly pre-authorized it in their prompt.
6. Long tasks: report progress as you go; if the page navigates, re-observe
   before continuing. Multi-tab work: get_tabs / switch_tab keep you oriented.`,
		profile, displayURL(url))
}

// displayURL renders the bound URL for the prompt block ("(none)" when empty
// or about:blank).
func displayURL(url string) string {
	if url == "" || url == "about:blank" {
		return "(none — navigate first)"
	}
	return url
}
