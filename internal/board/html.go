// SPDX-License-Identifier: Apache-2.0

package board

// The shareable board report: one standalone HTML page (no external assets)
// that `##board output … html` opens and `… pdf` prints. It is the built-in
// form of the datalake prototype's .board_output.py and matches it visually.
//
// html/template, never string concatenation: post text is written by agents,
// and the template's contextual escaping is what guarantees a `<script>` in a
// post is text. Links are built as data (a segment with a URL) for the same
// reason — the template, not this file, writes the <a>.

import (
	"bytes"
	"fmt"
	"html/template"
	"regexp"
	"strings"
	"time"
)

// HTMLPage is what RenderHTML draws.
type HTMLPage struct {
	Board   string
	Worker  string
	N       int    // the count asked for; 0 = all
	Picked  []Item // oldest first
	Matched int    // the worker's items before N
	Total   int    // every item on the board window
	Now     time.Time
	Loc     *time.Location // nil = time.Local
}

var (
	urlRE   = regexp.MustCompile(`https?://[^\s<>()]+[^\s<>().,;:]`)
	fieldRE = regexp.MustCompile(`^\s*([A-Z][A-Z /&-]{1,30}):\s?(.*)$`)
)

// statusTones maps a STATUS value to a badge colour, first match wins.
var statusTones = []struct{ key, tone string }{
	{"blocked", "bad"}, {"waiting", "warn"}, {"progress", "info"},
	{"done", "good"}, {"idle", "muted"}, {"milestone", "good"},
}

// StatusTone is the badge class for a STATUS value: blocked → bad (red),
// waiting → warn (amber), progress → info (blue), done/milestone → good
// (green), idle or anything else → muted (grey).
func StatusTone(value string) string {
	v := strings.ToLower(value)
	for _, t := range statusTones {
		if strings.Contains(v, t.key) {
			return t.tone
		}
	}
	return "muted"
}

type htmlSeg struct{ Text, URL string }

type htmlField struct {
	Label string
	Value []htmlSeg
}

type htmlCard struct {
	Persona, Kind, ISO, When string
	Badge, Tone              string
	Loose                    []htmlSeg
	Fields                   []htmlField
}

func linkify(s string) []htmlSeg {
	var out []htmlSeg
	last := 0
	for _, m := range urlRE.FindAllStringIndex(s, -1) {
		if m[0] > last {
			out = append(out, htmlSeg{Text: s[last:m[0]]})
		}
		out = append(out, htmlSeg{URL: s[m[0]:m[1]]})
		last = m[1]
	}
	if last < len(s) {
		out = append(out, htmlSeg{Text: s[last:]})
	}
	return out
}

// parseFields splits a body into the lines before the first FIELD: line and
// the label/value grid. A line that is not a field continues the previous one.
func parseFields(text string) (loose string, fields []htmlField, status string) {
	type raw struct{ label, value string }
	var rows []raw
	var pre []string
	for _, line := range strings.Split(text, "\n") {
		if m := fieldRE.FindStringSubmatch(line); m != nil {
			label := strings.TrimSpace(m[1])
			if label == "STATUS" && status == "" {
				status = m[2]
			}
			rows = append(rows, raw{label, m[2]})
		} else if len(rows) > 0 {
			rows[len(rows)-1].value += "\n" + line
		} else {
			pre = append(pre, line)
		}
	}
	loose = strings.TrimSpace(strings.Join(pre, "\n"))
	for _, r := range rows {
		fields = append(fields, htmlField{Label: r.label, Value: linkify(strings.TrimSpace(r.value))})
	}
	return loose, fields, status
}

func badgeText(status string) string {
	head, _, _ := strings.Cut(status, " - ")
	r := []rune(head)
	if len(r) > 40 {
		r = r[:40]
	}
	return string(r)
}

// RenderHTML draws the page.
func RenderHTML(p HTMLPage) ([]byte, error) {
	loc := p.Loc
	if loc == nil {
		loc = time.Local
	}
	cards := make([]htmlCard, 0, len(p.Picked))
	for _, it := range p.Picked {
		post := it.Post
		when := time.UnixMilli(post.TS).In(loc)
		body := SanitiseBody(post.Text)
		loose, fields, status := parseFields(body)
		c := htmlCard{
			Persona: SanitiseInline(post.Persona),
			Kind:    SanitiseInline(post.Kind),
			ISO:     when.Format("2006-01-02T15:04:05"),
			When:    when.Format("2006-01-02 15:04:05"),
			Fields:  fields,
		}
		if c.Persona == "" {
			c.Persona = "?"
		}
		if loose != "" {
			c.Loose = linkify(loose)
		}
		if status != "" {
			c.Badge, c.Tone = badgeText(status), StatusTone(status)
		}
		cards = append(cards, c)
	}

	title := "Agent board · " + p.Board
	scope := ""
	if p.Worker != "" {
		title += " · " + p.Worker
		scope = p.Worker + " · "
	}
	window := fmt.Sprintf("%d messages", len(p.Picked))
	if p.N > 0 && p.Matched > len(p.Picked) {
		window = fmt.Sprintf("last %d of %d", len(p.Picked), p.Matched)
	}
	meta := fmt.Sprintf("%s%s · board has %d messages · generated %s",
		scope, window, p.Total, p.Now.In(loc).Format("2006-01-02 15:04"))

	var buf bytes.Buffer
	err := pageTmpl.Execute(&buf, struct {
		Title, Meta string
		Cards       []htmlCard
	}{title, meta, cards})
	return buf.Bytes(), err
}

var pageTmpl = template.Must(template.New("board").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root {
  --bg:#f6f7f9; --card:#ffffff; --fg:#1d232b; --muted:#5d6875; --line:#e1e5ea; --link:#1f5fbf;
  --good:#1f7a4d; --good-bg:#e3f4ea; --bad:#b3261e; --bad-bg:#fbe7e5; --warn:#8a5a00; --warn-bg:#fdf1d6;
  --info:#1f5fbf; --info-bg:#e5eefb; --mut:#5d6875; --mut-bg:#eceff3;
}
@media (prefers-color-scheme: dark) { :root:not([data-theme="light"]) {
  --bg:#12161b; --card:#1a2027; --fg:#e4e8ed; --muted:#98a3b0; --line:#2a323c; --link:#8ab4f8;
  --good:#7fd6a4; --good-bg:#16352a; --bad:#f2a49c; --bad-bg:#3d1c1a; --warn:#f0c674; --warn-bg:#3a2e14;
  --info:#8ab4f8; --info-bg:#172a45; --mut:#98a3b0; --mut-bg:#232a33; } }
* { box-sizing:border-box; }
body { margin:0; background:var(--bg); color:var(--fg);
  font:14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; }
main { max-width:960px; margin:0 auto; padding:24px 16px 48px; }
h1 { font-size:20px; margin:0 0 4px; }
.meta { color:var(--muted); margin:0 0 20px; font-size:13px; }
.card { background:var(--card); border:1px solid var(--line); border-radius:8px; padding:14px 16px;
  margin:0 0 12px; break-inside:avoid; page-break-inside:avoid; }
.card header { display:flex; flex-wrap:wrap; align-items:center; gap:8px; margin-bottom:8px; }
.persona { font-weight:600; font-family:ui-monospace, SFMono-Regular, Menlo, monospace; }
.kind { color:var(--muted); font-size:12px; }
time { margin-left:auto; color:var(--muted); font-size:12px; font-variant-numeric:tabular-nums; }
.badge { font-size:11px; font-weight:600; padding:2px 8px; border-radius:999px; text-transform:uppercase; letter-spacing:.02em; }
.good { color:var(--good); background:var(--good-bg); } .bad { color:var(--bad); background:var(--bad-bg); }
.warn { color:var(--warn); background:var(--warn-bg); } .info { color:var(--info); background:var(--info-bg); }
.muted { color:var(--mut); background:var(--mut-bg); }
dl { display:grid; grid-template-columns:max-content 1fr; gap:4px 14px; margin:0; }
dt { font-weight:600; font-size:12px; color:var(--muted); padding-top:1px; white-space:nowrap; }
dd { margin:0; white-space:pre-wrap; overflow-wrap:anywhere; }
.loose { margin:0 0 6px; white-space:pre-wrap; overflow-wrap:anywhere; }
a { color:var(--link); text-decoration:none; } a:hover { text-decoration:underline; }
@media (max-width:600px) { dl { grid-template-columns:1fr; } dt { margin-top:6px; } }
@media print {
  :root, :root:not([data-theme="light"]) {
    --bg:#ffffff; --card:#ffffff; --fg:#1d232b; --muted:#5d6875; --line:#cccccc; --link:#1f5fbf;
    --good:#1f7a4d; --good-bg:#e3f4ea; --bad:#b3261e; --bad-bg:#fbe7e5; --warn:#8a5a00; --warn-bg:#fdf1d6;
    --info:#1f5fbf; --info-bg:#e5eefb; --mut:#5d6875; --mut-bg:#eceff3; }
  body { background:#fff; } main { padding:0; max-width:none; } }
</style></head>
<body><main>
<h1>{{.Title}}</h1>
<p class="meta">{{.Meta}}</p>
{{range .Cards}}
<article class="card">
  <header>
    <span class="persona">{{.Persona}}</span>
    <span class="kind">{{.Kind}}</span>
    {{if .Badge}}<span class="badge {{.Tone}}">{{.Badge}}</span>{{end}}
    <time datetime="{{.ISO}}">{{.When}}</time>
  </header>
  {{if .Loose}}<p class="loose">{{template "segs" .Loose}}</p>{{end}}
  {{if .Fields}}<dl>{{range .Fields}}<dt>{{.Label}}</dt><dd>{{template "segs" .Value}}</dd>{{end}}</dl>{{end}}
</article>
{{end}}
</main></body></html>
{{define "segs"}}{{range .}}{{if .URL}}<a href="{{.URL}}">{{.URL}}</a>{{else}}{{.Text}}{{end}}{{end}}{{end}}
`))
