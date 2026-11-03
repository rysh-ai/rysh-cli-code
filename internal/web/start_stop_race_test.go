// SPDX-License-Identifier: Apache-2.0

package web

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Stop may come straight after Start, before the serving goroutine has run.
// The goroutine must not read s.httpSrv (which Stop clears) — under -race this
// failed, and without -race the goroutine could call Serve on nil.
func TestStopRightAfterStart(t *testing.T) {
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	for i := 0; i < 20; i++ {
		s := NewServer(freePort(t), "start-stop", pub, nc, pub.Codecs())
		s.SetHost("127.0.0.1")
		if err := s.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := s.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}
}
