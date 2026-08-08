package actors

import (
	"strings"
	"testing"
)

func upstreamCmd(t *testing.T, w *WorkspaceActor, args ...string) string {
	t.Helper()
	var out strings.Builder
	w.handleUpstreamCommand(&out, "", args)
	return out.String()
}

// TestUpstreamCommand_BareIsStatus pins that `##upstream` alone reports status.
func TestUpstreamCommand_BareIsStatus(t *testing.T) {
	w := &WorkspaceActor{}
	bare := upstreamCmd(t, w)
	explicit := upstreamCmd(t, w, "status")

	if bare != explicit {
		t.Errorf("bare ##upstream differs from ##upstream status:\n bare: %q\n status: %q", bare, explicit)
	}
}

// TestUpstreamCommand_StatusWhenDisabled pins the disabled report, including
// the line telling the user which config key to set. That hint is the whole
// value of the message.
func TestUpstreamCommand_StatusWhenDisabled(t *testing.T) {
	out := upstreamCmd(t, &WorkspaceActor{}, "status")

	if !strings.Contains(out, "upstream is not enabled") {
		t.Errorf("expected the disabled report, got:\n%s", out)
	}
	if !strings.Contains(out, "upstream.enabled: true in rysh.config.yaml") {
		t.Errorf("the disabled report must name the config key to set:\n%s", out)
	}
	// It must NOT fall through and print the status table.
	if strings.Contains(out, "shared panes") {
		t.Errorf("disabled upstream printed the status table:\n%s", out)
	}
}

// TestUpstreamCommand_StatusWhenEnabled pins the status table, including the
// shared-pane count. That count used to be a hand-rolled lane/group/pane walk
// and is now domain.PaneIDsInTabWhere with a !Sharing predicate, reading the
// REJECTED count — so a wrong predicate direction would show every unshared
// pane as shared. With no tabs the answer is 0 either way, which is exactly
// the case a sign error would still pass, so the field's presence and
// alignment are asserted alongside it.
func TestUpstreamCommand_StatusWhenEnabled(t *testing.T) {
	w := &WorkspaceActor{}
	w.cfg.Upstream.Enabled = true
	w.cfg.Upstream.URL = "nats://example.invalid:4222"
	w.cfg.Upstream.AutoShare = true

	out := upstreamCmd(t, w, "status")

	for _, want := range []string{
		"upstream status",
		"  enabled      : true",
		"  url          : nats://example.invalid:4222",
		"  auto_share   : true",
		"  shared panes : 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "upstream is not enabled") {
		t.Errorf("enabled upstream printed the disabled report:\n%s", out)
	}
}

func TestUpstreamCommand_UnknownSubcommand(t *testing.T) {
	out := upstreamCmd(t, &WorkspaceActor{}, "wibble")

	if !strings.Contains(out, `unknown subcommand for ##upstream: "wibble"`) {
		t.Errorf("expected the unknown-subcommand line, got:\n%s", out)
	}
	for _, want := range []string{
		"##upstream status",
		"##upstream my-shares",
		"##upstream list-remote",
		"##upstream subscribe <shareID>",
		"##upstream unsubscribe",
		"##upstream send <text>",
		"####<command>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q:\n%s", want, out)
		}
	}
}
