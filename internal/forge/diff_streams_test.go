// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"reflect"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/forge/ir"
)

// streamSpecs returns two APIs whose Operations are IDENTICAL and whose Streams
// differ in all three ways: one added, one removed, one whose signature
// changed. Anything DiffAPIs reports for these two is necessarily about
// streams.
func streamSpecs() (oldAPI, newAPI *ir.API) {
	sameOps := []ir.Operation{
		{ID: "getUser", Method: "GET", Path: "/users/{id}", Params: []ir.Param{{Name: "id", In: "path", Required: true}}},
	}
	oldAPI = &ir.API{
		Operations: append([]ir.Operation(nil), sameOps...),
		Streams: []ir.Operation{
			{ID: "watchUsers", Method: "POST", Path: "/rpc/WatchUsers"},
			{ID: "tailLogs", Method: "POST", Path: "/rpc/TailLogs"},
			{ID: "stayStream", Method: "POST", Path: "/rpc/Stay"},
		},
	}
	newAPI = &ir.API{
		Operations: append([]ir.Operation(nil), sameOps...),
		Streams: []ir.Operation{
			// watchUsers gains a required param → changed
			{ID: "watchUsers", Method: "POST", Path: "/rpc/WatchUsers", Params: []ir.Param{{Name: "since", In: "query", Required: true}}},
			// tailLogs removed
			{ID: "followEvents", Method: "POST", Path: "/rpc/FollowEvents"}, // added
			{ID: "stayStream", Method: "POST", Path: "/rpc/Stay"},           // unchanged
		},
	}
	return oldAPI, newAPI
}

// TestDiffAPIsSeesStreamChanges is T5b. ir.API.Streams holds gRPC
// server-streaming methods and GraphQL subscriptions, deliberately kept out of
// Operations so unary generators do not emit a wrapper that hangs. DiffAPIs
// indexed Operations only, so `forge diff` — the one command whose whole job is
// "what changed when the spec changed" — was silent about that entire half of
// the surface.
func TestDiffAPIsSeesStreamChanges(t *testing.T) {
	oldAPI, newAPI := streamSpecs()

	d := DiffAPIs(oldAPI, newAPI)

	// Guard the premise: if these fire, the fixture is wrong and the rest of
	// the assertions would be proving nothing.
	if len(d.Added) != 0 || len(d.Removed) != 0 || len(d.Changed) != 0 {
		t.Fatalf("fixture is malformed: the two APIs must differ ONLY in Streams, "+
			"but the operation-level diff is non-empty: added=%v removed=%v changed=%v",
			d.Added, d.Removed, d.Changed)
	}

	if want := []string{"followEvents"}; !reflect.DeepEqual(d.StreamsAdded, want) {
		t.Errorf("StreamsAdded = %v, want %v", d.StreamsAdded, want)
	}
	if want := []string{"tailLogs"}; !reflect.DeepEqual(d.StreamsRemoved, want) {
		t.Errorf("StreamsRemoved = %v, want %v", d.StreamsRemoved, want)
	}
	if want := []string{"watchUsers"}; !reflect.DeepEqual(d.StreamsChanged, want) {
		t.Errorf("StreamsChanged = %v, want %v", d.StreamsChanged, want)
	}

	if d.Empty() {
		t.Errorf("Empty() = true for two specs whose stream sets differ — " +
			"`forge diff` would print \"No changes\" and the caller would adopt the new spec blind")
	}
	if !DiffAPIs(newAPI, newAPI).Empty() {
		t.Errorf("identical specs must produce an empty diff")
	}
}

// TestDiffAPIsKeepsStreamsAndOperationsApart is the distinction that matters:
// a removed STREAM breaks a background session, a removed OPERATION breaks a
// tool call. They are different events, and — because Streams and Operations
// are separate lists of ir.Operation — one id can name both at once. Merging
// them into the same slices would report one event and hide the other.
func TestDiffAPIsKeepsStreamsAndOperationsApart(t *testing.T) {
	// "sync" exists as BOTH a unary operation and a stream in the old spec.
	// The new spec drops the stream and keeps the operation.
	oldAPI := &ir.API{
		Operations: []ir.Operation{{ID: "sync", Method: "POST", Path: "/sync"}},
		Streams:    []ir.Operation{{ID: "sync", Method: "POST", Path: "/rpc/SyncStream"}},
	}
	newAPI := &ir.API{
		Operations: []ir.Operation{{ID: "sync", Method: "POST", Path: "/sync"}},
	}

	d := DiffAPIs(oldAPI, newAPI)

	if len(d.Removed) != 0 {
		t.Errorf("Removed = %v, want none: the unary operation \"sync\" still exists, "+
			"only the stream of the same name went away", d.Removed)
	}
	if want := []string{"sync"}; !reflect.DeepEqual(d.StreamsRemoved, want) {
		t.Errorf("StreamsRemoved = %v, want %v", d.StreamsRemoved, want)
	}
	if d.Empty() {
		t.Errorf("Empty() = true after a stream was removed")
	}
}

// TestDiffEmptyAccountsForEveryField keeps Empty() honest as Diff grows. A
// field added to Diff but forgotten in Empty() makes `forge diff` print
// "No changes" while holding changes — the worst possible failure for this
// command, and a silent one.
func TestDiffEmptyAccountsForEveryField(t *testing.T) {
	v := reflect.ValueOf(Diff{})
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		probe := Diff{}
		f := reflect.ValueOf(&probe).Elem().Field(i)
		if f.Kind() != reflect.Slice {
			t.Fatalf("Diff.%s is %s, not a slice: this test only knows how to probe slice fields "+
				"and must be taught about the new shape", name, f.Kind())
		}
		f.Set(reflect.ValueOf([]string{"probe"}))
		if probe.Empty() {
			t.Errorf("Empty() = true with Diff.%s populated — Empty() does not account for that field", name)
		}
	}
}
