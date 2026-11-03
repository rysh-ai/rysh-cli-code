// SPDX-License-Identifier: Apache-2.0

package main

// setup_bus_port.go — D-55 option B: `rysh setup` gives a NEW project a NATS
// port of its own, so its daemon never joins another project's bus as a guest
// (design 028 §9 limit 7). "New" is the case where setup derives the
// directory's session name: an existing project, a config that already names a
// port, an external bus and a RYSH_NATS_PORT override are all left alone. A
// daemon whose config names its port refuses a foreign host (bus.Config
// RefuseForeignHost), so the port is a real boundary of state, not of trust:
// every daemon of one OS user can still read every bus token.

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// projectBusPortBase and projectBusPortSpan are the band setup picks from:
// clear of the 24242 default and the desktop app's 24242+slot secondaries.
const (
	projectBusPortBase = 24300
	projectBusPortSpan = 600
)

// pickProjectBusPort returns a port in the band for dir, starting from a point
// derived from its path and probing upward (wrapping inside the band) past any
// port inUse reports. 0 means the band is full.
func pickProjectBusPort(dir string, inUse func(port int) bool) int {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	sum := sha256.Sum256([]byte(dir))
	start := int(binary.BigEndian.Uint32(sum[:4]) % projectBusPortSpan)
	for i := 0; i < projectBusPortSpan; i++ {
		p := projectBusPortBase + (start+i)%projectBusPortSpan
		if !inUse(p) {
			return p
		}
	}
	return 0
}

// assignProjectBusPort writes a port of its own into a new project's config
// and returns cfg with it applied. It never fails setup: a port it cannot
// write is a warning, and the project keeps today's shared default.
func assignProjectBusPort(cfg config.Config, cfgFile, dir string, store *session.Store) config.Config {
	if cfg.NATS.Mode == "external" || strings.TrimSpace(os.Getenv("RYSH_NATS_PORT")) != "" {
		return cfg
	}
	used := map[int]bool{}
	if store != nil {
		if recs, err := store.List(); err == nil {
			for _, r := range recs {
				used[r.NATSPort] = true
			}
		}
	}
	port := pickProjectBusPort(dir, func(p int) bool { return used[p] || loopbackPortBound(p) })
	if port == 0 {
		fmt.Printf("WARN no free port in %d-%d for this project's own bus; it keeps the shared default\n",
			projectBusPortBase, projectBusPortBase+projectBusPortSpan-1)
		return cfg
	}
	written, err := config.SetNATSPortIfUnset(cfgFile, port)
	if err != nil {
		fmt.Printf("WARN could not save nats.port %d to %s (%v); this project keeps the shared bus\n", port, cfgFile, err)
		return cfg
	}
	if !written {
		return cfg
	}
	cfg.NATS.Port, cfg.NATS.OwnPort = port, true
	fmt.Printf("nats port: %d (this project's own bus; saved as nats.port in %s)\n", port, cfgFile)
	return cfg
}

// loopbackPortBound reports whether something already listens on 127.0.0.1:p.
func loopbackPortBound(p int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	if err != nil {
		return true
	}
	_ = ln.Close()
	return false
}

// daemonRefusesForeignHost is the daemon's bus.Config RefuseForeignHost (D-55
// B): true exactly when the project's own config file names its port, never
// when RYSH_NATS_PORT chose it (config.NATSConfig.OwnPort). Kept as a function
// so the decision has a unit test without starting a daemon (security-lead N1).
func daemonRefusesForeignHost(cfg config.Config) bool {
	return cfg.NATS.OwnPort
}
