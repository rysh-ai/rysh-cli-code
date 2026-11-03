(function () {
  "use strict";
  // Renders the embedded run record with textContent only. No network, no
  // HTML parsing of record data.
  var app = document.getElementById("app");
  function el(tag, text, cls) {
    var e = document.createElement(tag);
    if (text !== undefined && text !== null) e.textContent = String(text);
    if (cls) e.className = cls;
    return e;
  }
  function clear(n) { while (n.firstChild) n.removeChild(n.firstChild); }
  function kv(grid, k, v) {
    var d = el("div", null, "kv");
    d.appendChild(el("b", k));
    d.appendChild(el("span", v));
    grid.appendChild(d);
  }
  function usd(micro) { return "$" + (Number(micro || 0) / 1e6).toFixed(4); }
  function table(cols, rows) {
    var t = el("table"), h = el("tr");
    cols.forEach(function (c) { h.appendChild(el("th", c)); });
    t.appendChild(h);
    rows.forEach(function (r) {
      var tr = el("tr");
      r.forEach(function (c) { tr.appendChild(el("td", c)); });
      t.appendChild(tr);
    });
    return t;
  }

  var rec;
  try {
    rec = JSON.parse(document.getElementById("run-record").textContent);
  } catch (e) {
    clear(app);
    app.appendChild(el("p", "This run record could not be read.", "bad"));
    return;
  }
  var a = rec.audit || {};
  clear(app);

  app.appendChild(el("h1", "Run record — " + (a.session || "")));
  app.appendChild(el("p", "rysh " + (rec.rysh_version || "?") + " · created " + (rec.created_at || "?"), "muted"));
  app.appendChild(el("div", "Not signed: this record carries no integrity or tamper evidence. Secrets were redacted by pattern detectors only; review before sharing.", "banner"));

  var g = el("div", null, "grid");
  kv(g, "Status", (a.status || "?") + " (exit " + a.exit_code + ")");
  kv(g, "Provider", a.provider || "?");
  kv(g, "Duration", ((a.duration_ms || 0) / 1000).toFixed(1) + " s");
  kv(g, "Tokens", (a.usage && a.usage.total_tokens) || 0);
  kv(g, "Cost", usd(a.usage && a.usage.cost_micro_usd));
  kv(g, "Base commit", (rec.repo && rec.repo.base_commit) || "none");
  app.appendChild(g);
  if (rec.repo && rec.repo.base_note) app.appendChild(el("p", rec.repo.base_note, "muted"));
  if (a.detail) app.appendChild(el("p", a.detail, "muted"));

  app.appendChild(el("h2", "Prompt"));
  app.appendChild(el("pre", a.prompt || ""));

  app.appendChild(el("h2", "Tool calls (" + (a.tool_calls || []).length + ")"));
  app.appendChild(table(["#", "Tool", "Input", "Result", "ms"], (a.tool_calls || []).map(function (c, i) {
    return [i + 1, c.tool + (c.depth ? " (depth " + c.depth + ")" : ""), c.input_summary || "",
      c.incomplete ? "incomplete" : (c.result_summary || ""), c.duration_ms || ""];
  })));

  app.appendChild(el("h2", "Approvals (" + (a.approvals || []).length + ")"));
  app.appendChild(table(["Type", "Description", "Policy rule", "Decision"], (a.approvals || []).map(function (p) {
    return [p.type, p.description, p.policy_rule || "", p.decision];
  })));

  app.appendChild(el("h2", "Cost and budget"));
  var u = a.usage || {}, b = a.budget || {};
  app.appendChild(table(["In", "Out", "Cache read", "Cache write", "Total", "Cost", "Ceiling", "Exhausted"], [[
    u.in_tokens || 0, u.out_tokens || 0, u.cache_read || 0, u.cache_write || 0, u.total_tokens || 0,
    usd(u.cost_micro_usd), b.ceiling_tokens ? b.ceiling_tokens + " tokens" : (b.ceiling_micro_usd ? usd(b.ceiling_micro_usd) : "none"),
    b.exhausted ? "yes" : "no"]]));

  app.appendChild(el("h2", "Diff"));
  var d = a.diff || {};
  app.appendChild(el("p", "Changed: " + ((d.changed_paths || []).join(", ") || "nothing") + (d.note ? " — " + d.note : ""), "muted"));
  if (d.stat) app.appendChild(el("pre", d.stat));
  if (d.patch) app.appendChild(el("pre", d.patch + (d.patch_truncated ? "\n… (truncated)" : "")));

  app.appendChild(el("h2", "Replay"));
  var r = rec.replay || {};
  app.appendChild(el("p", r.available ? ("Replayable from " + r.transcript_entries + " recorded provider turn(s).") : "Not replayable.", r.available ? "ok" : "muted"));
  if (r.command) app.appendChild(el("pre", r.command));
  if (r.note) app.appendChild(el("p", r.note, "muted"));
  app.appendChild(el("p", "Replay re-runs the recorded tool calls on your machine, under rysh's policy and fail-closed approval gates.", "muted"));

  app.appendChild(el("h2", "Redaction"));
  var red = rec.redaction || {}, hits = red.detector_hits || {};
  var names = Object.keys(hits).sort();
  app.appendChild(el("p", (names.length ? names.map(function (k) { return k + ": " + hits[k]; }).join(", ") : "no detector matches") +
    " · home paths masked: " + (red.home_paths_masked || 0) + " · username masked: " + (red.username_masked || 0), "muted"));

  app.appendChild(el("h2", "Limits"));
  var ul = el("ul");
  (rec.limits || []).forEach(function (l) { ul.appendChild(el("li", l)); });
  app.appendChild(ul);
})();
