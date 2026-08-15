// SPDX-License-Identifier: Apache-2.0

package main

// Title lab (-title-lab): empirically determine which trusted-input strategy
// makes Medium's title field REGISTER in its model (the `is-defaultValue`
// ghost class must drop and the text render black), not just mutate the DOM.
// Each strategy starts from a fresh https://medium.com/new-story load.
//
//	A  caret via JS focus/selection  + Input.insertText (current harness path)
//	B  caret via JS focus/selection  + per-char trusted key events
//	C  trusted MOUSE CLICK on the title + per-char trusted key events
//	D  trusted mouse click            + Input.insertText

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/cdp"
)

func titleState(b *cdp.Browser) string {
	p, _ := json.Marshal(map[string]string{"code": `(function(){
  var h = document.querySelector("h3");
  if (!h) return "NO-H3";
  var ghost = (h.className||"").indexOf("defaultValue") !== -1;
  return (ghost ? "GHOST" : "REGISTERED") + " text=" + JSON.stringify((h.textContent||"").slice(0,40));
})()`})
	res, _, _ := b.Do("execute_js", json.RawMessage(p))
	return string(res)
}

func freshEditor(b *cdp.Browser) {
	nav, _ := json.Marshal(map[string]string{"url": "https://medium.com/new-story"})
	_, _, _ = b.Do("navigate", json.RawMessage(nav))
	time.Sleep(4 * time.Second)
}

func focusTitleJS(b *cdp.Browser) {
	p, _ := json.Marshal(map[string]string{"code": `(function(){
  var h = document.querySelector("h3");
  if (!h) return "no-h3";
  var host = h.closest('[contenteditable="true"]') || h;
  host.focus();
  var r = document.createRange();
  r.selectNodeContents(h);
  r.collapse(true);
  var s = window.getSelection();
  s.removeAllRanges();
  s.addRange(r);
  return "focused";
})()`})
	_, _, _ = b.Do("execute_js", json.RawMessage(p))
	time.Sleep(300 * time.Millisecond)
}

func clickTitleTrusted(b *cdp.Browser) error {
	p, _ := json.Marshal(map[string]string{"code": `(function(){
  var h = document.querySelector("h3");
  if (!h) return "";
  var r = h.getBoundingClientRect();
  return JSON.stringify({x: r.left + 40, y: r.top + r.height/2});
})()`})
	res, _, err := b.Do("execute_js", json.RawMessage(p))
	if err != nil {
		return err
	}
	var wrap struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(res, &wrap); err != nil || wrap.Result == "" {
		return fmt.Errorf("no title rect: %s", string(res))
	}
	var pt struct{ X, Y float64 }
	if err := json.Unmarshal([]byte(wrap.Result), &pt); err != nil {
		return err
	}
	for _, ev := range []string{"mousePressed", "mouseReleased"} {
		if _, err := b.Page("Input.dispatchMouseEvent", map[string]any{
			"type": ev, "x": pt.X, "y": pt.Y, "button": "left", "clickCount": 1,
		}); err != nil {
			return err
		}
	}
	time.Sleep(300 * time.Millisecond)
	return nil
}

func typePerChar(b *cdp.Browser, text string) {
	for _, ch := range text {
		k := string(ch)
		if k == " " {
			k = "Space"
		}
		kp, _ := json.Marshal(map[string]string{"key": k})
		_, _, _ = b.Do("press_key", json.RawMessage(kp))
	}
}

func insertText(b *cdp.Browser, text string) {
	_, _ = b.Page("Input.insertText", map[string]any{"text": text})
}

func runTitleLab(profile string, headless bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	b, err := cdp.Launch(ctx, cdp.LaunchOptions{
		UserDataDir: profile, Headless: headless, URL: "https://medium.com/new-story",
	})
	if err != nil {
		fmt.Println("launch:", err)
		os.Exit(1)
	}
	defer b.Close()
	time.Sleep(4 * time.Second)

	type strategy struct {
		name string
		run  func(text string)
	}
	strategies := []strategy{
		{"A js-caret + insertText", func(t string) { focusTitleJS(b); insertText(b, t) }},
		{"B js-caret + per-char keys", func(t string) { focusTitleJS(b); typePerChar(b, t) }},
		{"C trusted-click + per-char keys", func(t string) { _ = clickTitleTrusted(b); typePerChar(b, t) }},
		{"D trusted-click + insertText", func(t string) { _ = clickTitleTrusted(b); insertText(b, t) }},
	}

	fmt.Println("=== Medium title lab ===")
	for i, s := range strategies {
		if i > 0 {
			freshEditor(b)
		}
		before := titleState(b)
		s.run(fmt.Sprintf("Lab %s %s", strings.Fields(s.name)[0], time.Now().Format("15:04:05")))
		time.Sleep(1200 * time.Millisecond) // let the model/render tick settle
		after := titleState(b)
		verdict := "NOT-REGISTERED"
		if strings.Contains(after, "REGISTERED") && strings.Contains(after, "Lab") {
			verdict = "REGISTERED ✓"
		}
		fmt.Printf("%-32s before=%s after=%s => %s\n", s.name, firstN(before, 40), firstN(after, 70), verdict)
	}
	fmt.Println("NOTE: each strategy leaves a draft shell; check /me/stories/drafts to tidy.")
}
