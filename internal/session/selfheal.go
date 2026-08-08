package session

// SelfHeal returns the record a live daemon should have on disk, given what is
// actually there. It is the daemon-side truth check behind the boot Upsert's
// standing guarantee (see startSessionRecordGuard in main): existing is the
// record read from the registry (found reports whether one exists), self is
// the record the daemon wrote at boot (name, path, PID, NATS port, version,
// config identity, source).
//
// The record is left untouched (changed=false) when it already reflects this
// daemon: same PID, same NATS port, and a live state (running/detached) — the
// attach flow owns TUIPIDs and the running/detached flip, so a truthful
// record is never rewritten. Anything else (missing file, foreign or stale
// PID, wrong NATS port, state "stopped" while the daemon is alive) is
// replaced with the daemon's own identity; the TUI bookkeeping is preserved
// when the record was otherwise ours.
func SelfHeal(existing Record, found bool, self Record) (healed Record, changed bool) {
	if found && existing.PID == self.PID && existing.NATSPort == self.NATSPort &&
		(existing.State == "running" || existing.State == "detached") {
		return existing, false
	}
	healed = self
	healed.State = "detached"
	// Provenance is stamped once, by whichever front-end created the session,
	// and outlives every daemon that serves it. Healing must never rewrite it:
	// restarting an app-created session from the terminal would otherwise
	// relabel it "cli" and lose the reason the terminal degrades its web panes.
	if found && existing.Source != "" {
		healed.Source = existing.Source
	}
	if found && existing.PID == self.PID {
		// Ours, but wrong elsewhere (port or state): keep the TUI bookkeeping
		// (and the live app-client count, maintained by the web hub) so an
		// attached client survives the heal.
		healed.TUIPIDs = existing.AliveTUIPIDs()
		healed.AppClients = existing.AppClients
		healed.ProxyPort = existing.ProxyPort // live daemon-maintained, like AppClients
		// The web endpoint is maintained live by the daemon (UpdateWebEndpoint)
		// and is how the desktop app adopts this session — dropping it here
		// would strand a connected app on the next heal tick.
		healed.WebPort = existing.WebPort
		if len(healed.TUIPIDs) > 0 {
			healed.State = "running"
		}
	}
	return healed, true
}
