package cdp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLaunchArgs(t *testing.T) {
	args := launchArgs(LaunchOptions{UserDataDir: "/tmp/x", Headless: true, URL: "https://example.com"})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--remote-debugging-port=0", "--user-data-dir=/tmp/x", "--headless=new", "https://example.com"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
	// Headed launch omits the headless flag; empty URL becomes about:blank.
	headed := strings.Join(launchArgs(LaunchOptions{UserDataDir: "/tmp/x"}), " ")
	if strings.Contains(headed, "--headless") {
		t.Error("headed launch must not pass --headless")
	}
	if !strings.Contains(headed, "about:blank") {
		t.Error("empty URL should default to about:blank")
	}
}

func TestDevtoolsWaitTimeout(t *testing.T) {
	if d := devtoolsWaitTimeout(LaunchOptions{}); d != 20*time.Second {
		t.Errorf("default wait = %v, want 20s", d)
	}
	if d := devtoolsWaitTimeout(LaunchOptions{WaitTimeout: 50 * time.Second}); d != 50*time.Second {
		t.Errorf("override wait = %v, want 50s", d)
	}
	if d := devtoolsWaitTimeout(LaunchOptions{WaitTimeout: -time.Second}); d != 20*time.Second {
		t.Errorf("negative wait = %v, want default 20s", d)
	}
}

func TestActionParams_TabIDTolerance(t *testing.T) {
	var a actionParams
	if err := json.Unmarshal([]byte(`{"tab_id": 123}`), &a); err != nil || a.TabID != "123" {
		t.Errorf("numeric tab_id: %+v err=%v", a, err)
	}
	var b actionParams
	if err := json.Unmarshal([]byte(`{"tab_id": "target-abc"}`), &b); err != nil || b.TabID != "target-abc" {
		t.Errorf("string tab_id: %+v err=%v", b, err)
	}
}

func TestJSStr(t *testing.T) {
	got := jsStr(`he said "hi" </script>` + "\nnewline")
	if !strings.HasPrefix(got, `"`) || strings.Contains(got, "\n") {
		t.Errorf("jsStr must produce a single-line JS literal: %s", got)
	}
}

// TestIntegration_HeadlessBrowser exercises the real pipeline against a local
// Chromium: launch → navigate (data: URL) → observe → interact → screenshot →
// tabs. Skipped when no browser binary is available (CI without Chromium).
func TestIntegration_HeadlessBrowser(t *testing.T) {
	if FindChromium() == "" {
		t.Skip("no Chromium/Chrome binary available")
	}
	if testing.Short() {
		t.Skip("short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 50s wait inside the 60s ctx: the 20s default flaked on CI under -race
	// (cold Chromium start + race-instrumented siblings on a loaded runner).
	b, err := Launch(ctx, LaunchOptions{UserDataDir: t.TempDir(), Headless: true, WaitTimeout: 50 * time.Second})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer b.Close()

	page := `data:text/html,<html><head><title>rysh-test</title></head><body>` +
		`<h1 id="hd">Hello Rysh</h1>` +
		`<input id="name" placeholder="type here">` +
		`<button id="btn" data-testid="go" onclick="document.getElementById('hd').innerText='Clicked'">Go</button>` +
		`</body></html>`

	do := func(action, params string) json.RawMessage {
		t.Helper()
		res, _, err := b.Do(action, json.RawMessage(params))
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		return res
	}

	// navigate + get_text
	do("navigate", `{"url":`+jsStr(page)+`}`)
	txt := string(do("get_text", `{}`))
	if !strings.Contains(txt, "Hello Rysh") {
		t.Fatalf("get_text: %s", txt)
	}

	// type via CSS selector, read back via get_value
	do("type", `{"selector":"#name","text":"halil"}`)
	if v := string(do("get_value", `{"selector":"#name"}`)); !strings.Contains(v, "halil") {
		t.Fatalf("get_value: %s", v)
	}

	// click via testid: selector, verify the DOM changed (observe→act→verify)
	do("click", `{"selector":"testid:go"}`)
	if txt := string(do("get_text", `{"selector":"#hd"}`)); !strings.Contains(txt, "Clicked") {
		t.Fatalf("click did not take effect: %s", txt)
	}

	// get_elements with attributes
	els := string(do("get_elements", `{"selector":"button","attributes":["data-testid"]}`))
	if !strings.Contains(els, `"data-testid":"go"`) {
		t.Fatalf("get_elements: %s", els)
	}

	// execute_js
	if v := string(do("execute_js", `{"code":"1+41"}`)); !strings.Contains(v, "42") {
		t.Fatalf("execute_js: %s", v)
	}

	// screenshot returns base64 PNG data
	_, shot, err := b.Do("screenshot", json.RawMessage(`{}`))
	if err != nil || len(shot) < 100 {
		t.Fatalf("screenshot: len=%d err=%v", len(shot), err)
	}

	// tabs: new_tab → get_tabs(2) → switch back by index → close the extra
	do("new_tab", `{"url":"about:blank"}`)
	tabs := string(do("get_tabs", `{}`))
	if !strings.Contains(tabs, "rysh-test") {
		t.Fatalf("get_tabs should list the original page: %s", tabs)
	}
	do("switch_tab", `{"url_pattern":"*data*"}`)
	if txt := string(do("get_text", `{"selector":"#hd"}`)); !strings.Contains(txt, "Clicked") {
		t.Fatalf("switch_tab landed on wrong tab: %s", txt)
	}
}
