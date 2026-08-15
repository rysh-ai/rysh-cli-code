// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

type Record struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	State   string `json:"state"`
	PID     int    `json:"pid,omitempty"`      // daemon process PID
	TUIPIDs []int  `json:"tui_pids,omitempty"` // all attached TUI process PIDs
	// AppClients is the number of desktop-app (WebSocket) clients currently
	// connected to the daemon's web server. The registry's State field tracks
	// TUI attachment only, so app sessions read "detached" even while the
	// desktop app is driving them — this field lets list/info render them as
	// "attached (app)". Maintained live by the daemon's web hub.
	AppClients int `json:"app_clients,omitempty"`
	NATSPort   int `json:"nats_port,omitempty"`
	// ProxyPort is the loopback governance proxy's port (design 001) while it is
	// running, or 0 when it is off. Maintained live by the daemon on ##proxy
	// on/off (read-modify-write, so attach bookkeeping is preserved), so tools
	// inspecting the registry — and ##session info — can see the governed
	// endpoint (http://127.0.0.1:{ProxyPort}) without attaching a TUI.
	ProxyPort int       `json:"proxy_port,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	// Version and BinHash identify the rysh binary that the running daemon was
	// launched from. They are stamped by the daemon at startup and compared on
	// attach so a rebuilt binary can detect that the live daemon is outdated and
	// (with --upgrade) restart it. Version is the human-readable build string
	// (main.version, e.g. a git-describe tag); BinHash is a short content hash of
	// the executable, which changes on every rebuild even when Version does not
	// (e.g. repeated "-dirty" dev builds at the same commit).
	Version string `json:"version,omitempty"`
	BinHash string `json:"bin_hash,omitempty"`
	// ConfigFile is the rysh.config.yaml the daemon loaded (empty when none was
	// found), and RyshDir is the rysh directory that config location implied
	// (see config.resolveConfig). They are recorded so the session — and tools
	// that inspect the registry (e.g. "rysh list-sessions") — are aware of which
	// configuration and rysh state directory this session resolved at startup.
	ConfigFile string `json:"config_file,omitempty"`
	RyshDir    string `json:"rysh_dir,omitempty"`
	// Source identifies the front-end that CREATED the session: "cli" (rysh
	// command line) or "app" (rysh desktop app). It is stamped once, by the
	// daemon that first writes the record, from cfg.SessionSource
	// (RYSH_SESSION_SOURCE), and preserved verbatim by every later daemon —
	// restarting an app session from the terminal must not rewrite its
	// provenance. Either front-end may open either kind; Source only tells
	// EnsureCanOpen which render surfaces to warn about. Empty means
	// legacy/unknown and opens cleanly from either front-end.
	Source string `json:"source,omitempty"`
	// WebPort is the daemon's live web-server port on loopback, or 0 when no
	// web server is running. The desktop app's renderer reaches a daemon only
	// over HTTP/WebSocket, so recording the endpoint here is what lets the app
	// adopt a session it did not spawn — including one created from the command
	// line, whose daemon starts its web server on demand (`##rysh web start`).
	// Maintained live by the daemon on web-server start/stop (see
	// UpdateWebEndpoint), read-modify-write so attach bookkeeping set by other
	// writers is preserved.
	//
	// A web_token used to sit beside it, carrying the access token that guarded
	// the port. Access tokens are gone (internal/web/auth.go): a UI served off
	// this port asks for the workspace's login instead, so the port is the whole
	// endpoint. Records written by older daemons may still carry the field; it
	// is ignored.
	WebPort int `json:"web_port,omitempty"`
	// WebHost is the address that web server is BOUND to (127.0.0.1 for a
	// loopback-only server, 0.0.0.0 for one on the network). WebUser is the
	// login it asks for — the name only; the password lives in the secrets tier
	// and its hash in web-auth.json, and neither belongs in a world-readable
	// registry record. WebPublicURL is the tunnel address the session is
	// reachable at from outside this machine, empty when there is no tunnel.
	//
	// Together they are what makes a restarted session recognisable as the same
	// door: `##session info` and the desktop app can say where it is served,
	// who it asks for, and what to open on a phone — none of which the port
	// alone can answer. Maintained live alongside WebPort (UpdateWebMeta).
	WebHost      string `json:"web_host,omitempty"`
	WebUser      string `json:"web_user,omitempty"`
	WebPublicURL string `json:"web_public_url,omitempty"`
}

// SourceCLI and SourceApp are the canonical session Source values: the rysh
// command line vs the rysh desktop app. Source is PROVENANCE — which front-end
// created the session — and is stamped once, at creation. Either front-end may
// open either kind (see EnsureCanOpen); the value is used to describe what will
// degrade, not to deny access.
const (
	SourceCLI = domain.FrontendCLI
	SourceApp = domain.FrontendApp
)

// NormalizeSource maps a configured/recorded session source to its canonical
// value. Only "app" (the desktop app) is recognised explicitly; everything else
// — including the empty default — is the command line ("cli").
func NormalizeSource(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), SourceApp) {
		return SourceApp
	}
	return SourceCLI
}

// FrontendName renders a session source as a human-readable front-end name for
// error messages.
func FrontendName(source string) string {
	if NormalizeSource(source) == SourceApp {
		return "rysh desktop app"
	}
	return "rysh command line"
}

// EnsureCanOpen decides whether the front-end identified by want may open rec,
// and reports what will render differently if it does.
//
// Both front-ends drive the same daemon over the same subjects, and a workspace
// layout is relative (flex weights, not pixels), so neither kind of session is
// structurally off-limits to the other. What differs is the RENDERER: the
// desktop app is a superset of the terminal, so it opens terminal sessions with
// nothing lost, while the terminal opens app sessions with the app-only render
// surfaces degraded to labelled placeholders.
//
// The returned notes describe exactly those degradations (empty when there are
// none). Callers must surface them — a capability that vanishes silently is the
// failure mode this whole path exists to avoid.
//
// The error return is reserved for a genuinely unopenable session. No
// combination of front-ends produces one today; it stays in the signature so
// the call sites keep handling the possibility rather than having to grow it
// back later.
//
// A blank record source is legacy/unknown and opens cleanly from either
// front-end. want is normalized internally.
func EnsureCanOpen(rec Record, want string) ([]string, error) {
	if rec.Source == "" {
		return nil, nil
	}
	creator, opener := NormalizeSource(rec.Source), NormalizeSource(want)
	if creator == opener {
		return nil, nil
	}
	return domain.CapsFor(opener).MissingVersus(domain.CapsFor(creator)), nil
}

// DegradationSummary renders EnsureCanOpen's notes as a short block for a
// terminal or a command's output, or "" when there is nothing to report. The
// caller supplies the record so the summary can name the creating front-end.
func DegradationSummary(rec Record, notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "session %q was created by the %s; opening it here:\n", rec.Name, FrontendName(rec.Source))
	for _, n := range notes {
		fmt.Fprintf(&b, "  - %s\n", n)
	}
	return b.String()
}

// AddTUIPID adds a TUI PID to the active list (idempotent).
func (r *Record) AddTUIPID(pid int) {
	for _, p := range r.TUIPIDs {
		if p == pid {
			return
		}
	}
	r.TUIPIDs = append(r.TUIPIDs, pid)
}

// RemoveTUIPID removes a TUI PID from the active list.
func (r *Record) RemoveTUIPID(pid int) {
	filtered := r.TUIPIDs[:0]
	for _, p := range r.TUIPIDs {
		if p != pid {
			filtered = append(filtered, p)
		}
	}
	r.TUIPIDs = filtered
}

// HasAliveTUIs checks if any tracked TUI PIDs are still alive.
func (r *Record) HasAliveTUIs() bool {
	for _, pid := range r.TUIPIDs {
		if pid > 0 && ProcessAlive(pid) {
			return true
		}
	}
	return false
}

// AliveTUIPIDs returns only the PIDs that are still alive.
func (r *Record) AliveTUIPIDs() []int {
	alive := make([]int, 0, len(r.TUIPIDs))
	for _, pid := range r.TUIPIDs {
		if pid > 0 && ProcessAlive(pid) {
			alive = append(alive, pid)
		}
	}
	return alive
}

// CleanStalePIDs removes dead PIDs from TUIPIDs.
func (r *Record) CleanStalePIDs() {
	r.TUIPIDs = r.AliveTUIPIDs()
}

// DaemonAlive reports whether this record's daemon process is currently running.
func (r *Record) DaemonAlive() bool {
	return r.PID > 0 && ProcessAlive(r.PID)
}

// reconcileLiveness corrects a record's state to reflect reality. If the record
// claims the daemon is running/detached but its PID is not actually alive, the
// daemon crashed or was SIGKILLed without running its shutdown handler (which
// would have marked the record stopped). We report it as stopped with no PID so
// a dead daemon never appears live to "rysh list-sessions", attach, or create.
// NATSPort is preserved to match the shape the daemon itself writes on a clean
// shutdown. The second return value reports whether anything changed.
func reconcileLiveness(r Record) (Record, bool) {
	if r.State == "stopped" || r.DaemonAlive() {
		return r, false
	}
	r.State = "stopped"
	r.PID = 0
	r.TUIPIDs = nil // a stopped daemon has no attached TUIs
	return r, true
}

type Store struct {
	dir            string
	natsDataDirCfg string // custom NATS data dir from config, empty = use default
}

func NewStore(cfg config.Config) (*Store, error) {
	if strings.TrimSpace(cfg.SessionDir) == "" {
		return nil, errors.New("session directory is empty")
	}
	if err := os.MkdirAll(cfg.SessionDir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: cfg.SessionDir, natsDataDirCfg: cfg.NATS.DataDir}, nil
}

func (s *Store) Upsert(record Record) (Record, error) {
	record.Name = sanitizeName(record.Name)
	record.UpdatedAt = time.Now().UTC()

	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return Record{}, err
	}
	if err := os.WriteFile(s.pathFor(record.Name), data, 0o644); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (s *Store) List() ([]Record, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}

	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			return nil, err
		}

		var record Record
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, err
		}
		if reconciled, changed := reconcileLiveness(record); changed {
			record = reconciled
			_ = s.writeRecord(record) // best-effort self-heal of a dead-daemon record
		}
		records = append(records, record)
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].Name < records[j].Name
	})
	return records, nil
}

func (s *Store) Get(name string) (Record, error) {
	data, err := os.ReadFile(s.pathFor(name))
	if err != nil {
		return Record{}, err
	}

	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, err
	}
	if reconciled, changed := reconcileLiveness(record); changed {
		record = reconciled
		_ = s.writeRecord(record) // best-effort self-heal of a dead-daemon record
	}
	return record, nil
}

// writeRecord persists a record as-is, without touching UpdatedAt. Used to
// self-heal a record whose daemon has died (see reconcileLiveness): we preserve
// the original UpdatedAt because it reflects the daemon's last real activity,
// not the moment we noticed it was gone. Upsert is the normal write path and
// does bump UpdatedAt.
func (s *Store) writeRecord(record Record) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.pathFor(record.Name), data, 0o644)
}

func (s *Store) Delete(name string) error {
	record, err := s.Get(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Capture the NATS port before we kill the daemon. We need it to delete the
	// session's KV buckets from the shared NATS server, which may stay alive
	// after this session's daemon dies: the embedded server is owned by
	// whichever session daemon started first, and other sessions connect to it
	// as clients.
	natsPort := 0
	if err == nil {
		natsPort = record.NATSPort
	}

	// Kill the daemon if it's still running.
	if err == nil && record.PID > 0 {
		if err := terminatePID(record.PID); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}

	// Remove the session record file.
	if rmErr := os.Remove(s.pathFor(name)); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return rmErr
	}

	// Remove the NATS JetStream KV buckets for this session so that
	// restarting with the same name doesn't restore old pane output,
	// shell history, agent state, or workspace layout. This runs even when the
	// session record is already gone (orphaned KV data).
	s.deleteNATSSessionData(name, natsPort)

	// Remove this session's own on-disk artifacts, so a session recreated under
	// the same name doesn't inherit stale state. Scoped to files named for the
	// session: the rest of .rysh belongs to the workspace, not to this session.
	if err == nil {
		deleteSessionArtifacts(record.Path, name)
	}

	return nil
}

// deleteSessionArtifacts removes the on-disk state belonging to ONE session
// inside its working directory.
//
// It used to delete every entry under ".rysh" except "browser-instances", on the
// theory that the directory held only that session's scratch state. It does not.
// ".rysh" is the workspace's state directory: secrets, humanoid and agent
// artefacts, channel state (the WhatsApp dedupe store and replay watermark),
// policy, pipelines, installed packages, the shared embedded-NATS data
// directory, and the records of every *other* session rooted in that directory.
// Deleting a session therefore wiped all of it — and because `rysh run` creates
// a throwaway session and tears it down on exit, a single headless CI run in a
// real workspace silently destroyed the credentials and channel state it found
// there, leaving other sessions' records gone and their daemons unreachable.
//
// The only thing under ".rysh" named for a session is its shell history file, so
// that is the only thing removed here. Everything else outlives any one session
// and belongs to the workspace or the user. A session's own transient state —
// pane output, workspace layout, agent state — lives in JetStream KV and is
// cleaned up by deleteNATSSessionData, which is namespaced by session name.
func deleteSessionArtifacts(workDir, sessionName string) {
	workDir = strings.TrimSpace(workDir)
	sessionName = strings.TrimSpace(sessionName)
	if workDir == "" || sessionName == "" {
		return
	}
	// Mirrors actors.historyFilePath: <rysh-dir>/history/<session>.history.
	_ = os.Remove(filepath.Join(workDir, ".rysh", "history", sessionName+".history"))
}

// natsBaseDir returns the root NATS data directory.
// Returns "" if a custom data dir is configured or the path can't be determined.
func (s *Store) natsBaseDir() string {
	if s.natsDataDirCfg != "" {
		return s.natsDataDirCfg
	}
	// Default layout, anchored to the rysh dir (see config.resolveConfig):
	//   sessions:  <rysh-dir>/sessions/
	//   NATS data: <rysh-dir>/nats/
	// natsDataDirCfg (cfg.NATS.DataDir) is normally set to <rysh-dir>/nats by
	// the loader, so this fallback — derive the sibling "nats" dir from the
	// session dir's parent — only runs for directly-constructed stores.
	base := filepath.Dir(s.dir)
	return filepath.Join(base, "nats")
}

// sessionBuckets returns the JetStream KV bucket names owned by a session.
// These are namespaced by session name so multiple sessions can share one
// NATS server (see internal/bus).
func sessionBuckets(sessionName string) []string {
	safe := sanitizeName(sessionName)
	return []string{
		"rysh-panes-" + safe,
		"rysh-workspace-" + safe,
		"rysh-pipeline-" + safe,
		"rysh-agents-" + safe,
	}
}

// deleteNATSSessionData removes a session's JetStream KV buckets so that
// recreating the session with the same name doesn't restore old pane output,
// shell history, agent state, or workspace layout.
//
// Rysh runs a single embedded NATS server per machine that is shared by every
// session daemon: the first daemon to start owns the server, and the rest
// connect to it as clients. Because of this, removing the bucket directories
// off disk is NOT enough — if the server is still alive (kept up by another
// session) it keeps the buckets in memory and re-persists them on its next
// checkpoint, so recreating the session would restore stale state.
//
// We therefore delete the buckets through the JetStream API on the live server
// first, then also remove any on-disk stream directories as a fallback for the
// case where no server is running (e.g. every session is stopped).
func (s *Store) deleteNATSSessionData(sessionName string, natsPort int) {
	buckets := sessionBuckets(sessionName)

	// 1. Delete via the live (possibly shared) server, if one is reachable.
	s.deleteNATSBucketsViaAPI(natsPort, buckets)

	// 2. Remove on-disk stream directories. Covers the no-server-running case
	//    and cleans up any leftover files.
	natsDir := s.natsBaseDir()
	if natsDir == "" {
		return
	}
	streamsDir := filepath.Join(natsDir, "jetstream", "$G", "streams")
	for _, bucket := range buckets {
		_ = os.RemoveAll(filepath.Join(streamsDir, "KV_"+bucket))
	}
}

// deleteNATSBucketsViaAPI connects to the (possibly shared) NATS server and
// deletes the given KV buckets through the JetStream API. It is best-effort:
// if no server is reachable it returns silently and the caller falls back to
// on-disk removal.
func (s *Store) deleteNATSBucketsViaAPI(natsPort int, buckets []string) {
	if natsPort <= 0 {
		natsPort = 24242 // default embedded NATS port
	}

	nc, err := nats.Connect(
		fmt.Sprintf("nats://127.0.0.1:%d", natsPort),
		nats.MaxReconnects(0),
		nats.Timeout(1*time.Second),
	)
	if err != nil {
		return // no server running — caller falls back to on-disk removal
	}
	defer nc.Close()

	js, err := nc.JetStream()
	if err != nil {
		return
	}

	for _, bucket := range buckets {
		// Ignore errors: the bucket may not exist (e.g. never created).
		_ = js.DeleteKeyValue(bucket)
	}
}

func (s *Store) pathFor(name string) string {
	return filepath.Join(s.dir, sanitizeName(name)+".json")
}

func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "default"
	}
	return strings.ReplaceAll(name, string(filepath.Separator), "-")
}

// UpdateAppClients records the daemon's live desktop-app (WebSocket) client
// count on its session record, read-modify-write so attach bookkeeping
// (TUIPIDs, state) set by other writers is preserved. Failures are silent —
// presence display must never break the daemon. Called by the web hub on
// every client connect/disconnect transition.
func UpdateAppClients(cfg config.Config, name string, n int) {
	store, err := NewStore(cfg)
	if err != nil {
		return
	}
	rec, err := store.Get(name)
	if err != nil || rec.AppClients == n {
		return
	}
	rec.AppClients = n
	_, _ = store.Upsert(rec)
}

// UpdateProxyPort records the governance proxy's port (design 001) on the
// session record, read-modify-write so attach bookkeeping (TUIPIDs, state,
// app-client count) set by other writers is preserved. port 0 clears it (proxy
// stopped). Failures are silent — recording the endpoint must never break the
// daemon. Called from ##proxy on/off.
func UpdateProxyPort(cfg config.Config, name string, port int) {
	store, err := NewStore(cfg)
	if err != nil {
		return
	}
	rec, err := store.Get(name)
	if err != nil || rec.ProxyPort == port {
		return
	}
	rec.ProxyPort = port
	_, _ = store.Upsert(rec)
}

// UpdateWebEndpoint records the daemon's live web-server port on the session
// record, read-modify-write so attach bookkeeping (TUIPIDs, state, app-client
// count, proxy port) set by other writers is preserved. port 0 clears it (web
// server stopped).
//
// This is the hand-off the desktop app adopts a running daemon through: the
// renderer speaks only HTTP/WebSocket, so without a recorded endpoint the app
// can reach only daemons it spawned itself and knows the port of. Failures are
// silent — recording the endpoint must never break the daemon.
func UpdateWebEndpoint(cfg config.Config, name string, port int) {
	store, err := NewStore(cfg)
	if err != nil {
		return
	}
	rec, err := store.Get(name)
	if err != nil {
		return
	}
	if port < 0 {
		port = 0
	}
	if rec.WebPort == port {
		return
	}
	rec.WebPort = port
	_, _ = store.Upsert(rec)
}

// WebMeta is everything the session record says about how this session is
// served: the address the web server is bound to, who signs in, and the public
// URL a tunnel publishes it at.
//
// The port alone answered "where does the desktop app dial", which was all the
// record ever needed. Reaching a session from OFF this machine asks two more
// questions the port cannot: what address was it started on (so a restart
// repeats it), and what URL is it reachable at right now (so it can be shared
// without hunting through ngrok's dashboard).
type WebMeta struct {
	Port int
	Host string
	// User is the login username. The password is never recorded here — it
	// lives in the secrets tier, and its hash in web-auth.json.
	User string
	// PublicURL is the tunnel's public address, empty when there is no tunnel.
	PublicURL string
}

// UpdateWebMeta records the live web endpoint AND its public face on the
// session record, read-modify-write like UpdateWebEndpoint so other writers'
// bookkeeping survives. A zero Port clears the whole set — a stopped server has
// no address, no login and no tunnel.
func UpdateWebMeta(cfg config.Config, name string, meta WebMeta) {
	store, err := NewStore(cfg)
	if err != nil {
		return
	}
	rec, err := store.Get(name)
	if err != nil {
		return
	}
	if meta.Port <= 0 {
		meta = WebMeta{}
	}
	if rec.WebPort == meta.Port && rec.WebHost == meta.Host &&
		rec.WebUser == meta.User && rec.WebPublicURL == meta.PublicURL {
		return
	}
	rec.WebPort, rec.WebHost = meta.Port, meta.Host
	rec.WebUser, rec.WebPublicURL = meta.User, meta.PublicURL
	_, _ = store.Upsert(rec)
}

// ProcessAlive reports whether a process with the given PID is still running.
// A zombie (exited but not yet reaped by its parent) counts as DEAD: signal 0
// succeeds on zombies, but the process can never run again. Treating zombies
// as alive made Terminate report "still alive after SIGKILL" (delete-session
// bailed without removing the record) and made spawn/attach believe a dead
// daemon was still serving its session.
func ProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if process.Signal(syscall.Signal(0)) != nil {
		return false
	}
	return !processZombie(pid)
}

// processZombie reports whether pid is a zombie process. Linux reads the
// state field of /proc/<pid>/stat; darwin/BSD shells out to `ps -o stat=`
// (cold paths only: registry checks, terminate polling). Unknown → false.
func processZombie(pid int) bool {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return false
		}
		// Field 3 (state) follows the parenthesised comm, which may itself
		// contain spaces/parens — take the first byte after the LAST ") ".
		s := string(data)
		if i := strings.LastIndex(s, ") "); i >= 0 && i+2 < len(s) {
			return s[i+2] == 'Z'
		}
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// Terminate gracefully stops the process with the given PID and never sends a
// bare SIGKILL. It sends SIGTERM first so the process can run its shutdown
// handlers — a rysh daemon flushes its actor state to JetStream KV and marks its
// session record stopped on SIGTERM — waits up to grace for it to exit, and only
// then escalates to SIGKILL so a wedged process that ignores SIGTERM still dies
// (and never lingers as an orphan holding the NATS port). Returns nil once the
// process is gone (or was already gone).
func Terminate(pid int, grace time.Duration) error {
	if pid <= 0 {
		return nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return nil // already gone
		}
		return err
	}

	if waitGone(pid, grace) {
		return nil // exited gracefully
	}

	// Still alive after the grace period — escalate to SIGKILL.
	if err := process.Signal(syscall.SIGKILL); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	if waitGone(pid, 2*time.Second) {
		return nil
	}
	return fmt.Errorf("process %d still alive after SIGKILL", pid)
}

// waitGone polls until the process is gone or the timeout elapses, returning
// true if it exited.
func waitGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !ProcessAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// terminatePID stops a daemon using the standard 1-second SIGTERM grace before
// escalating to SIGKILL. Used by Delete, where KV is being torn down anyway so a
// long flush window is unnecessary.
func terminatePID(pid int) error {
	return Terminate(pid, time.Second)
}
