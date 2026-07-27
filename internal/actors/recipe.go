package actors

import (
	"fmt"
	"regexp"
	"strings"
)

// The "recipe" view behind `##agent show <name>` and `##humanoid show <name>`:
// the skill file exactly as written — frontmatter plus the system prompt body —
// so the definition an entity was spawned from can be read without leaving the
// session. Agents and humanoids share the renderer; only the tag, the separator
// width and the lookup directory differ.

// recipeSecretKeys are the frontmatter keys whose values are credentials. A
// literal value under one of these is masked before the recipe is printed: a
// pane can be shared (##share pane) or mirrored into the web UI, so echoing a
// live token there would widen its exposure well beyond the file on disk.
var recipeSecretKeys = map[string]bool{
	"access_token":   true,
	"account_sid":    true,
	"api_key":        true,
	"apikey":         true,
	"app_secret":     true,
	"app_token":      true,
	"auth_token":     true,
	"bot_token":      true,
	"client_secret":  true,
	"password":       true,
	"refresh_token":  true,
	"relay_api_key":  true,
	"secret":         true,
	"signing_secret": true,
	"token":          true,
	"verify_token":   true,
	"webhook_secret": true,
}

// recipeKeyValue matches a "key: value" frontmatter line, tolerating list-item
// ("- key: value") and indented forms.
var recipeKeyValue = regexp.MustCompile(`^(\s*(?:-\s+)?)([A-Za-z0-9_.-]+):[ \t]*(.*)$`)

// redactRecipeSecrets masks literal credential values in a frontmatter block.
// ${VAR} references pass through untouched — the placeholder is the part worth
// reading, and it names a secret rather than being one.
func redactRecipeSecrets(frontmatter string) string {
	lines := strings.Split(frontmatter, "\n")
	for i, line := range lines {
		m := recipeKeyValue.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		value := strings.TrimSpace(m[3])
		if value == "" || !recipeSecretKeys[strings.ToLower(m[2])] {
			continue
		}
		if isPlaceholderValue(value) {
			continue
		}
		lines[i] = m[1] + m[2] + ": <redacted>"
	}
	return strings.Join(lines, "\n")
}

// isPlaceholderValue reports whether a value is nothing but ${VAR} references
// (optionally quoted) — a pointer into the secret store, not a secret.
func isPlaceholderValue(v string) bool {
	v = strings.TrimSpace(strings.Trim(strings.TrimSpace(v), `"'`))
	if v == "" {
		return false
	}
	return strings.TrimSpace(envVarPattern.ReplaceAllString(v, "")) == ""
}

// renderRecipe writes an entity's recipe under tag ("agents" or "humanoids").
// path is the skill file the content came from; an empty path means the entity
// has no file on disk — it was spawned inline — and content is then the system
// prompt the registry holds. Kept pure (no registry/actor dependency) so it can
// be tested directly.
func renderRecipe(out *strings.Builder, tag, name, path string, loaded bool, content string, width int) {
	rule := strings.Repeat("-", width)
	fmt.Fprintf(out, "\n[%s] recipe for %q\n", tag, name)
	fmt.Fprintf(out, "%s\n", rule)
	if path != "" {
		fmt.Fprintf(out, "  source: %s\n", path)
	} else {
		fmt.Fprintf(out, "  source: none — spawned inline, no skill file on disk\n")
	}
	status := "not loaded in this workspace"
	if loaded {
		status = "loaded in this workspace"
	}
	fmt.Fprintf(out, "  status: %s\n", status)
	fmt.Fprintf(out, "%s\n", rule)

	if fm, body, ok := splitFrontmatter(content); ok {
		fmt.Fprintf(out, "---\n%s\n---\n", strings.TrimRight(redactRecipeSecrets(fm), "\n"))
		fmt.Fprintf(out, "%s\n", strings.TrimSpace(body))
	} else {
		fmt.Fprintf(out, "%s\n", strings.TrimSpace(content))
	}
	fmt.Fprintf(out, "%s\n\n", rule)
}
