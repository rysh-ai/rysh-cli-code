package plugin

// End-to-end check on the SHIPPED example (examples/plugins/echo-channel):
// compile it with the real Go toolchain, install-shape it into a registry
// root, and drive it through the real adapter — Start, send→echo round-trip,
// clean Stop. This is the "the authoring kit actually works" test: if the
// example drifts from the wire contract or stops building, this fails.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// moduleRoot walks up from the package dir to the go.mod directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above package dir")
		}
		dir = parent
	}
}

func TestExampleEchoPluginEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the example plugin with the go toolchain")
	}
	root := moduleRoot(t)
	pluginDir := filepath.Join(t.TempDir(), "echo")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Build the example exactly as the docs tell an author to.
	bin := filepath.Join(pluginDir, "echo-channel")
	cmd := exec.Command("go", "build", "-o", bin, "./examples/plugins/echo-channel")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("example does not build: %v\n%s", err, out)
	}

	// Its manifest, as shipped (name/transport/exec must stay in sync with
	// examples/plugins/echo-channel/rysh.channel.toml).
	src, err := os.ReadFile(filepath.Join(root, "examples", "plugins", "echo-channel", ManifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(src)
	if err != nil {
		t.Fatalf("shipped example manifest invalid: %v", err)
	}
	if m.Name != "echo" || m.Transport != TransportStdio || m.Exec != "echo-channel" {
		t.Fatalf("shipped manifest changed: %+v (update docs/plugin-authoring.md too)", m)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, ManifestFileName), src, 0o644); err != nil {
		t.Fatal(err)
	}

	opts := SupervisorOptions{
		Dir:          pluginDir,
		ReadyTimeout: 10 * time.Second,
		StopGrace:    time.Second,
	}
	a, err := NewPluginChannelAdapter(m, msg.ChannelConfig{Enabled: true}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	out := channels.OutboundMessage{RecipientID: "r", Content: "round trip", ThreadID: "t-1"}
	if err := a.Send(context.Background(), out); err != nil {
		t.Fatalf("Send: %v", err)
	}
	recvInboundWith(t, a.InboundCh(), 5*time.Second, "echoed inbound",
		func(im channels.InboundMessage) bool {
			return im.Content == "echo: round trip" && im.ThreadID == "t-1"
		})

	if st := a.Status(); !st.Connected {
		t.Fatalf("Status after start = %+v, want connected", st)
	}
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
