// Copyright 2026 Canonical Inc.
// Originally by Gustavo Niemeyer.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package snippets_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/canonical/rebear/internal/logging"
	"github.com/canonical/rebear/internal/notes"
	"github.com/canonical/rebear/internal/scripts"
	"github.com/canonical/rebear/internal/setup"
	"github.com/canonical/rebear/internal/snippets"
	"go.starlark.net/starlark"
)

// newNotes builds a store over a temp dir, closing it at the
// test's end.
func newNotes(t *testing.T) *notes.Store {
	t.Helper()
	store, err := notes.New(t.TempDir(), time.Now())
	if err != nil {
		t.Fatalf("cannot create notes store: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

// closeFlow runs one flow to its closings, returning the entry
// the closings logged on.
func closeFlow(t *testing.T, program *snippets.Program, inlet *setup.Inlet, outlet *setup.Outlet) *logging.Entry {
	t.Helper()
	entry := logging.Begin("GET", "/", "")
	flow, err := program.OpeningFlow(nil, inlet, entry)
	if err != nil {
		t.Fatalf("cannot start flow: %v", err)
	}
	if _, err := program.OpeningFlow(flow, outlet, nil); err != nil {
		t.Fatalf("cannot settle outlet: %v", err)
	}
	flow.LockOut()
	flow.LockIn()
	program.ClosingFlow(flow)
	return entry
}

// setState replaces the store's state, for arranging one.
func setState(store *notes.Store, state string) {
	store.Release(store.Acquire()[:0] + state)
}

// mapState reads the store's state as a dict, for asserting
// on it.
func mapState(t *testing.T, store *notes.Store) *starlark.Dict {
	t.Helper()
	state := store.Acquire()
	value, err := scripts.DecodeJSON([]byte(state))
	if err != nil {
		store.Release(state)
		t.Fatalf("cannot decode state %q: %v", state, err)
	}
	store.Release(state)
	dict, ok := value.(*starlark.Dict)
	if !ok {
		t.Fatalf("state %q is not a json object", state)
	}
	return dict
}

func TestClosingFlowNotes(t *testing.T) {
	// The closings see the notes argument: writes reach the
	// shared store, a later closing reads them back, and the
	// whole dict api works - the value delegates to the
	// genuine dict.
	cfg, inlet, outlet := config()
	store := newNotes(t)
	outlet.ClosingFlow = reindent(`
		|notes["count"] = 1
		|notes.update({"extra": True})
	`)
	inlet.ClosingFlow = reindent(`
		|print("count: " + str(notes.get("count", 0)))
		|print("keys: " + str(len(notes)))
	`)
	program := compile(t, cfg)
	program.SetNotes(store)
	entry := closeFlow(t, program, inlet, outlet)
	want := []map[string]any{
		{"at": "inlets.local.closing-flow:1", "print": "count: 1"},
		{"at": "inlets.local.closing-flow:2", "print": "keys: 2"},
	}
	if !reflect.DeepEqual(entry.Logs, want) {
		t.Errorf("log %q, want %q", entry.Logs, want)
	}
	state := mapState(t, store)
	if got := state.String(); got != `{"count": 1, "extra": True}` {
		t.Errorf("state = %s, want the closings' writes", got)
	}
}

func TestClosingFlowNotesSharedAcrossFlows(t *testing.T) {
	// The notes are global: a second flow's closings see the
	// first flow's writes.
	cfg, inlet, outlet := config()
	store := newNotes(t)
	inlet.ClosingFlow = reindent(`
		|notes["hits"] = notes.get("hits", 0) + 1
	`)
	program := compile(t, cfg)
	program.SetNotes(store)
	closeFlow(t, program, inlet, outlet)
	closeFlow(t, program, inlet, outlet)
	state := mapState(t, store)
	if got := state.String(); got != `{"hits": 2}` {
		t.Errorf("state = %s, want the two flows' writes", got)
	}
}

func TestClosingFlowNotesEncodeFailure(t *testing.T) {
	// A value the state cannot hold logs on the culprit's flow
	// entry and loses the snippet's changes: the state stays,
	// and the bad value does not survive its snippet.
	cfg, inlet, outlet := config()
	store := newNotes(t)
	outlet.ClosingFlow = reindent(`
		|notes["kept"] = 1
	`)
	inlet.ClosingFlow = reindent(`
		|notes["bad"] = fail
		|print("still running")
	`)
	program := compile(t, cfg)
	program.SetNotes(store)
	entry := closeFlow(t, program, inlet, outlet)
	want := []map[string]any{
		{"at": "inlets.local.closing-flow:2", "print": "still running"},
		{"error": "notes: cannot encode: cannot encode <built-in function fail> as json"},
	}
	if !reflect.DeepEqual(entry.Logs, want) {
		t.Errorf("log %q, want %q", entry.Logs, want)
	}
	state := mapState(t, store)
	if got := state.String(); got != `{"kept": 1}` {
		t.Errorf("state = %s, want the outlet's write alone", got)
	}
}

func TestClosingFlowNotesDecodeFailure(t *testing.T) {
	// A state that does not decode logs on the witnessing
	// flow's entry and answers an empty map: the snippet did
	// nothing wrong, but it is the only witness. The release
	// then writes the empty map as the new state.
	cfg, inlet, outlet := config()
	store := newNotes(t)
	setState(store, "not json")
	outlet.ClosingFlow = reindent(`
		|print("keys: " + str(len(notes)))
		|notes["fresh"] = 1
	`)
	program := compile(t, cfg)
	program.SetNotes(store)
	entry := closeFlow(t, program, inlet, outlet)
	want := []map[string]any{
		{"error": "notes: cannot decode: invalid character 'o' in literal null (expecting 'u')"},
		{"at": "outlets.remote.closing-flow:1", "print": "keys: 0"},
	}
	if !reflect.DeepEqual(entry.Logs, want) {
		t.Errorf("log %q, want %q", entry.Logs, want)
	}
	state := mapState(t, store)
	if got := state.String(); got != `{"fresh": 1}` {
		t.Errorf("state = %s, want the fresh write", got)
	}
}

func TestClosingFlowNotesUntouchedHoldsNothing(t *testing.T) {
	// A closing never touching the notes holds no lock: the
	// store is free the moment the closings ran.
	cfg, inlet, outlet := config()
	store := newNotes(t)
	inlet.ClosingFlow = reindent(`
		|print("no notes here")
	`)
	program := compile(t, cfg)
	program.SetNotes(store)
	entry := closeFlow(t, program, inlet, outlet)
	if len(entry.Logs) != 1 || entry.Logs[0]["print"] != "no notes here" {
		t.Errorf("log %q, want the closing's print", entry.Logs)
	}
	// The store is free the moment the closings ran: the value
	// released it, or never took it.
	acquired := make(chan struct{})
	go func() {
		state := store.Acquire()
		store.Release(state)
		close(acquired)
	}()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatalf("store lock not free after the closings")
	}
}
