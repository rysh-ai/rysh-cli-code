package actors

// ##secret — the secret flavour of the named-value store (see store.go for the
// shared machinery). Secrets are SecretNAT-protected: the LLM provider only ever
// sees the ${NAME} token, never the real value, and every mutation re-syncs the
// SecretNAT known tier (secretnat_cmd.go). Storage is .rysh/secrets.

import (
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"
)

// newSecretStore builds the secret store over the (possibly nil) session KV
// bucket and the config-declared, workspace-scoped secrets ([secrets] section).
func newSecretStore(kv nats.KeyValue, cfgSecrets map[string]map[string]string) *namedStore {
	return newNamedStore(secretKind, kv, cfgSecrets)
}

// writePersistedSecret writes a secret to .rysh/secrets/<scope>/<NAME>. Thin
// wrapper over the kind-generic persistence, retained for direct callers/tests.
func writePersistedSecret(scope, name, value string) (string, error) {
	return secretKind.writePersisted(scope, name, value)
}

// handleSecretSubcommand handles the ##secret (alias ##secrets) system command.
// It delegates to the shared named-store handler, wiring in the secret store and
// the SecretNAT known-tier sync (pushKnownSecrets) that runs after every mutation
// so a new/deleted secret translates to/from its ${NAME} token from the next
// prompt on.
func (w *WorkspaceActor) handleSecretSubcommand(out *strings.Builder, paneID string, parts []string) {
	w.handleNamedStoreSubcommand(out, paneID, parts, namedStoreCmd{
		store: w.secrets,
		// Refresh the SecretNAT known tier AND re-check snat.required (design
		// 013): registering a required secret clears its fail-closed block.
		afterMut: w.afterSecretMutation,
	})
}

// maskSecretCommandEcho rewrites a "##secret new|set|add <NAME> <VALUE>" command
// line so the secret VALUE is never echoed into the pane output or recorded in
// history, while keeping --persist/--no-persist flags visible. The "##secrets"
// (plural) alias is handled too. Every other command line is returned unchanged
// — notably ##variable, whose values are plain config and intentionally visible.
// fullText includes the leading "##".
func maskSecretCommandEcho(fullText string) string {
	body := strings.TrimPrefix(strings.TrimSpace(fullText), "##")
	fields := strings.Fields(body)
	if len(fields) < 4 || (fields[0] != "secret" && fields[0] != "secrets") {
		return fullText
	}
	switch fields[1] {
	case "new", "set", "add":
	default:
		return fullText
	}
	// The first non-flag token after the subcommand is the NAME; everything else
	// is the secret value (masked). The --persist/--no-persist and --tab <name>
	// flags are non-secret and preserved for clarity.
	name := ""
	var flags []string
	args := fields[2:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--persist" || a == "-p":
			flags = append(flags, "--persist")
		case a == "--no-persist":
			flags = append(flags, "--no-persist")
		case a == "--tab" || a == "-t":
			if i+1 < len(args) {
				flags = append(flags, "--tab", args[i+1])
				i++
			} else {
				flags = append(flags, "--tab")
			}
		case strings.HasPrefix(a, "--tab="):
			flags = append(flags, a)
		default:
			if name == "" {
				name = a
			}
			// Any further non-flag tokens are value parts: masked, not emitted.
		}
	}
	if name == "" {
		return fullText
	}
	masked := fmt.Sprintf("##%s %s %s ****", fields[0], fields[1], name)
	if len(flags) > 0 {
		masked += " " + strings.Join(flags, " ")
	}
	return masked
}
