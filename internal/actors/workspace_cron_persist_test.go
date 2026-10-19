// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/cron"
)

// cronKV is a fake workspace KV holding one value for cronKVKey. getErrs are
// returned (in order) by the first Get calls before the stored value is served,
// modelling a guest daemon whose host broker is still recovering at boot.
type cronKV struct {
	nats.KeyValue
	value   []byte // nil = key absent
	getErrs []error
	puts    int
}

type cronKVEntry struct {
	nats.KeyValueEntry
	v []byte
}

func (e cronKVEntry) Value() []byte { return e.v }

func (f *cronKV) Get(key string) (nats.KeyValueEntry, error) {
	if len(f.getErrs) > 0 {
		err := f.getErrs[0]
		f.getErrs = f.getErrs[1:]
		return nil, err
	}
	if f.value == nil {
		return nil, nats.ErrKeyNotFound
	}
	return cronKVEntry{v: f.value}, nil
}

func (f *cronKV) Put(key string, value []byte) (uint64, error) {
	f.puts++
	f.value = append([]byte(nil), value...)
	return uint64(f.puts), nil
}

func savedJobs(t *testing.T, names ...string) []byte {
	t.Helper()
	var jobs []*cron.Job
	for _, n := range names {
		jobs = append(jobs, &cron.Job{ID: n, Name: n, Schedule: "0 */6 * * *", Target: "active", Mode: "shell", Input: "echo " + n, Enabled: true})
	}
	b, err := json.Marshal(jobs)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func storedNames(t *testing.T, raw []byte) []string {
	t.Helper()
	var jobs []*cron.Job
	if err := json.Unmarshal(raw, &jobs); err != nil {
		t.Fatalf("stored cron.jobs is not a job list: %v", err)
	}
	var names []string
	for _, j := range jobs {
		names = append(names, j.Name)
	}
	return names
}

func newCronWorkspace(t *testing.T, kv *cronKV) *WorkspaceActor {
	t.Helper()
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: t.TempDir()}, wKV: kv}
	t.Cleanup(w.stopCron)
	return w
}

// The bug: a transient Get error at boot left the scheduler empty, and startCron
// then persisted that empty list over the saved jobs. A failed load must never
// write, and the next tick must pick the saved jobs back up.
func TestCronStartDoesNotEraseJobsWhenLoadFails(t *testing.T) {
	kv := &cronKV{value: savedJobs(t, "article-publisher"), getErrs: []error{errors.New("nats: timeout")}}
	w := newCronWorkspace(t, kv)

	w.startCron()

	if kv.puts != 0 {
		t.Fatalf("startCron wrote cron.jobs %d time(s) after a failed load — that erases the saved jobs", kv.puts)
	}
	if got := storedNames(t, kv.value); len(got) != 1 || got[0] != "article-publisher" {
		t.Fatalf("stored jobs changed after failed load: %v", got)
	}
	if w.cron.loaded {
		t.Fatalf("scheduler marked loaded although the read failed")
	}

	w.handleCronTick(nil) // broker recovered: the retry succeeds

	if !w.cron.loaded || len(w.cron.jobs) != 1 || w.cron.jobs[0].Name != "article-publisher" {
		t.Fatalf("tick did not restore the saved job: loaded=%v jobs=%d", w.cron.loaded, len(w.cron.jobs))
	}
	if got := storedNames(t, kv.value); len(got) != 1 || got[0] != "article-publisher" {
		t.Fatalf("stored jobs after recovery = %v, want [article-publisher]", got)
	}
}

// A corrupt stored value is data we cannot interpret — leave it alone rather
// than replacing it with an empty list.
func TestCronStartDoesNotOverwriteUndecodableJobs(t *testing.T) {
	kv := &cronKV{value: []byte("{not json")}
	w := newCronWorkspace(t, kv)

	w.startCron()

	if kv.puts != 0 || string(kv.value) != "{not json" {
		t.Fatalf("undecodable cron.jobs was overwritten (puts=%d, value=%q)", kv.puts, kv.value)
	}
}

// An absent key is the normal first-boot case: loaded, and persisting is fine.
func TestCronStartWithNoSavedJobsIsLoaded(t *testing.T) {
	kv := &cronKV{}
	w := newCronWorkspace(t, kv)

	w.startCron()

	if !w.cron.loaded {
		t.Fatalf("absent key should count as loaded")
	}
}

// While the stored list is unreadable, commands that change jobs must refuse
// instead of mutating (and later persisting) a partial in-memory list.
func TestCronAddRefusedWhileJobsUnreadable(t *testing.T) {
	kv := &cronKV{value: savedJobs(t, "article-publisher"),
		getErrs: []error{errors.New("nats: timeout"), errors.New("nats: timeout")}}
	w := newCronWorkspace(t, kv)
	w.startCron() // first error

	var out strings.Builder
	w.handleCronCommand(nil, &out, "", `cron add other "@every 1h" echo hi`) // second error

	if !strings.Contains(out.String(), "could not be read") {
		t.Fatalf("add was not refused while the store is unreadable; output: %q", out.String())
	}
	if kv.puts != 0 {
		t.Fatalf("refused add still wrote cron.jobs (%d puts)", kv.puts)
	}
	if got := storedNames(t, kv.value); len(got) != 1 || got[0] != "article-publisher" {
		t.Fatalf("stored jobs changed: %v", got)
	}
}
