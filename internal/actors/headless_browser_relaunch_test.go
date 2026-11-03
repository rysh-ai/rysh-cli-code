// SPDX-License-Identifier: Apache-2.0

//go:build !windows

// Unix-only: the fake browser is a #!/bin/sh script and the check uses
// syscall.Kill, which Windows does not have (QA-24: vet windows/amd64).

package actors

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/cdp"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// A Chromium that never prints its DevTools line used to stop the headless
// browser for good: one launch, then "failed to start". Now the actor kills
// it, reaps it and relaunches once. The stub wedges on its first run and
// becomes the real browser on its second.
func TestHeadlessBrowserRelaunchesAWedgedStart(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("CI") != "" {
		t.Skip("CI: browser-launch integration not covered (container Chrome cannot start without --no-sandbox)")
	}
	real := cdp.FindChromium()
	if real == "" {
		t.Skip("no Chromium/Chrome binary available")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "wedges-once")
	state := filepath.Join(dir, "launches")
	wedgedPID := filepath.Join(dir, "wedged.pid")
	script := "#!/bin/sh\n" +
		"echo x >> '" + state + "'\n" +
		"if [ \"$(wc -l < '" + state + "')\" -eq 1 ]; then echo $$ > '" + wedgedPID + "'; exec sleep 300; fi\n" +
		"exec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	defer ns.Shutdown()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats not ready")
	}
	nc, err := nats.Connect(nats.DefaultURL, nats.InProcessServer(ns))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	system := actor.NewActorSystem()

	// Not t.TempDir(): Chromium writes its profile until it exits (see the
	// web recorder e2e test).
	profileDir, err := os.MkdirTemp("", "rysh-relaunch-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(profileDir) }()

	hb := NewHeadlessBrowserActor("relaunchpane", "e2e", "about:blank", profileDir, pub, nc)
	hb.binPath = stub
	// The wedged first attempt is cut short; the real relaunch gets the
	// production bound, so a slow cold start under load is not a failure.
	hb.launchWaits = []time.Duration{2 * time.Second, headlessLaunchWaits[1]}
	pid := system.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return hb }))
	defer func() {
		_ = system.Root.StopFuture(pid).Wait()
		time.Sleep(500 * time.Millisecond)
	}()

	waitBrowserReady(t, nc, pub, "relaunchpane")

	b, _ := os.ReadFile(state)
	if n := strings.Count(string(b), "x"); n != 2 {
		t.Fatalf("launches = %d, want 2 (one wedged, one relaunch)", n)
	}
	// The wedged first attempt was killed and reaped, not left running.
	pb, err := os.ReadFile(wedgedPID)
	if err != nil {
		t.Fatal(err)
	}
	wp, _ := strconv.Atoi(strings.TrimSpace(string(pb)))
	if wp <= 1 {
		t.Fatalf("bad wedged pid %q", pb)
	}
	if err := syscall.Kill(wp, 0); err == nil {
		t.Fatalf("wedged launch (pid %d) still running", wp)
	}
}
