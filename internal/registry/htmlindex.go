package registry

// The human-browsable half of the published registry (design 005, B9): a
// static index.html generated NEXT TO index.json by cmd/registry-index, so
// packages.rysh.ai has a landing page without any service. html/template
// escaping keeps package-supplied names/descriptions inert.

import (
	"bytes"
	"html/template"
	"sort"
)

var indexPageTmpl = template.Must(template.New("index").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>rysh package registry</title>
<style>
  body { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; margin: 2rem auto; max-width: 60rem; padding: 0 1rem; color: #1a1a1a; background: #fcfcfc; }
  h1 { font-size: 1.3rem; }
  table { border-collapse: collapse; width: 100%; }
  th, td { text-align: left; padding: .4rem .6rem; border-bottom: 1px solid #e2e2e2; vertical-align: top; }
  th { font-size: .8rem; text-transform: uppercase; letter-spacing: .05em; color: #666; }
  code { background: #f0f0f0; padding: .1rem .3rem; border-radius: 3px; }
  .muted { color: #777; font-size: .85rem; }
  @media (prefers-color-scheme: dark) {
    body { color: #ddd; background: #111; }
    th { color: #999; } th, td { border-color: #2a2a2a; }
    code { background: #222; }
  }
</style>
</head>
<body>
<h1>rysh package registry</h1>
<p class="muted">{{len .Rows}} package(s){{if .Generated}} · generated {{.Generated}}{{end}} ·
install with <code>rysh install &lt;name&gt;</code></p>
<table>
<tr><th>package</th><th>latest</th><th>type</th><th>description</th><th>versions</th></tr>
{{range .Rows}}<tr>
  <td><code>{{.Name}}</code></td>
  <td>{{.Latest}}</td>
  <td>{{.Type}}</td>
  <td>{{.Description}}</td>
  <td class="muted">{{.Versions}}</td>
</tr>
{{end}}</table>
<p class="muted">The machine-readable catalogue is <a href="index.json">index.json</a>.
Artifacts are checksum-verified at install; unsigned packages pass the consent
gate only with an explicit prompt (or <code>--force</code>).</p>
</body>
</html>
`))

// RenderIndexHTML renders the browsable landing page for an index.
func RenderIndexHTML(idx *Index) ([]byte, error) {
	type row struct {
		Name, Latest, Type, Description, Versions string
	}
	data := struct {
		Generated string
		Rows      []row
	}{Generated: idx.Generated}

	names := make([]string, 0, len(idx.Packages))
	for n := range idx.Packages {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := idx.Packages[n]
		vs := sortedVersions(p.Versions)
		// newest first reads better on a landing page
		for i, j := 0, len(vs)-1; i < j; i, j = i+1, j-1 {
			vs[i], vs[j] = vs[j], vs[i]
		}
		joined := ""
		for i, v := range vs {
			if i > 0 {
				joined += ", "
			}
			joined += v
		}
		data.Rows = append(data.Rows, row{
			Name: n, Latest: highestVersion(p.Versions), Type: p.Type,
			Description: p.Description, Versions: joined,
		})
	}

	var buf bytes.Buffer
	if err := indexPageTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
