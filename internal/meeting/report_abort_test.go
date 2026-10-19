// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// abort means "nothing is written" (report_bot.go). A chunk summary already
// asking the LLM when abort runs must not write the notes checkpoint when its
// answer arrives: that late write is what re-created notes.json after a
// recovery in TestReporterRecoversFromCheckpointAfterRestart (21/200 runs).
func TestReporterAbortStopsAnInFlightCheckpointWrite(t *testing.T) {
	smallChunks(t, 50)
	asked, release := make(chan struct{}, 4), make(chan struct{})
	llm := &fakeLLM{}
	gated := func(ctx context.Context, prompt string) (string, error) {
		asked <- struct{}{}
		<-release
		return llm.exec(ctx, prompt)
	}
	dir := filepath.Join(t.TempDir(), "m-abcdef")
	r, err := newReportTaker(ReportConfig{MeetingID: "m-abcdef", Platform: PlatformZoom, Dir: dir, Summarize: gated})
	if err != nil {
		t.Fatal(err)
	}
	say(r, "Ada", "a line long enough to make a chunk due right away rawphrase") // wakes the loop
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never summarized the due chunk")
	}
	r.abort()
	close(release)
	if !r.wait(5 * time.Second) {
		t.Fatal("aborted taker did not stop")
	}
	// The in-flight summary returns after abort; give it the time it needs.
	r.sumMu.Lock()
	r.sumMu.Unlock()
	if _, err := os.Stat(filepath.Join(dir, "notes.json")); !os.IsNotExist(err) {
		t.Fatalf("an aborted taker wrote the notes checkpoint (stat err %v)", err)
	}
}
