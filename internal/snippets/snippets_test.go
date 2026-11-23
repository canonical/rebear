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
	"bytes"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/canonical/rebear/internal/logging"
	"github.com/canonical/rebear/internal/setup"
	"github.com/canonical/rebear/internal/snippets"
)

// reindent strips each line up to and including its first "|",
// keeping the suffix as the line, and drops lines without one.
// It lets snippet bodies sit pipe-prefixed and indented at the
// depth of the code around them.
func reindent(sample string) string {
	var out strings.Builder
	for _, line := range strings.Split(strings.Trim(sample, "\n"), "\n") {
		_, content, found := strings.Cut(line, "|")
		if !found {
			continue
		}
		out.WriteString(content)
		out.WriteString("\n")
	}
	return out.String()
}

// config builds a setup with one inlet and one outlet, ready
// for snippet fields to be filled in.
func config() (*setup.Setup, *setup.Inlet, *setup.Outlet) {
	inlet := &setup.Inlet{Name: "local"}
	outlet := &setup.Outlet{Name: "remote"}
	return &setup.Setup{
		Inlets:  []*setup.Inlet{inlet},
		Outlets: []*setup.Outlet{outlet},
	}, inlet, outlet
}

// compile builds the setup-wide program, failing the test on
// any compile error.
func compile(t *testing.T, cfg *setup.Setup) *snippets.Program {
	t.Helper()
	program, err := snippets.Compile(cfg)
	if err != nil {
		t.Fatalf("cannot compile: %v", err)
	}
	return program
}

func TestCompileNoSnippets(t *testing.T) {
	// A setup with no snippets compiles to a nil program, and
	// a flow on it starts without error.
	cfg, inlet, _ := config()
	program, err := snippets.Compile(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if program != nil {
		t.Fatalf("program = %v, want nil", program)
	}
	if _, err := program.OpeningFlow(nil, inlet, nil); err != nil {
		t.Fatalf("nil program OpeningFlow: %v", err)
	}
}

func TestBadNamesRefused(t *testing.T) {
	// Names become function names, so a name breaking the
	// rule cannot enter: snippetless entities are checked
	// too, and the message spells the rule.
	testCases := []struct {
		kind, name string
	}{{
		kind: "inlet",
		name: "Foo",
	}, {
		kind: "inlet",
		name: "a_b",
	}, {
		kind: "inlet",
		name: "a-B",
	}, {
		kind: "inlet",
		name: "-ab",
	}, {
		kind: "outlet",
		name: "ab-",
	}, {
		kind: "midlet",
		name: "Foo",
	}}
	for _, tc := range testCases {
		cfg, _, _ := config()
		if tc.kind == "inlet" {
			cfg.Inlets = append(cfg.Inlets, &setup.Inlet{Name: tc.name})
		} else if tc.kind == "midlet" {
			cfg.Midlets = append(cfg.Midlets, &setup.Midlet{Name: tc.name})
		} else {
			cfg.Outlets = append(cfg.Outlets, &setup.Outlet{Name: tc.name})
		}
		_, err := snippets.Compile(cfg)
		want := tc.kind + " invalid name " + strconv.Quote(tc.name) + ": must match ^[a-z][a-z0-9]*(-[a-z0-9]+)*$"
		if err == nil || err.Error() != want {
			t.Errorf("%s %q: error = %v, want %q", tc.kind, tc.name, err, want)
		}
	}
}

func TestDashedAndShortNamesCompile(t *testing.T) {
	// Dashes are legal in names and become underscores in the
	// function names; a single character is legal too - the
	// name rule makes both no problem.
	cfg := &setup.Setup{
		Inlets: []*setup.Inlet{{
			Name: "a",
			OpeningFlow: reindent(`
				|flow.state["mark"] = "ok"
			`),
		}},
		Outlets: []*setup.Outlet{{
			Name: "b",
		}},
	}
	program := compile(t, cfg)
	if _, err := program.OpeningFlow(nil, cfg.Inlets[0], nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg2 := &setup.Setup{
		Inlets: []*setup.Inlet{{
			Name: "a-b",
			OpeningFlow: reindent(`
				|flow.state["mark"] = "ok"
			`),
		}},
		Outlets: []*setup.Outlet{{Name: "remote"}},
	}
	program2 := compile(t, cfg2)
	if _, err := program2.OpeningFlow(nil, cfg2.Inlets[0], nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOpeningFlowRunsPerKind(t *testing.T) {
	// Inlet and outlet openings run apart, on the same flow
	// state: the outlet's opening reads what the inlet's left.
	// Print rides the entry's log list, as one object per call.
	cfg, inlet, outlet := config()
	inlet.OpeningFlow = reindent(`
		|flow.state["mark"] = "inlet"
	`)
	outlet.OpeningFlow = reindent(`
		|print(flow.state["mark"])
	`)
	program, err := snippets.Compile(cfg)
	if err != nil {
		t.Fatalf("cannot compile: %v", err)
	}
	entry := logging.Begin("GET", "/", "")
	flow, err := program.OpeningFlow(nil, inlet, entry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entry.Logs) != 0 {
		t.Errorf("log %q before the outlet's opening ran", entry.Logs)
	}
	if _, err := program.OpeningFlow(flow, outlet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]any{"at": "outlets.remote.opening-flow:1", "print": "inlet"}
	if len(entry.Logs) != 1 || !reflect.DeepEqual(entry.Logs[0], want) {
		t.Errorf("log %q, want the outlet's print of the inlet's mark", entry.Logs)
	}
}

func TestSameNameInletAndOutlet(t *testing.T) {
	// One name for an inlet and an outlet: the kind keeps
	// their functions apart, and each opening runs its own.
	cfg := &setup.Setup{
		Inlets: []*setup.Inlet{{
			Name: "local",
			OpeningFlow: reindent(`
				|print("from inlet")
			`),
		}},
		Outlets: []*setup.Outlet{{
			Name: "local",
			OpeningFlow: reindent(`
				|print("from outlet")
			`),
		}},
	}
	program, err := snippets.Compile(cfg)
	if err != nil {
		t.Fatalf("cannot compile: %v", err)
	}
	entry := logging.Begin("GET", "/", "")
	flow, err := program.OpeningFlow(nil, cfg.Inlets[0], entry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := program.OpeningFlow(flow, cfg.Outlets[0], nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []map[string]any{
		{"at": "inlets.local.opening-flow:1", "print": "from inlet"},
		{"at": "outlets.local.opening-flow:1", "print": "from outlet"},
	}
	if !reflect.DeepEqual(entry.Logs, want) {
		t.Errorf("log %q, want %q", entry.Logs, want)
	}
}

func TestOpeningFlowErrorCarriesSnippetPath(t *testing.T) {
	// A runtime failure in an opening snippet renders the cause
	// alone by default; SetLogAt prefixes the snippet's config
	// path and the remapped position.
	cfg, inlet, _ := config()
	inlet.OpeningFlow = reindent(`
		|return 1 // 0
	`)
	program := compile(t, cfg)
	_, err := program.OpeningFlow(nil, inlet, nil)
	if err == nil {
		t.Fatalf("error = nil, want a failure")
	}
	if want := "floored division by zero"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}

	program = compile(t, cfg)
	program.SetLogAt(true)
	_, err = program.OpeningFlow(nil, inlet, nil)
	if err == nil {
		t.Fatalf("error = nil, want a failure")
	}
	if want := "inlets.local.opening-flow:1:10: floored division by zero"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestCompileErrorNamesSnippet(t *testing.T) {
	// A snippet that does not compile fails Compile, with the
	// snippet's config path and position.
	cfg, inlet, _ := config()
	inlet.OutgoingHeaders = reindent(`
		|def(:=
	`)
	_, err := snippets.Compile(cfg)
	if err == nil || !strings.Contains(err.Error(), "inlets.local.outgoing-headers:1:5:") {
		t.Fatalf("error = %v, want the snippet path and a position", err)
	}
}

func TestPrintReportsSnippetPath(t *testing.T) {
	// Print lands on the entry as a log object: the snippet's
	// config path and line under at, the message under print.
	cfg, inlet, _ := config()
	inlet.OpeningFlow = reindent(`
		|print("hello")
	`)
	program := compile(t, cfg)
	entry := logging.Begin("GET", "/", "")
	if _, err := program.OpeningFlow(nil, inlet, entry); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]any{"at": "inlets.local.opening-flow:1", "print": "hello"}
	if len(entry.Logs) != 1 || !reflect.DeepEqual(entry.Logs[0], want) {
		t.Errorf("log %q, want %q", entry.Logs, want)
	}
}

func TestSetOutletRecordsTheRoute(t *testing.T) {
	// set_outlet records the outlet the request will leave
	// through, while the choice is still open: the record is
	// readable before any outlet opening ran.
	cfg, inlet, _ := config()
	backup := &setup.Outlet{Name: "backup"}
	cfg.Outlets = append(cfg.Outlets, backup)
	inlet.OutgoingHeaders = reindent(`
		|flow.set_outlet("backup")
	`)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("GET", "/", "", http.Header{})
	if err := program.OutgoingHeaders(flow, inlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := flow.Outlet(); got != backup {
		t.Errorf("Outlet() = %v, want the backup outlet", got)
	}
}

func TestSetOutletUnknownFails(t *testing.T) {
	// An unknown outlet name is a starlark failure with the
	// position and the name spelled out.
	cfg, inlet, _ := config()
	inlet.OutgoingHeaders = reindent(`
		|flow.set_outlet("nope")
	`)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("GET", "/", "", http.Header{})
	err = program.OutgoingHeaders(flow, inlet)
	if err == nil || !strings.Contains(err.Error(), `set_outlet: unknown outlet "nope"`) {
		t.Fatalf("error = %v, want the unknown outlet failure", err)
	}
}

func TestSetOutletAfterOutletRunsFails(t *testing.T) {
	// Once the outlet's stage began, the choice is closed:
	// set_outlet from the outlet's own snippets fails.
	cfg, inlet, outlet := config()
	backup := &setup.Outlet{Name: "backup"}
	cfg.Outlets = append(cfg.Outlets, backup)
	outlet.OutgoingHeaders = reindent(`
		|flow.set_outlet("backup")
	`)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := program.OpeningFlow(flow, outlet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("GET", "/", "", http.Header{})
	err = program.OutgoingHeaders(flow, outlet)
	if err == nil || !strings.Contains(err.Error(), "set_outlet: too late to redefine the outlet") {
		t.Fatalf("error = %v, want the too late failure", err)
	}
}

func TestAddMidletQueuesTheMidlet(t *testing.T) {
	// add_midlet appends the midlet to the chain's queue,
	// readable as the next one to run.
	cfg, inlet, _ := config()
	strip := &setup.Midlet{Name: "strip"}
	cfg.Midlets = append(cfg.Midlets, strip)
	inlet.OutgoingHeaders = reindent(`
		|flow.add_midlet("strip")
	`)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("GET", "/", "", http.Header{})
	if err := program.OutgoingHeaders(flow, inlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := flow.PopNextMidlet(); got != strip {
		t.Errorf("PopNextMidlet() = %v, want the strip midlet", got)
	}
	if got := flow.PopNextMidlet(); got != nil {
		t.Errorf("PopNextMidlet() = %v, want the queue drained", got)
	}
}

func TestAddMidletUnknownFails(t *testing.T) {
	// An unknown midlet name is a starlark failure with the
	// name spelled out.
	cfg, inlet, _ := config()
	inlet.OutgoingHeaders = reindent(`
		|flow.add_midlet("nope")
	`)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("GET", "/", "", http.Header{})
	err = program.OutgoingHeaders(flow, inlet)
	if err == nil || !strings.Contains(err.Error(), `add_midlet: unknown midlet "nope"`) {
		t.Fatalf("error = %v, want the unknown midlet failure", err)
	}
}

func TestAddMidletAfterOutletRunsFails(t *testing.T) {
	// The midlet choice closes with the outlet's: add_midlet
	// from the outlet's own snippets fails.
	cfg, inlet, outlet := config()
	strip := &setup.Midlet{Name: "strip"}
	cfg.Midlets = append(cfg.Midlets, strip)
	outlet.OutgoingHeaders = reindent(`
		|flow.add_midlet("strip")
	`)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := program.OpeningFlow(flow, outlet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("GET", "/", "", http.Header{})
	err = program.OutgoingHeaders(flow, outlet)
	if err == nil || !strings.Contains(err.Error(), "add_midlet: too late to add a midlet") {
		t.Fatalf("error = %v, want the too late failure", err)
	}
}

func TestAddMidletAlreadyRanSkipped(t *testing.T) {
	// A midlet runs once: add_midlet naming the running midlet
	// is a no-op, and its later listings never run again.
	cfg, inlet, _ := config()
	loop := &setup.Midlet{Name: "loop", OpeningFlow: reindent(`
		|flow.add_midlet("loop")
	`)}
	cfg.Midlets = append(cfg.Midlets, loop)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := flow.AddMidlet(loop); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := flow.PopNextMidlet(); got != loop {
		t.Fatalf("PopNextMidlet() = %v, want the loop midlet", got)
	}
	// The opening's add_midlet("loop") is a no-op: the midlet
	// is already in the past.
	if _, err := program.OpeningFlow(flow, loop, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := flow.PopNextMidlet(); got != nil {
		t.Errorf("PopNextMidlet() = %v, want the queue empty", got)
	}
}

func TestMidletSettleRecordsOutletAndRunsOpeningFlow(t *testing.T) {
	// Settling a midlet records the midlet's static outlet over
	// an earlier one, and runs its opening snippet.
	cfg, inlet, _ := config()
	_, outlet := cfg.Inlets[0], cfg.Outlets[0]
	backup := &setup.Outlet{Name: "backup"}
	cfg.Outlets = append(cfg.Outlets, backup)
	strip := &setup.Midlet{
		Name:   "strip",
		Outlet: "backup",
		OpeningFlow: reindent(`
		|print(flow.state["mark"])
		`),
	}
	cfg.Midlets = append(cfg.Midlets, strip)
	inlet.OpeningFlow = reindent(`
		|flow.state["mark"] = "kept"
	`)
	inlet.OutgoingHeaders = reindent(`
		|flow.set_outlet("remote")
	`)
	entry := logging.Begin("GET", "/", "")
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, entry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("GET", "/", "", http.Header{})
	if err := program.OutgoingHeaders(flow, inlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := flow.Outlet(); got != outlet {
		t.Fatalf("Outlet() = %v, want the inlet's record", got)
	}
	if _, err := program.OpeningFlow(flow, strip, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := flow.Outlet(); got != backup {
		t.Errorf("Outlet() = %v, want the midlet's static outlet", got)
	}
	if got := flow.PopNextMidlet(); got != nil {
		t.Errorf("PopNextMidlet() = %v, want the queue empty", got)
	}
	want := map[string]any{"at": "midlets.strip.opening-flow:1", "print": "kept"}
	if len(entry.Logs) != 1 || !reflect.DeepEqual(entry.Logs[0], want) {
		t.Errorf("log %q, want the midlet's print of the inlet's mark", entry.Logs)
	}
}

func TestOpeningFlowStartRules(t *testing.T) {
	// A flow starts on an inlet only, and an inlet is refused
	// once the flow started: both are caller bugs, not
	// snippet failures.
	cfg, inlet, outlet := config()
	inlet.OpeningFlow = reindent(`
		|flow.state["mark"] = "ok"
	`)
	midlet := &setup.Midlet{Name: "strip"}
	cfg.Midlets = append(cfg.Midlets, midlet)
	program := compile(t, cfg)
	testCases := []struct {
		s    setup.Sniplet
		want string
	}{{
		s:    outlet,
		want: `cannot start a flow on "outlet"`,
	}, {
		s:    midlet,
		want: `cannot start a flow on "midlet"`,
	}}
	for _, tc := range testCases {
		if _, err := program.OpeningFlow(nil, tc.s, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("OpeningFlow(nil, %s): error = %v, want it to contain %q", tc.s.Kindlet(), err, tc.want)
		}
	}
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := program.OpeningFlow(flow, inlet, nil); err == nil || !strings.Contains(err.Error(), "flow already started on inlet \"local\"") {
		t.Errorf("OpeningFlow(flow, inlet): error = %v, want the already started failure", err)
	}
}

func TestClosingFlowRunsInReverseOrder(t *testing.T) {
	// The closings run once the whole flow has finished, in
	// reverse opening order: the outlet, the midlet, the inlet.
	// Each sees the shared state, so the inlet's opening is
	// readable in every closing.
	cfg, inlet, outlet := config()
	strip := &setup.Midlet{Name: "strip"}
	cfg.Midlets = append(cfg.Midlets, strip)
	inlet.OpeningFlow = reindent(`
		|flow.state["mark"] = "kept"
	`)
	inlet.ClosingFlow = reindent(`
		|print("inlet:" + flow.state["mark"])
	`)
	strip.ClosingFlow = reindent(`
		|print("midlet")
	`)
	outlet.ClosingFlow = reindent(`
		|print("outlet:" + flow.state["mark"])
	`)
	program := compile(t, cfg)
	entry := logging.Begin("GET", "/", "")
	flow, err := program.OpeningFlow(nil, inlet, entry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := program.OpeningFlow(flow, strip, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := program.OpeningFlow(flow, outlet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.LockOut()
	flow.LockIn()
	program.ClosingFlow(flow)
	want := []map[string]any{
		{"at": "outlets.remote.closing-flow:1", "print": "outlet:kept"},
		{"at": "midlets.strip.closing-flow:1", "print": "midlet"},
		{"at": "inlets.local.closing-flow:1", "print": "inlet:kept"},
	}
	if !reflect.DeepEqual(entry.Logs, want) {
		t.Errorf("log %q, want %q", entry.Logs, want)
	}
}

func TestClosingFlowSkipsFailedOpenings(t *testing.T) {
	// A sniplet whose opening failed never closes - things that
	// did not open are not closed - but the ones that opened
	// before it still do.
	cfg, inlet, _ := config()
	strip := &setup.Midlet{
		Name: "strip",
		OpeningFlow: reindent(`
			|fail("no")
		`),
		ClosingFlow: reindent(`
			|print("midlet")
		`),
	}
	cfg.Midlets = append(cfg.Midlets, strip)
	inlet.ClosingFlow = reindent(`
		|print("inlet")
	`)
	program := compile(t, cfg)
	entry := logging.Begin("GET", "/", "")
	flow, err := program.OpeningFlow(nil, inlet, entry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := program.OpeningFlow(flow, strip, nil); err == nil {
		t.Fatalf("error = nil, want the midlet's opening failure")
	}
	flow.LockOut()
	flow.LockIn()
	program.ClosingFlow(flow)
	want := []map[string]any{
		{"at": "inlets.local.closing-flow:1", "print": "inlet"},
	}
	if !reflect.DeepEqual(entry.Logs, want) {
		t.Errorf("log %q, want %q", entry.Logs, want)
	}
}

func TestClosingFlowErrorStillCloses(t *testing.T) {
	// The flow is finished when the closings run, so a closing
	// failure cannot change anything: the error lands on the
	// request's log entry, and the remaining closings still run.
	cfg, inlet, outlet := config()
	outlet.ClosingFlow = reindent(`
		|return 1 // 0
	`)
	inlet.ClosingFlow = reindent(`
		|print("inlet")
	`)
	program := compile(t, cfg)
	entry := logging.Begin("GET", "/", "")
	flow, err := program.OpeningFlow(nil, inlet, entry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := program.OpeningFlow(flow, outlet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.LockOut()
	flow.LockIn()
	program.ClosingFlow(flow)
	want := []map[string]any{
		{"error": "floored division by zero"},
		{"at": "inlets.local.closing-flow:1", "print": "inlet"},
	}
	if !reflect.DeepEqual(entry.Logs, want) {
		t.Errorf("log %q, want %q", entry.Logs, want)
	}
}

func TestClosingFlowRunsOnce(t *testing.T) {
	// A second ClosingFlow is a no-op: the flow left closing
	// with the first, so the closings never run twice - the
	// closing half's mirror of the opening's rerun refusal.
	cfg, inlet, _ := config()
	inlet.ClosingFlow = reindent(`
		|print("closing")
	`)
	program := compile(t, cfg)
	entry := logging.Begin("GET", "/", "")
	flow, err := program.OpeningFlow(nil, inlet, entry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.LockOut()
	flow.LockIn()
	program.ClosingFlow(flow)
	program.ClosingFlow(flow)
	want := []map[string]any{
		{"at": "inlets.local.closing-flow:1", "print": "closing"},
	}
	if !reflect.DeepEqual(entry.Logs, want) {
		t.Errorf("log %q, want the closings run exactly once", entry.Logs)
	}
}

func TestOpeningFlowDoubleSettleFails(t *testing.T) {
	// Settling an outlet or midlet twice fails: the second
	// settle would re-run the opening on a flow whose routing
	// has already moved past it.
	cfg, inlet, outlet := config()
	inlet.OpeningFlow = reindent(`
		|flow.state["mark"] = "ok"
	`)
	midlet := &setup.Midlet{Name: "strip"}
	cfg.Midlets = append(cfg.Midlets, midlet)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := program.OpeningFlow(flow, midlet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := program.OpeningFlow(flow, midlet, nil); err == nil || !strings.Contains(err.Error(), `cannot use midlet "strip" twice`) {
		t.Errorf("second midlet settle: error = %v, want the twice failure", err)
	}
	if _, err := program.OpeningFlow(flow, outlet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := program.OpeningFlow(flow, outlet, nil); err == nil || !strings.Contains(err.Error(), "set_outlet: too late to redefine the outlet") {
		t.Errorf("second outlet settle: error = %v, want the too late failure", err)
	}
}

func TestOutgoingHeaders(t *testing.T) {
	// The header filter runs one entity's snippet on the
	// headers: additions and deletions both survive, and the
	// flow state carries data between calls.
	cfg, inlet, _ := config()
	inlet.OutgoingHeaders = reindent(`
		|flow.state["seen"] = "yes"
		|headers["X-Inlet"] = "yes"
		|headers.pop("X-Old")
	`)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	headers := http.Header{}
	headers.Set("X-Old", "gone")
	flow.SetOut("GET", "/", "", headers)
	if err := program.OutgoingHeaders(flow, inlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	outHeaders := flow.OutHeaders()
	if outHeaders.Get("X-Inlet") != "yes" {
		t.Errorf("X-Inlet = %q, want yes", outHeaders.Get("X-Inlet"))
	}
	if _, ok := outHeaders["X-Old"]; ok {
		t.Errorf("X-Old = %q, want deleted", outHeaders.Get("X-Old"))
	}
}

func TestHeadersShareFlowState(t *testing.T) {
	// The outlet's filter reads what the inlet's recorded on
	// the same flow.
	cfg, inlet, outlet := config()
	inlet.OutgoingHeaders = reindent(`
		|flow.state["from_inlet"] = "yes"
	`)
	outlet.OutgoingHeaders = reindent(`
		|if flow.state["from_inlet"] != "yes":
		|    fail("inlet did not run first")
	`)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := program.OpeningFlow(flow, outlet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	flow.SetOut("GET", "/", "", http.Header{})
	if err := program.OutgoingHeaders(flow, inlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := program.OutgoingHeaders(flow, outlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHeaderFiltersCollapseMultiValue(t *testing.T) {
	// A multi-valued header holds one value in starlark - the
	// first - and on the way back replaces the whole list.
	cfg, inlet, _ := config()
	inlet.OutgoingHeaders = reindent(`
		|headers["X-Multi"] += "!"
	`)
	program := compile(t, cfg)
	flow, err := program.OpeningFlow(nil, inlet, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	headers := http.Header{}
	headers.Add("X-Multi", "a")
	headers.Add("X-Multi", "b")
	flow.SetOut("GET", "/", "", headers)
	if err := program.OutgoingHeaders(flow, inlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := flow.OutHeaders()["X-Multi"]; len(got) != 1 || got[0] != "a!" {
		t.Errorf("X-Multi = %v, want one value a!", got)
	}
}

func TestNilProgramCallsAreNoOps(t *testing.T) {
	// A nil program and a nil flow leave headers and
	// bodies untouched: the checks the proxy relies on to
	// call without asking.
	var program *snippets.Program
	inlet := &setup.Inlet{Name: "local"}
	outlet := &setup.Outlet{Name: "remote"}
	headers := http.Header{}
	headers.Set("X-Keep", "yes")
	if err := program.OutgoingHeaders(nil, inlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := program.IncomingHeaders(nil, inlet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if headers.Get("X-Keep") != "yes" || len(headers) != 1 {
		t.Errorf("headers = %v, want untouched", headers)
	}
	body := []byte(`{"keep":"yes"}`)
	value := snippets.NewJSONValue(body)
	if err := program.OutgoingBody(nil, inlet, value); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := value.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("body = %q, want untouched", got)
	}
	if err := program.IncomingBody(nil, outlet, value); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err = value.Bytes()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("body = %q, want untouched", got)
	}
	if program.FiltersOutgoingBody(inlet) || program.FiltersIncomingBody(inlet) {
		t.Errorf("nil program reports body filters")
	}
}

func TestAnyOutletFiltersBodies(t *testing.T) {
	// FiltersOutgoingBody is a per-sniplet fact: an inlet without
	// outgoing-body does not buffer even when an outlet holds one -
	// the buffering happens at the sniplet that filters.
	cfg, inlet, outlet := config()
	outlet.OutgoingBody = &setup.BodyFilter{Body: reindent(`
		|flow.state["seen"] = "yes"
	`)}
	program := compile(t, cfg)
	if program.FiltersOutgoingBody(inlet) {
		t.Errorf("inlet without a body filter reported")
	}
	if !program.FiltersOutgoingBody(outlet) {
		t.Errorf("outlet with a body filter not reported")
	}
}

func TestFailAbortsTheCall(t *testing.T) {
	// fail is a universe builtin: calling it aborts the snippet
	// with its message on the error. Mutations before it stand.
	// The message renders alone by default; SetLogAt prefixes
	// the call site's position.
	cfg, inlet, _ := config()
	inlet.OpeningFlow = reindent(`
		|flow.state["mark"] = "set"
		|fail("rejected: bad hair day")
	`)
	program := compile(t, cfg)
	_, err := program.OpeningFlow(nil, inlet, nil)
	if err == nil {
		t.Fatalf("error = nil, want a failure")
	}
	if want := "rejected: bad hair day"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}

	program = compile(t, cfg)
	program.SetLogAt(true)
	_, err = program.OpeningFlow(nil, inlet, nil)
	if err == nil {
		t.Fatalf("error = nil, want a failure")
	}
	if want := "inlets.local.opening-flow:2:5: rejected: bad hair day"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestFailFields(t *testing.T) {
	// fail's code= and body= reach the caller as FailError
	// fields through errors.As, with the backtrace intact.
	testCases := []struct {
		name    string
		snippet string
		want    snippets.FailError
	}{{
		name:    "plain message",
		snippet: `fail("gone")`,
		want:    snippets.FailError{Message: "gone"},
	}, {
		name:    "joined with sep",
		snippet: `fail("a", "b", sep=", ")`,
		want:    snippets.FailError{Message: "a, b"},
	}, {
		name:    "non-string rendered",
		snippet: `fail("id", 42)`,
		want:    snippets.FailError{Message: "id 42"},
	}, {
		name:    "code and string body",
		snippet: `fail("slow", code=429, body="try later")`,
		want:    snippets.FailError{Message: "slow", Code: 429, Body: "try later"},
	}, {
		name:    "map body becomes json",
		snippet: `fail("bad", code=400, body={"error": "missing", "retry": False})`,
		want:    snippets.FailError{Message: "bad", Code: 400, Body: `{"error":"missing","retry":false}`, BodyJSON: true},
	}, {
		name:    "map body escapes plainly",
		snippet: `fail("bad", body={"error": "a&b <c>"})`,
		want:    snippets.FailError{Message: "bad", Body: `{"error":"a&b <c>"}`, BodyJSON: true},
	}}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, inlet, _ := config()
			inlet.OpeningFlow = tc.snippet
			program := compile(t, cfg)
			_, err := program.OpeningFlow(nil, inlet, nil)
			var failErr *snippets.FailError
			if err == nil || !errors.As(err, &failErr) {
				t.Fatalf("error = %v, want a FailError", err)
			}
			if *failErr != tc.want {
				t.Errorf("FailError = %+v, want %+v", *failErr, tc.want)
			}
		})
	}
}

func TestLogAppendsToEntry(t *testing.T) {
	// log() appends the dict to the request's entry, as one
	// object. Values convert for JSON: None to nil, lists and
	// dicts recursively. With SetLogAt on, the call's position
	// joins the keys as at; without it, no at - the default.
	cfg, inlet, _ := config()
	inlet.OpeningFlow = reindent(`
		|log({"key": "value", "n": 1, "none": None, "list": [1, "two"], "dict": {"k": "v"}})
	`)
	program := compile(t, cfg)
	entry := logging.Begin("GET", "/", "")
	if _, err := program.OpeningFlow(nil, inlet, entry); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entry.Logs) != 1 {
		t.Fatalf("Logs = %q, want one object", entry.Logs)
	}
	want := map[string]any{
		"key":  "value",
		"n":    int64(1),
		"none": nil,
		"list": []any{int64(1), "two"},
		"dict": map[string]any{"k": "v"},
	}
	if !reflect.DeepEqual(entry.Logs[0], want) {
		t.Errorf("log object = %q, want %q without at", entry.Logs[0], want)
	}

	inlet.OpeningFlow = reindent(`
		|log({"key": "value"})
	`)
	program = compile(t, cfg)
	program.SetLogAt(true)
	entry = logging.Begin("GET", "/", "")
	if _, err := program.OpeningFlow(nil, inlet, entry); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want = map[string]any{
		"at":  "inlets.local.opening-flow:1",
		"key": "value",
	}
	if !reflect.DeepEqual(entry.Logs[0], want) {
		t.Errorf("log object = %q, want %q", entry.Logs[0], want)
	}
}

func TestLogStacksCalls(t *testing.T) {
	// Repeated calls stack in order, each with its own at once
	// SetLogAt is on.
	cfg, inlet, _ := config()
	inlet.OpeningFlow = reindent(`
		|log({"a": 1})
		|log({"b": 2})
	`)
	program := compile(t, cfg)
	program.SetLogAt(true)
	entry := logging.Begin("GET", "/", "")
	if _, err := program.OpeningFlow(nil, inlet, entry); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entry.Logs) != 2 {
		t.Fatalf("Logs = %q, want two objects", entry.Logs)
	}
	if entry.Logs[0]["at"] != "inlets.local.opening-flow:1" || entry.Logs[0]["a"] != int64(1) {
		t.Errorf("first object = %q, want a at line 1", entry.Logs[0])
	}
	if entry.Logs[1]["at"] != "inlets.local.opening-flow:2" || entry.Logs[1]["b"] != int64(2) {
		t.Errorf("second object = %q, want b at line 2", entry.Logs[1])
	}
}

func TestLogRejectsBadArguments(t *testing.T) {
	// A non-dict argument and an unconvertible value fail the
	// call, naming log.
	testCases := []struct {
		name, snippet, want string
	}{{
		name:    "not a dict",
		snippet: `log("hello")`,
		want:    "log: for parameter 1: got string, want dict",
	}, {
		name:    "unconvertible value",
		snippet: `log({"f": lambda: 1})`,
		want:    "log: cannot convert",
	}}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, inlet, _ := config()
			inlet.OpeningFlow = tc.snippet
			program := compile(t, cfg)
			_, err := program.OpeningFlow(nil, inlet, logging.Begin("GET", "/", ""))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
