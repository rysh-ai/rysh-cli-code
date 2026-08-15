// SPDX-License-Identifier: Apache-2.0

package cdp

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Do executes one browser_action request against the browser and returns the
// action result (JSON for the model), an optional base64 PNG screenshot, and
// an error. The action names, param shapes, and selector formats match the
// browser_action tool spec (rysh-shared/tools/browser_action.go) exactly, so
// the headless executor is a drop-in peer of the desktop app / extension.
func (b *Browser) Do(action string, rawParams json.RawMessage) (json.RawMessage, string, error) {
	var p actionParams
	if len(rawParams) > 0 {
		if err := json.Unmarshal(rawParams, &p); err != nil {
			return nil, "", fmt.Errorf("invalid params: %w", err)
		}
	}
	switch action {
	case "navigate":
		return b.doNavigate(p)
	case "back":
		return b.doHistory(-1)
	case "forward":
		return b.doHistory(1)
	case "reload":
		if _, err := b.Page("Page.reload", map[string]any{}); err != nil {
			return nil, "", err
		}
		b.waitReady(10 * time.Second)
		return b.pageSummary()
	case "click":
		return b.doClick(p)
	case "type":
		return b.doType(p)
	case "select":
		return b.doSelect(p)
	case "check":
		return b.doCheck(p)
	case "hover":
		return b.doHover(p)
	case "paste":
		return nil, "", fmt.Errorf("paste is only available in the desktop app's embedded browser panes (Electron native paste); in CLI-owned headless Chromium use type/keystrokes instead")
	case "press_key":
		return b.doPressKey(p)
	case "drag_drop":
		return b.doDragDrop(p)
	case "scroll":
		return b.doScroll(p)
	case "wait":
		return b.doWait(p)
	case "get_text":
		return b.doGetText(p)
	case "get_html":
		return b.doGetHTML(p)
	case "get_elements":
		return b.doGetElements(p)
	case "get_value":
		return b.evalResult(fmt.Sprintf(`(()=>{%s; const el=__ryshFind(%s,%s,%d); if(!el) throw new Error("element not found: "+%s); return {value: el.value ?? el.textContent ?? ""};})()`,
			findJS, jsStr(p.Selector), jsStr(p.Text), p.Index, jsStr(p.Selector)))
	case "screenshot":
		return b.doScreenshot(p)
	case "get_tabs":
		return b.doGetTabs()
	case "switch_tab":
		return b.doSwitchTab(p)
	case "new_tab":
		return b.doNewTab(p)
	case "close_tab":
		return b.doCloseTab(p)
	case "execute_js":
		if strings.TrimSpace(p.Code) == "" {
			return nil, "", fmt.Errorf("execute_js requires params.code")
		}
		return b.evalResult(fmt.Sprintf(`(()=>({result: eval(%s)}))()`, jsStr(p.Code)))
	default:
		return nil, "", fmt.Errorf("unknown browser action %q", action)
	}
}

// actionParams is the union of all action param shapes from the tool spec.
type actionParams struct {
	URL          string   `json:"url,omitempty"`
	Selector     string   `json:"selector,omitempty"`
	FromSelector string   `json:"from_selector,omitempty"`
	ToSelector   string   `json:"to_selector,omitempty"`
	Text         string   `json:"text,omitempty"`
	Value        string   `json:"value,omitempty"`
	Code         string   `json:"code,omitempty"`
	Key          string   `json:"key,omitempty"`
	Modifiers    []string `json:"modifiers,omitempty"`
	Direction    string   `json:"direction,omitempty"`
	Amount       int      `json:"amount,omitempty"`
	Index        int      `json:"index,omitempty"`
	Limit        int      `json:"limit,omitempty"`
	TimeoutMs    int      `json:"timeout_ms,omitempty"`
	TabID        string   `json:"tab_id,omitempty"`
	URLPattern   string   `json:"url_pattern,omitempty"`
	Attributes   []string `json:"attributes,omitempty"`
	Clear        *bool    `json:"clear,omitempty"`
	Keystrokes   bool     `json:"keystrokes,omitempty"`
	Count        int      `json:"count,omitempty"`
	Checked      *bool    `json:"checked,omitempty"`
	Outer        bool     `json:"outer,omitempty"`
	Visible      bool     `json:"visible,omitempty"`
	// Format/Quality are screenshot-only and NOT part of the browser_action
	// tool spec — the model never sets them. They exist for rysh's internal
	// run recorder (##auto web run --record), which captures a frame every
	// few hundred milliseconds and would burn hundreds of megabytes at the
	// default full-size PNG. Peers that predate these fields (the desktop
	// app, the extension) ignore them and return PNG, which the recorder
	// detects from the magic bytes and handles.
	Format  string `json:"format,omitempty"`  // "png" (default) | "jpeg" | "webp"
	Quality int    `json:"quality,omitempty"` // 0-100, jpeg/webp only
}

// UnmarshalJSON tolerates numeric tab_id (the tool examples show integers).
func (a *actionParams) UnmarshalJSON(data []byte) error {
	type alias actionParams
	var tmp struct {
		alias
		TabIDRaw json.RawMessage `json:"tab_id,omitempty"`
	}
	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}
	*a = actionParams(tmp.alias)
	if len(tmp.TabIDRaw) > 0 {
		var s string
		if json.Unmarshal(tmp.TabIDRaw, &s) == nil {
			a.TabID = s
		} else {
			var n int64
			if json.Unmarshal(tmp.TabIDRaw, &n) == nil {
				a.TabID = fmt.Sprintf("%d", n)
			}
		}
	}
	return nil
}

// findJS is the selector engine injected before element actions. It resolves
// the tool's selector formats: CSS, "xpath:"/"//…", "text:…", "aria:…",
// "role:…", "testid:…". Second arg optionally filters matches by contained
// text; third picks the match index.
const findJS = `const __ryshFind = (sel, filterText, idx) => {
  sel = String(sel || ""); idx = idx || 0;
  let els = [];
  const all = (q) => Array.from(document.querySelectorAll(q));
  try {
    if (sel.startsWith("xpath:") || sel.startsWith("//")) {
      const xp = sel.startsWith("xpath:") ? sel.slice(6) : sel;
      const it = document.evaluate(xp, document, null, XPathResult.ORDERED_NODE_SNAPSHOT_TYPE, null);
      for (let i = 0; i < it.snapshotLength; i++) els.push(it.snapshotItem(i));
    } else if (sel.startsWith("text:")) {
      const t = sel.slice(5).toLowerCase();
      els = all("a,button,input,select,textarea,label,[role],summary,option,span,div,li,td,th,h1,h2,h3,h4")
        .filter(e => ((e.innerText || e.value || "") + "").toLowerCase().includes(t));
      els.sort((x, y) => (x.innerText || "").length - (y.innerText || "").length);
    } else if (sel.startsWith("aria:")) {
      const t = sel.slice(5).toLowerCase();
      els = all("[aria-label]").filter(e => e.getAttribute("aria-label").toLowerCase().includes(t));
    } else if (sel.startsWith("role:")) {
      els = all('[role="' + sel.slice(5) + '"]');
    } else if (sel.startsWith("testid:")) {
      els = all('[data-testid="' + sel.slice(7) + '"]');
    } else {
      els = all(sel);
    }
  } catch (e) { throw new Error("bad selector " + sel + ": " + e.message); }
  if (filterText) {
    const t = String(filterText).toLowerCase();
    els = els.filter(e => ((e.innerText || e.value || "") + "").toLowerCase().includes(t));
  }
  return els[idx] || null;
};
const __ryshDescribe = (el) => {
  if (!el) return null;
  const r = el.getBoundingClientRect();
  return { tag: el.tagName.toLowerCase(), id: el.id || undefined,
           text: ((el.innerText || el.value || "") + "").slice(0, 120),
           visible: r.width > 0 && r.height > 0 };
};`

// jsStr encodes a Go string as a JS string literal.
func jsStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// evaluate runs an expression on the current tab and returns its
// returnByValue result.
func (b *Browser) evaluate(expr string) (json.RawMessage, error) {
	res, err := b.Page("Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	if out.ExceptionDetails != nil {
		msg := out.ExceptionDetails.Text
		if out.ExceptionDetails.Exception != nil && out.ExceptionDetails.Exception.Description != "" {
			msg = out.ExceptionDetails.Exception.Description
		}
		return nil, fmt.Errorf("page error: %s", firstJSLine(msg))
	}
	if out.Result.Value == nil {
		return json.RawMessage(`null`), nil
	}
	return out.Result.Value, nil
}

// evalResult adapts evaluate to the Do return shape.
func (b *Browser) evalResult(expr string) (json.RawMessage, string, error) {
	v, err := b.evaluate(expr)
	return v, "", err
}

func firstJSLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// waitReady polls document.readyState until "complete" (or interactive after
// half the budget) so actions after navigation see a settled DOM.
func (b *Browser) waitReady(max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		v, err := b.evaluate(`document.readyState`)
		if err == nil {
			state := strings.Trim(string(v), `"`)
			if state == "complete" || (state == "interactive" && time.Until(deadline) < max/2) {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// pageSummary returns {url, title} — the standard result for navigations.
func (b *Browser) pageSummary() (json.RawMessage, string, error) {
	return b.evalResult(`(()=>({url: location.href, title: document.title}))()`)
}

func (b *Browser) doNavigate(p actionParams) (json.RawMessage, string, error) {
	if p.URL == "" {
		return nil, "", fmt.Errorf("navigate requires params.url")
	}
	if _, err := b.Page("Page.navigate", map[string]any{"url": p.URL}); err != nil {
		return nil, "", err
	}
	b.waitReady(15 * time.Second)
	return b.pageSummary()
}

func (b *Browser) doHistory(delta int) (json.RawMessage, string, error) {
	fn := "back"
	if delta > 0 {
		fn = "forward"
	}
	if _, err := b.evaluate(`history.` + fn + `()`); err != nil {
		return nil, "", err
	}
	b.waitReady(10 * time.Second)
	return b.pageSummary()
}

func (b *Browser) doClick(p actionParams) (json.RawMessage, string, error) {
	return b.evalResult(fmt.Sprintf(`(()=>{%s;
  const el = __ryshFind(%s, %s, %d);
  if (!el) throw new Error("element not found: " + %s);
  el.scrollIntoView({block:"center", inline:"center"});
  el.click();
  return {clicked: __ryshDescribe(el)};})()`,
		findJS, jsStr(p.Selector), jsStr(p.Text), p.Index, jsStr(p.Selector)))
}

// doType inserts text as TRUSTED input via CDP Input.insertText (with real
// Input.dispatchKeyEvent Enters between lines). Synthetic JS input — value
// setters + dispatchEvent(new InputEvent(...)) or execCommand('insertText') —
// carries isTrusted:false and is ignored (or DOM-mutated then REVERTED on the
// next render) by controlled contenteditable editors like Medium's Draft.js/
// ProseMirror story editor. Focus + selection prep stays in JS (harmless);
// only the input itself must be trusted. The selection made here is REPLACED
// by the first Input.insertText, which implements clear without touching the
// editor's model behind its back.
func (b *Browser) doType(p actionParams) (json.RawMessage, string, error) {
	clear := true
	if p.Clear != nil {
		clear = *p.Clear
	}
	// Focus the element; select existing content when clearing, else collapse
	// the caret to the end for append.
	if _, _, err := b.evalResult(fmt.Sprintf(`(()=>{%s;
  const el = __ryshFind(%s, "", %d);
  if (!el) throw new Error("element not found: " + %s);
  el.scrollIntoView({block:"center"}); el.focus();
  const clear = %t;
  if (el.tagName === "INPUT" || el.tagName === "TEXTAREA") {
    if (clear) el.select();
    else el.setSelectionRange(el.value.length, el.value.length);
  } else if (el.isContentEditable) {
    const range = document.createRange();
    range.selectNodeContents(el);
    if (!clear) range.collapse(false);
    const s = window.getSelection();
    s.removeAllRanges();
    s.addRange(range);
  }
  return {focused: true};})()`,
		findJS, jsStr(p.Selector), p.Index, jsStr(p.Selector), clear)); err != nil {
		return nil, "", err
	}
	// Trusted insertion, line by line: Input.insertText replaces the active
	// selection; real Enter key events separate the lines so editors create
	// genuine paragraph/line breaks. keystrokes=true sends one real key event
	// per character instead — REQUIRED where the editor only reacts to actual
	// keydowns (lab-verified on Medium's title field 2026-07-18: its default
	// "Title" text clears on the first keydown but bulk insertText leaves it
	// appended to the typed title).
	lines := strings.Split(p.Text, "\n")
	for i, line := range lines {
		if p.Keystrokes {
			for _, ch := range line {
				key := string(ch)
				if key == " " {
					key = "Space"
				}
				if _, ok := keyCodes[key]; ok {
					if err := b.pressRawKey(key, 0); err != nil {
						return nil, "", err
					}
				} else {
					ev := map[string]any{"type": "char", "key": string(ch), "text": string(ch)}
					if _, err := b.Page("Input.dispatchKeyEvent", ev); err != nil {
						return nil, "", err
					}
				}
			}
		} else if line != "" {
			if _, err := b.Page("Input.insertText", map[string]any{"text": line}); err != nil {
				return nil, "", err
			}
		}
		if i < len(lines)-1 {
			if err := b.pressRawKey("Enter", 0); err != nil {
				return nil, "", err
			}
		}
	}
	return json.RawMessage(fmt.Sprintf(`{"typed":%d,"trusted":true,"keystrokes":%t}`, len(p.Text), p.Keystrokes)), "", nil
}

// pressRawKey sends one trusted keyDown/keyUp pair through CDP.
func (b *Browser) pressRawKey(key string, modifiers int) error {
	down := map[string]any{"type": "keyDown", "key": key, "modifiers": modifiers}
	up := map[string]any{"type": "keyUp", "key": key, "modifiers": modifiers}
	if code, ok := keyCodes[key]; ok {
		down["windowsVirtualKeyCode"] = code
		down["nativeVirtualKeyCode"] = code
		up["windowsVirtualKeyCode"] = code
		up["nativeVirtualKeyCode"] = code
	}
	if text, ok := keyText[key]; ok {
		down["text"] = text // without this, Enter/Space/Tab move focus but TYPE nothing
	}
	if _, err := b.Page("Input.dispatchKeyEvent", down); err != nil {
		return err
	}
	_, err := b.Page("Input.dispatchKeyEvent", up)
	return err
}

func (b *Browser) doSelect(p actionParams) (json.RawMessage, string, error) {
	return b.evalResult(fmt.Sprintf(`(()=>{%s;
  const el = __ryshFind(%s, "", %d);
  if (!el || el.tagName !== "SELECT") throw new Error("select element not found: " + %s);
  const byValue = %s, byText = %s;
  let opt = null;
  for (const o of el.options) {
    if ((byValue && o.value === byValue) || (byText && o.text.trim() === byText.trim())) { opt = o; break; }
  }
  if (!opt) throw new Error("option not found (value=" + byValue + ", text=" + byText + ")");
  el.value = opt.value;
  el.dispatchEvent(new Event("change", {bubbles: true}));
  return {selected: opt.value, text: opt.text};})()`,
		findJS, jsStr(p.Selector), p.Index, jsStr(p.Selector), jsStr(p.Value), jsStr(p.Text)))
}

func (b *Browser) doCheck(p actionParams) (json.RawMessage, string, error) {
	checked := true
	if p.Checked != nil {
		checked = *p.Checked
	}
	return b.evalResult(fmt.Sprintf(`(()=>{%s;
  const el = __ryshFind(%s, "", %d);
  if (!el) throw new Error("checkbox not found: " + %s);
  if (el.checked !== %t) { el.click(); }
  return {checked: el.checked, element: __ryshDescribe(el)};})()`,
		findJS, jsStr(p.Selector), p.Index, jsStr(p.Selector), checked))
}

func (b *Browser) doHover(p actionParams) (json.RawMessage, string, error) {
	return b.evalResult(fmt.Sprintf(`(()=>{%s;
  const el = __ryshFind(%s, "", %d);
  if (!el) throw new Error("element not found: " + %s);
  el.scrollIntoView({block:"center"});
  for (const t of ["pointerover","mouseover","mouseenter"])
    el.dispatchEvent(new MouseEvent(t, {bubbles: t !== "mouseenter", view: window}));
  return {hovered: __ryshDescribe(el)};})()`,
		findJS, jsStr(p.Selector), p.Index, jsStr(p.Selector)))
}

// keyCodes maps named keys to Windows virtual key codes for CDP key events.
var keyCodes = map[string]int{
	"Enter": 13, "Tab": 9, "Escape": 27, "Backspace": 8, "Delete": 46,
	"ArrowUp": 38, "ArrowDown": 40, "ArrowLeft": 37, "ArrowRight": 39,
	"Home": 36, "End": 35, "PageUp": 33, "PageDown": 34, "Space": 32,
}

// keyText is the character a named key TYPES (CDP keyDown.text). Without it a
// dispatched Enter/Space/Tab changes focus/submits but inserts nothing — which
// silently breaks editor markdown shortcuts that trigger on a real typed space
// (Medium's "## ", "* ", "```" + space patterns).
var keyText = map[string]string{
	"Enter": "\r", "Space": " ", "Tab": "\t",
}

func (b *Browser) doPressKey(p actionParams) (json.RawMessage, string, error) {
	if p.Key == "" {
		return nil, "", fmt.Errorf("press_key requires params.key")
	}
	var modifiers int
	for _, m := range p.Modifiers {
		switch strings.ToLower(m) {
		case "alt", "option":
			modifiers |= 1
		case "ctrl", "control":
			modifiers |= 2
		case "meta", "cmd", "command":
			modifiers |= 4
		case "shift":
			modifiers |= 8
		}
	}
	count := p.Count
	if count < 1 {
		count = 1
	}
	if count > 200 {
		count = 200
	}
	for rep := 0; rep < count; rep++ {
		if err := b.pressOnce(p.Key, modifiers); err != nil {
			return nil, "", err
		}
	}
	return json.RawMessage(fmt.Sprintf(`{"pressed":%s,"count":%d,"trusted":true}`, jsStr(p.Key), count)), "", nil
}

// modifierKeys are the physical modifier keys, pressed DOWN before the main
// key and released after — exactly like a real keyboard. Editors track the
// modifier keydown itself; only setting flag bits on the main key never
// formed selections (Shift+Arrow/Home/End) in controlled editors.
var modifierKeys = []struct {
	bit int
	key string
	vk  int
}{
	{1, "Alt", 18}, {2, "Control", 17}, {4, "Meta", 91}, {8, "Shift", 16},
}

// pressOnce dispatches one full trusted key press, wrapping it in physical
// modifier keyDown/keyUp events when modifiers are set.
func (b *Browser) pressOnce(key string, modifiers int) error {
	held := 0
	for _, mk := range modifierKeys {
		if modifiers&mk.bit != 0 {
			ev := map[string]any{"type": "keyDown", "key": mk.key, "modifiers": held,
				"windowsVirtualKeyCode": mk.vk, "nativeVirtualKeyCode": mk.vk}
			if _, err := b.Page("Input.dispatchKeyEvent", ev); err != nil {
				return err
			}
			held |= mk.bit
		}
	}
	err := b.pressMainKey(key, held)
	for i := len(modifierKeys) - 1; i >= 0; i-- {
		mk := modifierKeys[i]
		if modifiers&mk.bit != 0 {
			held &^= mk.bit
			ev := map[string]any{"type": "keyUp", "key": mk.key, "modifiers": held,
				"windowsVirtualKeyCode": mk.vk, "nativeVirtualKeyCode": mk.vk}
			_, _ = b.Page("Input.dispatchKeyEvent", ev)
		}
	}
	return err
}

// pressMainKey dispatches the non-modifier key with the (already physically
// held) modifier bits applied.
func (b *Browser) pressMainKey(key string, modifiers int) error {
	if _, ok := keyCodes[key]; ok {
		return b.pressRawKey(key, modifiers)
	}
	if runes := []rune(key); len(runes) == 1 {
		return b.pressCharKey(key, runes[0], modifiers)
	}
	return b.pressRawKey(key, modifiers)
}

// shortcutModifiers are the modifier bits that make a keystroke a COMMAND
// rather than typing. Shift is deliberately absent: Shift+a is still the
// character "A" and must carry text, while Cmd+a must not or the page receives
// an "a" instead of a select-all.
const shortcutModifiers = 1 | 2 | 4 // alt | ctrl | meta

// pressCharKey dispatches one printable character as a real keyDown/keyUp pair.
//
// F-46: this used to be two branches, and the no-modifier one sent a BARE `char`
// event. Measured against real Chrome, that produces a `keypress` and NOTHING
// ELSE — no keydown, no keyup. Every page that listens for keydown (editors,
// shortcut handlers, terminal emulators, and the web pane's own test page) sees
// exactly nothing, while press_key returns {"pressed":"a","trusted":true} and a
// nil error. Typing into a web pane silently did nothing, and the success
// report is why it went unnoticed.
//
// The modifier branch was already the right shape and is now the only shape.
// Measured: keyDown{key,text,code,vkey} + keyUp{key,code,vkey} yields
// keydown + keypress + keyup and then goes quiet.
func (b *Browser) pressCharKey(key string, r rune, modifiers int) error {
	down := map[string]any{"type": "keyDown", "key": key, "modifiers": modifiers}
	up := map[string]any{"type": "keyUp", "key": key, "modifiers": modifiers}

	if vk := charVirtualKey(r); vk != 0 {
		down["windowsVirtualKeyCode"] = vk
		down["nativeVirtualKeyCode"] = vk
		up["windowsVirtualKeyCode"] = vk
		up["nativeVirtualKeyCode"] = vk
	}
	// `code` is the PHYSICAL key. Only letters and digits get one: every other
	// printable character sits at a layout-dependent position, and a guessed
	// code is worse than none for a page that reads event.code.
	if code := charPhysicalCode(r); code != "" {
		down["code"] = code
		up["code"] = code
	}
	// text is what actually types the character. Withheld for shortcut combos,
	// where the page wants the accelerator and not an inserted glyph — that is
	// the one thing the old modifier branch got right.
	if modifiers&shortcutModifiers == 0 {
		down["text"] = key
	}

	if _, err := b.Page("Input.dispatchKeyEvent", down); err != nil {
		return err
	}
	// The keyUp is not optional bookkeeping: a key that is never released stays
	// down for the page's own key-state tracking.
	_, err := b.Page("Input.dispatchKeyEvent", up)
	return err
}

// charVirtualKey is the Windows virtual key code for a printable character, or
// 0 where there is no stable one. Letters map to their UPPERCASE code — a
// virtual key names the physical key, not the character it produced.
func charVirtualKey(r rune) int {
	switch {
	case r >= '0' && r <= '9':
		return int(r)
	case r >= 'a' && r <= 'z':
		return int(r) - 'a' + 'A'
	case r >= 'A' && r <= 'Z':
		return int(r)
	}
	return 0
}

// charPhysicalCode is the DOM `code` for a printable character on a US layout.
func charPhysicalCode(r rune) string {
	switch {
	case r >= '0' && r <= '9':
		return "Digit" + string(r)
	case r >= 'a' && r <= 'z':
		return "Key" + string(r-'a'+'A')
	case r >= 'A' && r <= 'Z':
		return "Key" + string(r)
	}
	return ""
}

func (b *Browser) doDragDrop(p actionParams) (json.RawMessage, string, error) {
	return b.evalResult(fmt.Sprintf(`(()=>{%s;
  const from = __ryshFind(%s, "", 0), to = __ryshFind(%s, "", 0);
  if (!from) throw new Error("drag source not found: " + %s);
  if (!to) throw new Error("drop target not found: " + %s);
  const dt = new DataTransfer();
  for (const [el, types] of [[from, ["dragstart"]], [to, ["dragenter","dragover","drop"]], [from, ["dragend"]]])
    for (const t of types)
      el.dispatchEvent(new DragEvent(t, {bubbles: true, cancelable: true, dataTransfer: dt}));
  return {dragged: __ryshDescribe(from), onto: __ryshDescribe(to)};})()`,
		findJS, jsStr(p.FromSelector), jsStr(p.ToSelector), jsStr(p.FromSelector), jsStr(p.ToSelector)))
}

func (b *Browser) doScroll(p actionParams) (json.RawMessage, string, error) {
	amount := p.Amount
	if amount == 0 {
		amount = 500
	}
	dx, dy := 0, amount
	switch strings.ToLower(p.Direction) {
	case "up":
		dy = -amount
	case "left":
		dx, dy = -amount, 0
	case "right":
		dx, dy = amount, 0
	}
	return b.evalResult(fmt.Sprintf(`(()=>{%s;
  const target = %s ? __ryshFind(%s, "", 0) : null;
  (target || window).scrollBy(%d, %d);
  const y = target ? target.scrollTop : window.scrollY;
  return {scrolled: {x: %d, y: %d}, position: y};})()`,
		findJS, jsStr(p.Selector), jsStr(p.Selector), dx, dy, dx, dy))
}

func (b *Browser) doWait(p actionParams) (json.RawMessage, string, error) {
	timeout := time.Duration(p.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	if p.Selector == "" {
		time.Sleep(timeout)
		return json.RawMessage(fmt.Sprintf(`{"waited_ms":%d}`, timeout.Milliseconds())), "", nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		v, err := b.evaluate(fmt.Sprintf(`(()=>{%s; const el = __ryshFind(%s, "", 0);
  if (!el) return false;
  if (%t) { const r = el.getBoundingClientRect(); return r.width > 0 && r.height > 0; }
  return true;})()`, findJS, jsStr(p.Selector), p.Visible))
		if err == nil && string(v) == "true" {
			return json.RawMessage(fmt.Sprintf(`{"found":%s}`, jsStr(p.Selector))), "", nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil, "", fmt.Errorf("wait: element %q not found within %s", p.Selector, timeout)
}

func (b *Browser) doGetText(p actionParams) (json.RawMessage, string, error) {
	return b.evalResult(fmt.Sprintf(`(()=>{%s;
  const el = %s ? __ryshFind(%s, "", %d) : document.body;
  if (!el) throw new Error("element not found: " + %s);
  return {url: location.href, title: document.title, text: (el.innerText || "").slice(0, 100000)};})()`,
		findJS, jsStr(p.Selector), jsStr(p.Selector), p.Index, jsStr(p.Selector)))
}

func (b *Browser) doGetHTML(p actionParams) (json.RawMessage, string, error) {
	return b.evalResult(fmt.Sprintf(`(()=>{%s;
  const el = %s ? __ryshFind(%s, "", %d) : document.documentElement;
  if (!el) throw new Error("element not found: " + %s);
  const html = (%t || !%s) ? el.outerHTML : el.innerHTML;
  return {html: html.slice(0, 150000)};})()`,
		findJS, jsStr(p.Selector), jsStr(p.Selector), p.Index, jsStr(p.Selector), p.Outer, jsStr(p.Selector)))
}

func (b *Browser) doGetElements(p actionParams) (json.RawMessage, string, error) {
	limit := p.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	attrs, _ := json.Marshal(p.Attributes)
	return b.evalResult(fmt.Sprintf(`(()=>{%s;
  const attrs = %s || [];
  const out = [];
  for (let i = 0; ; i++) {
    const el = __ryshFind(%s, %s, i);
    if (!el || out.length >= %d) break;
    const d = __ryshDescribe(el);
    for (const a of attrs) d[a] = el.getAttribute(a);
    out.push(d);
  }
  return {count: out.length, elements: out};})()`,
		findJS, string(attrs), jsStr(p.Selector), jsStr(p.Text), limit))
}

func (b *Browser) doScreenshot(p actionParams) (json.RawMessage, string, error) {
	// Default stays PNG so the model-facing `screenshot` action is unchanged;
	// only an explicit format (the recorder) opts into lossy capture.
	params := map[string]any{"format": "png"}
	switch strings.ToLower(strings.TrimSpace(p.Format)) {
	case "jpeg", "jpg":
		params["format"] = "jpeg"
		if p.Quality > 0 && p.Quality <= 100 {
			params["quality"] = p.Quality
		}
	case "webp":
		params["format"] = "webp"
		if p.Quality > 0 && p.Quality <= 100 {
			params["quality"] = p.Quality
		}
	}
	res, err := b.Page("Page.captureScreenshot", params)
	if err != nil {
		return nil, "", err
	}
	var out struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, "", err
	}
	return json.RawMessage(`{"screenshot":"captured"}`), out.Data, nil
}

func (b *Browser) doGetTabs() (json.RawMessage, string, error) {
	pages, err := b.pages()
	if err != nil {
		return nil, "", err
	}
	current := b.CurrentTargetID()
	type tab struct {
		TabID  string `json:"tab_id"`
		Index  int    `json:"index"`
		URL    string `json:"url"`
		Title  string `json:"title"`
		Active bool   `json:"active"`
	}
	tabs := make([]tab, 0, len(pages))
	for i, t := range pages {
		tabs = append(tabs, tab{TabID: t.TargetID, Index: i, URL: t.URL, Title: t.Title, Active: t.TargetID == current})
	}
	data, _ := json.Marshal(map[string]any{"tabs": tabs})
	return data, "", nil
}

func (b *Browser) doSwitchTab(p actionParams) (json.RawMessage, string, error) {
	pages, err := b.pages()
	if err != nil {
		return nil, "", err
	}
	var target *targetInfo
	switch {
	case p.TabID != "":
		for i := range pages {
			if pages[i].TargetID == p.TabID {
				target = &pages[i]
			}
		}
	case p.URLPattern != "":
		pat := strings.ToLower(strings.Trim(p.URLPattern, "*"))
		for i := range pages {
			if strings.Contains(strings.ToLower(pages[i].URL), pat) {
				target = &pages[i]
				break
			}
		}
	default:
		if p.Index >= 0 && p.Index < len(pages) {
			target = &pages[p.Index]
		}
	}
	if target == nil {
		return nil, "", fmt.Errorf("switch_tab: no matching tab (tab_id=%q index=%d url_pattern=%q)", p.TabID, p.Index, p.URLPattern)
	}
	if err := b.attach(target.TargetID); err != nil {
		return nil, "", err
	}
	data, _ := json.Marshal(map[string]any{"switched": map[string]string{"tab_id": target.TargetID, "url": target.URL, "title": target.Title}})
	return data, "", nil
}

func (b *Browser) doNewTab(p actionParams) (json.RawMessage, string, error) {
	url := p.URL
	if url == "" {
		url = "about:blank"
	}
	res, err := b.call("", "Target.createTarget", map[string]any{"url": url})
	if err != nil {
		return nil, "", err
	}
	var out struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, "", err
	}
	if err := b.attach(out.TargetID); err != nil {
		return nil, "", err
	}
	b.waitReady(10 * time.Second)
	data, _ := json.Marshal(map[string]string{"tab_id": out.TargetID, "url": url})
	return data, "", nil
}

func (b *Browser) doCloseTab(p actionParams) (json.RawMessage, string, error) {
	target := p.TabID
	if target == "" {
		target = b.CurrentTargetID()
	}
	if _, err := b.call("", "Target.closeTarget", map[string]any{"targetId": target}); err != nil {
		return nil, "", err
	}
	// Re-attach to a surviving page so the session stays usable.
	if target == b.CurrentTargetID() {
		if err := b.attachFirstPage(); err != nil {
			return nil, "", fmt.Errorf("closed tab but could not attach to another: %w", err)
		}
	}
	data, _ := json.Marshal(map[string]string{"closed": target})
	return data, "", nil
}
