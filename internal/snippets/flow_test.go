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
	"net/http"
	"strings"
	"testing"
)

// reindent, config, and compile live in snippets_test.go.

func TestFlowSnapshots(t *testing.T) {
	// The snapshot calls record names on their halves, from any
	// snippet until their stage closes: outgoing until the send,
	// incoming until the write-back - the arrival not required.
	// One name may ride both halves, and the distinct-name list
	// aggregates them. Past the lock, the call refuses.
	cfg, inlet, outlet := config()
	inlet.OpeningFlow = reindent(`
		|outgoing.snapshot("both")
		|outgoing.snapshot("req")
		|incoming.snapshot("both")
	`)
	inlet.IncomingHeaders = reindent(`
		|incoming.snapshot("resp")
	`)
	outlet.IncomingHeaders = reindent(`
		|incoming.snapshot("late")
	`)
	program := compile(t, cfg)
	flow := program.NewFlow(inlet, nil)
	flow.SetOut("POST", "/left", "", http.Header{})
	if _, err := program.OpeningFlow(flow, inlet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := flow.OutSnapshots(); !equalStrings(got, []string{"both", "req"}) {
		t.Errorf("OutSnapshots = %q, want [both req]", got)
	}
	if got := flow.InSnapshots(); !equalStrings(got, []string{"both"}) {
		t.Errorf("InSnapshots = %q, want [both] before the arrival", got)
	}
	// The arrival and the incoming stage record more.
	flow.LockOut()
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	if err := flow.SetIn(200, headers); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := program.IncomingHeaders(flow, inlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := flow.InSnapshots(); !equalStrings(got, []string{"both", "resp"}) {
		t.Errorf("InSnapshots = %q, want [both resp]", got)
	}
	// The distinct names across both halves, first-seen order.
	if got := flow.SnapshotNames(); !equalStrings(got, []string{"both", "req", "resp"}) {
		t.Errorf("SnapshotNames = %q, want [both req resp]", got)
	}
	// Past the write-back, incoming.snapshot refuses - even the
	// outlet's incoming stage, the closings already past.
	flow.LockIn()
	program.ClosingFlow(flow)
	err := program.IncomingHeaders(flow, outlet)
	if err == nil || !strings.Contains(err.Error(), "cannot snapshot incoming: response already sent") {
		t.Fatalf("error = %v, want the already sent failure", err)
	}
}

func TestFlowSnapshotRefusals(t *testing.T) {
	// A bad name refuses at the call; past the send,
	// outgoing.snapshot refuses. All the snippets are set before
	// the single compile: a flow's script state belongs to the
	// program that made it.
	cfg, inlet, _ := config()
	inlet.OpeningFlow = reindent(`
		|outgoing.snapshot("Bad/Name")
	`)
	program := compile(t, cfg)
	flow := program.NewFlow(inlet, nil)
	flow.SetOut("POST", "/left", "", http.Header{})
	_, err := program.OpeningFlow(flow, inlet, nil)
	if err == nil || !strings.Contains(err.Error(), `snapshot: invalid name "Bad/Name"`) {
		t.Fatalf("error = %v, want the invalid name failure", err)
	}

	cfg, inlet, _ = config()
	inlet.OpeningFlow = reindent(`
		|outgoing.snapshot("ok")
	`)
	inlet.OutgoingHeaders = reindent(`
		|outgoing.snapshot("late")
	`)
	program = compile(t, cfg)
	flow = program.NewFlow(inlet, nil)
	flow.SetOut("POST", "/left", "", http.Header{})
	if _, err := program.OpeningFlow(flow, inlet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.LockOut()
	err = program.OutgoingHeaders(flow, inlet)
	if err == nil || !strings.Contains(err.Error(), "cannot snapshot outgoing: request already sent") {
		t.Fatalf("error = %v, want the already sent failure", err)
	}
}

// equalStrings compares two slices for element equality.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestFlowOutReadsTheSeed(t *testing.T) {
	// The seeded request is there from the opening on: the
	// method, the mount-cut path, and the cleaned content type
	// over the seeded headers.
	cfg, inlet, _ := config()
	inlet.OpeningFlow = reindent(`
		|if outgoing.method != "POST":
		|    fail("method not seeded")
		|if outgoing.path != "/left":
		|    fail("path not seeded")
		|if outgoing.type != "application/json":
		|    fail(outgoing.type)
	`)
	program := compile(t, cfg)
	// The proxy's order: create, seed, then open - the opening
	// already sees the request.
	flow := program.NewFlow(inlet, nil)
	headers := http.Header{}
	headers.Set("Content-Type", "Application/JSON; charset=utf-8")
	flow.SetOut("POST", "/left", "", headers)
	if _, err := program.OpeningFlow(flow, inlet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFlowOutFieldsNoneBeforeSeed(t *testing.T) {
	// An unseeded flow models no request: every field reads None.
	cfg, inlet, _ := config()
	inlet.OpeningFlow = reindent(`
		|if outgoing.method != None or outgoing.path != None or outgoing.type != None or outgoing.headers != None:
		|    fail("unseeded fields must read None")
	`)
	program := compile(t, cfg)
	if _, err := program.OpeningFlow(nil, inlet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFlowOutHeadersLive(t *testing.T) {
	// The headers dict is the one the headers stages and flow.o
	// share: a stage's mutation is visible to the next, and the
	// content type follows it.
	cfg, inlet, outlet := config()
	inlet.OutgoingHeaders = reindent(`
		|headers["X-Mark"] = "yes"
	`)
	outlet.OutgoingHeaders = reindent(`
		|if headers["X-Mark"] != "yes":
		|    fail("the dict is not shared")
		|headers["Content-Type"] = "text/plain"
		|if outgoing.type != "text/plain":
		|    fail("the content type did not follow")
	`)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("POST", "/left", "", http.Header{})
	if err := program.OutgoingHeaders(flow, inlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := program.OutgoingHeaders(flow, outlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := flow.OutHeaders().Get("X-Mark"); got != "yes" {
		t.Errorf("X-Mark = %q, want yes", got)
	}
}

func TestFlowOutAssigns(t *testing.T) {
	// The scalars and the content type assign: the assignment
	// replaces the path as it is currently best known, and the
	// type writes the Content-Type header it cleans from.
	cfg, inlet, _ := config()
	inlet.OutgoingHeaders = reindent(`
		|outgoing.method = "PUT"
		|outgoing.path = "/rewritten"
		|outgoing.type = "application/json"
	`)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("POST", "/left", "", http.Header{})
	if err := program.OutgoingHeaders(flow, inlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := flow.OutMethod(); got != "PUT" {
		t.Errorf("method = %q, want PUT", got)
	}
	if got := flow.OutPath(); got != "/rewritten" {
		t.Errorf("path = %q, want /rewritten", got)
	}
	if !flow.PathTouched() {
		t.Errorf("PathTouched = false, want true")
	}
	if got := flow.OutHeaders().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestFlowOutPathIsTheTrailing(t *testing.T) {
	// The path a snippet reads and assigns is the trailing - no
	// outlet prefix on it: the outlet is not known from a snippet,
	// so the prefix joins at the send. An assignment marks the path
	// touched, the escaped-passthrough gate.
	cfg, inlet, _ := config()
	inlet.OutgoingHeaders = reindent(`
		|if outgoing.path != "/left":
		|    fail("path not the trailing")
		|outgoing.path = "/rewritten"
	`)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("POST", "/left", "", http.Header{})
	if err := program.OutgoingHeaders(flow, inlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := flow.OutPath(); got != "/rewritten" {
		t.Errorf("path = %q, want the assigned /rewritten, prefixless", got)
	}
	if !flow.PathTouched() {
		t.Errorf("PathTouched = false, want true")
	}
}

func TestFlowOutPathTouchedOnlyOnChange(t *testing.T) {
	// Reassigning the path as it stands is no change: the touched
	// mark - the escaped-passthrough gate - does not land, so a
	// snippet writing back what it read keeps the passthrough.
	// Only a differing value touches.
	cfg, inlet, _ := config()
	inlet.OutgoingHeaders = reindent(`
		|outgoing.path = "/left"
	`)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("POST", "/left", "", http.Header{})
	if flow.PathTouched() {
		t.Fatalf("PathTouched = true before any assignment")
	}
	if err := program.OutgoingHeaders(flow, inlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := flow.OutPath(); got != "/left" {
		t.Errorf("path = %q, want /left", got)
	}
	if flow.PathTouched() {
		t.Errorf("PathTouched = true, want false: the same path is no change")
	}
	flow.SetOutPath("/elsewhere")
	if !flow.PathTouched() {
		t.Errorf("PathTouched = false, want true on a differing value")
	}
	if got := flow.OutPath(); got != "/elsewhere" {
		t.Errorf("path = %q, want /elsewhere", got)
	}
}

func TestFlowLockOutedRefusesWrites(t *testing.T) {
	// Once the request left, nothing changes: the scalars refuse,
	// and the dict is frozen by the starlark rule.
	cfg, inlet, outlet := config()
	inlet.OutgoingHeaders = reindent(`
		|outgoing.method = "PUT"
	`)
	outlet.OutgoingHeaders = reindent(`
		|headers["X-Late"] = "no"
	`)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("POST", "/left", "", http.Header{})
	flow.LockOut()
	err = program.OutgoingHeaders(flow, inlet)
	if err == nil || !strings.Contains(err.Error(), "cannot set outgoing.method: request already sent") {
		t.Fatalf("error = %v, want the already sent failure", err)
	}
	if _, err := program.OpeningFlow(flow, outlet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err = program.OutgoingHeaders(flow, outlet)
	if err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("error = %v, want the frozen dict failure", err)
	}
}

func TestFlowInLifecycle(t *testing.T) {
	// The response half: None before the arrival, seeded with the
	// outlet's code and headers, assignable until the write-back,
	// and the arrival is only recordable once the request left.
	cfg, inlet, outlet := config()
	inlet.OpeningFlow = reindent(`
		|if incoming.code != None or incoming.type != None or incoming.headers != None:
		|    fail("response fields must read None before the arrival")
	`)
	inlet.IncomingHeaders = reindent(`
		|if incoming.code != 201:
		|    fail("code not seeded")
		|if incoming.type != "text/plain":
		|    fail(incoming.type)
		|incoming.code = 202
		|incoming.type = "application/json"
	`)
	outlet.IncomingHeaders = reindent(`
		|incoming.code = 203
	`)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("POST", "/left", "", http.Header{})

	if err := flow.SetIn(200, http.Header{}); err == nil {
		t.Fatalf("SetIn before the request left: error = nil, want the refusal")
	}
	flow.LockOut()
	headers := http.Header{}
	headers.Set("Content-Type", "text/plain; charset=utf-8")
	if err := flow.SetIn(201, headers); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := flow.InCode(); got != 201 {
		t.Errorf("code = %d, want 201", got)
	}
	if err := program.IncomingHeaders(flow, inlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := flow.InCode(); got != 202 {
		t.Errorf("code = %d, want the assigned 202", got)
	}
	if got := flow.InHeaders().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	flow.LockIn()
	err = program.IncomingHeaders(flow, outlet)
	if err == nil || !strings.Contains(err.Error(), "cannot set incoming.code: response already sent") {
		t.Fatalf("error = %v, want the already sent failure", err)
	}
}

func TestFlowWholesaleDictRefused(t *testing.T) {
	// The headers dicts never replace - mutation is the workflow.
	cfg, inlet, _ := config()
	inlet.OutgoingHeaders = reindent(`
		|outgoing.headers = {"X-New": "yes"}
	`)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("POST", "/left", "", http.Header{})
	err = program.OutgoingHeaders(flow, inlet)
	if err == nil || !strings.Contains(err.Error(), "cannot set outgoing.headers: mutate the dict in place") {
		t.Fatalf("error = %v, want the mutation failure", err)
	}
}
