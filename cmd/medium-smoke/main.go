// medium-smoke is a self-verifying trusted-input smoke test against Medium's
// story editor. It proves the CDP input path delivers isTrusted:true events
// that the controlled contenteditable accepts (no revert), WITHOUT publishing.
//
// Run (profile must already be logged in — the test STOPS on a login wall):
//
//	cd rysh-cli && GOWORK=off go run ./cmd/medium-smoke \
//	  -profile /path/to/.rysh/browser-instances/halilagin-medium/headless
//
// Assertions (PASS/FAIL each; exit 1 on any FAIL):
//  1. editor reached, no login wall
//  2. every injected event observed page-side has isTrusted === true
//  3. typed title survives readback
//  4. typed body paragraph survives readback AND a 1.2s re-render tick
//  5. "##" + discrete trusted Space converts the line to a real heading
//     element (per-keystroke fidelity, not just bulk insert)
//  6. Backspace works (trusted editing keys)
//
// Cleanup: select-all + Backspace in the body and title. Medium may still
// autosave an empty draft shell; that is reported, not hidden.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/cdp"
)

var failures int

func check(name string, ok bool, detail string) {
	if ok {
		fmt.Printf("PASS  %s\n", name)
	} else {
		failures++
		fmt.Printf("FAIL  %s — %s\n", name, detail)
	}
}

func main() {
	profile := flag.String("profile", "", "Chromium user-data-dir of the logged-in medium profile (required)")
	headless := flag.Bool("headless", false, "run headless (default headed so you can watch)")
	titleLab := flag.Bool("title-lab", false, "run the title-strategy lab instead of the smoke test")
	flag.Parse()
	if *profile == "" {
		fmt.Println("FAIL  -profile is required (the pre-authenticated browser profile dir)")
		os.Exit(1)
	}

	if *titleLab {
		runTitleLab(*profile, *headless)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	b, err := cdp.Launch(ctx, cdp.LaunchOptions{
		UserDataDir: *profile,
		Headless:    *headless,
		URL:         "https://medium.com/new-story",
	})
	if err != nil {
		fmt.Printf("FAIL  launch: %v\n", err)
		os.Exit(1)
	}
	defer b.Close()
	time.Sleep(4 * time.Second) // editor bootstrap

	do := func(action, params string) (string, bool) {
		res, _, err := b.Do(action, json.RawMessage(params))
		if err != nil {
			return err.Error(), false
		}
		return string(res), true
	}
	ex := func(code string) (string, bool) {
		p, _ := json.Marshal(map[string]string{"code": code})
		return do("execute_js", string(p))
	}

	// 1. Editor reached, logged in? (never auto-login / never touch captchas)
	page, _ := ex(`(document.body.innerText || "").slice(0, 4000)`)
	lower := strings.ToLower(page)
	if strings.Contains(lower, "sign in") || strings.Contains(lower, "sign up to continue") || strings.Contains(lower, "captcha") {
		check("editor reached (no login wall)", false, "login/captcha wall — log in once by hand, then re-run")
		os.Exit(1)
	}
	check("editor reached (no login wall)", true, "")

	// 2. Page-side isTrusted logger — installed BEFORE any input.
	_, ok := ex(`window.__rysh_trust = [];
["keydown","keyup","beforeinput","input"].forEach(function(t){
  document.addEventListener(t, function(e){ window.__rysh_trust.push([t, e.isTrusted]); }, true);
}); "armed"`)
	check("isTrusted logger armed", ok, "logger install failed")

	// 3. Title via trusted bulk insert.
	title := "Rysh trusted-input smoke " + time.Now().Format("15:04:05")
	// keystrokes mode is REQUIRED for the title: Medium's default "Title"
	// text clears only on real per-character key events (title lab, 2026-07-18).
	tp, _ := json.Marshal(map[string]any{"selector": "h3", "text": title, "clear": true, "keystrokes": true})
	res, ok := do("type", string(tp))
	check("type action succeeded (title)", ok, res)
	back, _ := ex(`(document.querySelector("h3") || {}).textContent || ""`)
	check("title readback matches", strings.Contains(back, "Rysh trusted-input smoke"), "got: "+back)
	check("title has NO default-text residue", !strings.Contains(back, "smoke Title") && !strings.HasSuffix(strings.TrimSpace(back), "Title"), "residue: "+back)

	// 4. Body paragraph: Enter into the body, then trusted insert.
	_, _ = do("press_key", `{"key":"Enter"}`)
	body := "The quick brown fox proves trusted input works."
	bp, _ := json.Marshal(map[string]any{"selector": "article p", "text": body, "clear": false})
	res, ok = do("type", string(bp))
	check("type action succeeded (body)", ok, res)
	read1, _ := ex(`(document.querySelector("article") || document.body).innerText`)
	check("body readback matches", strings.Contains(read1, "quick brown fox"), "got: "+firstN(read1, 200))

	// ...and SURVIVES a render tick (the revert signature check).
	time.Sleep(1200 * time.Millisecond)
	read2, _ := ex(`(document.querySelector("article") || document.body).innerText`)
	check("body survived a render tick (no revert)", strings.Contains(read2, "quick brown fox"), "reverted; now: "+firstN(read2, 200))

	// 5. Heading via Medium's REAL mechanism: Cmd+Alt+1 (or +2) on the line.
	// (Finding from the first smoke run: Medium has NO "## " markdown
	// shortcut — even fully trusted keystrokes leave literal hashes. The
	// recipe's toolbar/keyboard-shortcut path is the correct one; modifier
	// combos are only possible because press_key is now trusted.)
	_, _ = do("press_key", `{"key":"Enter"}`)
	for _, ch := range "Heading Line" {
		k := string(ch)
		if k == " " {
			k = "Space"
		}
		_, _ = do("press_key", `{"key":`+fmt.Sprintf("%q", k)+`}`)
	}
	time.Sleep(300 * time.Millisecond)
	_, _ = do("press_key", `{"key":"1","modifiers":["meta","alt"]}`)
	time.Sleep(600 * time.Millisecond)
	heading, _ := ex(`(function(){
  var hs = document.querySelectorAll("article h1, article h2, article h3, article h4");
  for (var i = 0; i < hs.length; i++) if ((hs[i].textContent||"").indexOf("Heading Line") !== -1) return "heading-element";
  return "still-paragraph";
})()`)
	if !strings.Contains(heading, "heading-element") {
		_, _ = do("press_key", `{"key":"2","modifiers":["meta","alt"]}`)
		time.Sleep(600 * time.Millisecond)
		heading, _ = ex(`(function(){
  var hs = document.querySelectorAll("article h1, article h2, article h3, article h4");
  for (var i = 0; i < hs.length; i++) if ((hs[i].textContent||"").indexOf("Heading Line") !== -1) return "heading-element";
  return "still-paragraph";
})()`)
	}
	check("Cmd+Alt+1/2 converts the line to a real heading", strings.Contains(heading, "heading-element"), "state: "+heading)

	// 6. Backspace works (trusted editing key): "Heading Line" -> "Heading Lin".
	_, _ = do("press_key", `{"key":"Backspace"}`)
	afterBs, _ := ex(`(document.querySelector("article")||document.body).innerText`)
	check("Backspace edits the editor", strings.Contains(afterBs, "Heading Lin") && !strings.Contains(afterBs, "Heading Line"), "unexpected text state")

	// 2b. Every observed event trusted?
	trust, _ := ex(`(function(){
  var a = window.__rysh_trust || [], untrusted = 0;
  for (var i = 0; i < a.length; i++) if (!a[i][1]) untrusted++;
  return "events=" + a.length + " untrusted=" + untrusted;
})()`)
	check("all injected events isTrusted:true", strings.Contains(trust, "untrusted=0") && !strings.Contains(trust, "events=0 "), trust)

	// Cleanup: select-all + Backspace (both trusted), title cleared via type.
	mod := "meta"
	if os.Getenv("RYSH_SMOKE_CTRL") != "" {
		mod = "ctrl"
	}
	_, _ = do("press_key", fmt.Sprintf(`{"key":"a","modifiers":[%q]}`, mod))
	_, _ = do("press_key", `{"key":"Backspace"}`)
	ct, _ := json.Marshal(map[string]any{"selector": "h3", "text": "", "clear": true})
	_, _ = do("type", string(ct))
	time.Sleep(800 * time.Millisecond)
	left, _ := ex(`((document.querySelector("article")||document.body).innerText||"").trim().length`)
	fmt.Printf("INFO  cleanup: remaining editor text length = %s (Medium may still keep an empty autosaved draft — check /me/stories/drafts)\n", left)

	if failures > 0 {
		fmt.Printf("\n%d FAILURE(S)\n", failures)
		os.Exit(1)
	}
	fmt.Println("\nALL PASS")
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
