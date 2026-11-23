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

package scripts_test

import (
	"strings"
	"testing"

	"github.com/canonical/rebear/internal/scripts"
	"go.starlark.net/starlark"
)

// reindent strips each line up to and including its first "|",
// keeping the suffix as the line, and drops lines without one.
// It lets multi-line expected text sit pipe-prefixed and
// indented at the depth of the code around it.
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

// script builds one from the options, failing the test on any
// compile error.
func newScript(t *testing.T, opts *scripts.Options) *scripts.Script {
	t.Helper()
	m, err := scripts.New(opts)
	if err != nil {
		t.Fatalf("cannot build script: %v", err)
	}
	return m
}

func newState(t *testing.T, s *scripts.Script) *scripts.State {
	t.Helper()
	return s.Init()
}

func dict(t *testing.T, entries ...starlark.Value) *starlark.Dict {
	t.Helper()
	d := starlark.NewDict(len(entries) / 2)
	for i := 0; i+1 < len(entries); i += 2 {
		if err := d.SetKey(entries[i], entries[i+1]); err != nil {
			t.Fatalf("cannot build dict: %v", err)
		}
	}
	return d
}

func TestCallMutatesNamespace(t *testing.T) {
	// A dict handed in is the same object the function mutates:
	// the caller reads the change back. No return gives None.
	m := newScript(t, &scripts.Options{
		Funcs: []scripts.Func{{
			Name: "mark",
			Args: []string{"state", "headers"},
			Body: `headers["X-Mark"] = "yes"
`,
		}},
	})
	st := newState(t, m)
	state := starlark.NewDict(0)
	headers := dict(t, starlark.String("Host"), starlark.String("example"))

	result, err := st.Call("mark", "state", state, "headers", headers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != starlark.None {
		t.Errorf("result = %v, want None", result)
	}
	got, _, err := headers.Get(starlark.String("X-Mark"))
	if err != nil || got != starlark.String("yes") {
		t.Errorf("X-Mark = %v (err %v), want yes", got, err)
	}
}

func TestStateSharedAcrossFunctions(t *testing.T) {
	// State passed to every function is the flow's shared
	// mutable data: init writes it, a later function reads it.
	m := newScript(t, &scripts.Options{
		Funcs: []scripts.Func{{
			Name: "init",
			Args: []string{"state"},
			Body: `state["count"] = 1
`,
		}, {
			Name: "bump",
			Args: []string{"state"},
			Body: `return state["count"] + 1
`,
		}},
	})
	st := newState(t, m)
	state := starlark.NewDict(1)
	if _, err := st.Call("init", "state", state); err != nil {
		t.Fatalf("init failed: %v", err)
	}
	result, err := st.Call("bump", "state", state)
	if err != nil {
		t.Fatalf("bump failed: %v", err)
	}
	if result != starlark.MakeInt(2) {
		t.Errorf("result = %v, want 2", result)
	}
}

func TestFunctionReadsGlue(t *testing.T) {
	// Two defs plus Init glue in one file: both functions run,
	// both reach the glue's constants.
	m := newScript(t, &scripts.Options{
		Funcs: []scripts.Func{{
			Name: "limit",
			Args: []string{"state"},
			Body: `return LIMIT
`,
		}, {
			Name: "double_limit",
			Args: []string{"state"},
			Body: `return LIMIT * 2
`,
		}, {
			// No body: assembly must emit a pass.
			Name: "noop",
			Args: []string{"state"},
		}},
		Init: []string{"LIMIT = 10"},
	})
	st := newState(t, m)
	one, err := st.Call("limit", "state", starlark.NewDict(0))
	if err != nil {
		t.Fatalf("limit failed: %v", err)
	}
	if one != starlark.MakeInt(10) {
		t.Errorf("limit = %v, want 10", one)
	}
	two, err := st.Call("double_limit", "state", starlark.NewDict(0))
	if err != nil {
		t.Fatalf("double_limit failed: %v", err)
	}
	if two != starlark.MakeInt(20) {
		t.Errorf("double_limit = %v, want 20", two)
	}
	if _, err := st.Call("noop", "state", starlark.NewDict(0)); err != nil {
		t.Fatalf("noop failed: %v", err)
	}
}

func TestGlobalsFrozen(t *testing.T) {
	// Freezing is transitive: glue containers freeze too, and a
	// function mutating one fails at the call.
	m := newScript(t, &scripts.Options{
		Funcs: []scripts.Func{{
			Name: "poke",
			Args: []string{"state"},
			Body: `GLOBAL["a"] = 1
`,
		}},
		Init: []string{"GLOBAL = {}"},
	})
	st := newState(t, m)
	_, err := st.Call("poke", "state", starlark.NewDict(0))
	if err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Errorf("error = %v, want a frozen dict failure", err)
	}
}

func TestPrint(t *testing.T) {
	// Print reaches the callback with the call site's position
	// in the snippet, beside the message.
	var got []string
	m := newScript(t, &scripts.Options{
		Print: func(t *starlark.Thread, at, msg string) { got = append(got, at, msg) },
		Funcs: []scripts.Func{{
			Name: "hello",
			Args: []string{"state"},
			Body: `print("hello")
`,
		}},
	})
	st := newState(t, m)
	if _, err := st.Call("hello", "state", starlark.NewDict(0)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "rebear:1:6" || got[1] != "hello" {
		t.Errorf("printed %q, want rebear:1:6 hello", got)
	}
}

func TestPrintInNestedDef(t *testing.T) {
	// Print inside a nested def reports the print call site in
	// the defining snippet, at the call's open paren.
	var got []string
	m := newScript(t, &scripts.Options{
		Print: func(t *starlark.Thread, at, msg string) { got = append(got, at, msg) },
		Funcs: []scripts.Func{{
			Name: "hello",
			Args: []string{"state"},
			Body: `def helper():
    print("in helper")
return helper()
`,
		}},
	})
	st := newState(t, m)
	if _, err := st.Call("hello", "state", starlark.NewDict(0)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "rebear:2:10" || got[1] != "in helper" {
		t.Errorf("printed %q, want rebear:2:10 in helper", got)
	}
}

func TestPrintAtReportsSnippetsAt(t *testing.T) {
	// A func with At reports print through its config path,
	// not the file name.
	var got []string
	m := newScript(t, &scripts.Options{
		Print: func(t *starlark.Thread, at, msg string) { got = append(got, at, msg) },
		Funcs: []scripts.Func{{
			Name: "local_inlet_opening_flow",
			Args: []string{"flow"},
			At:   "inlets.local.opening-flow",
			Body: `print("from inlet")
`,
		}},
	})
	st := newState(t, m)
	if _, err := st.Call("local_inlet_opening_flow", "flow", starlark.NewDict(0)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "inlets.local.opening-flow:1:6" || got[1] != "from inlet" {
		t.Errorf("printed %q, want inlets.local.opening-flow:1:6 from inlet", got)
	}
}

func TestEvalErrorOneLine(t *testing.T) {
	// A runtime failure renders as the cause alone by default;
	// SetLogAt prefixes the innermost frame's remapped position -
	// here `//` at the user's column 10, not the assembled 14 -
	// with no function name.
	m := newScript(t, &scripts.Options{
		Funcs: []scripts.Func{{
			Name: "boom",
			Args: []string{"state"},
			Body: `return 1 // 0
`,
		}},
	})
	st := newState(t, m)
	_, err := st.Call("boom", "state", starlark.NewDict(0))
	if err == nil {
		t.Fatalf("error = nil, want a failure")
	}
	want := "floored division by zero"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}

	st = newState(t, m)
	st.SetLogAt(true)
	_, err = st.Call("boom", "state", starlark.NewDict(0))
	if err == nil {
		t.Fatalf("error = nil, want a failure")
	}
	want = "rebear:1:10: floored division by zero"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestEvalErrorInNestedDef(t *testing.T) {
	// A failure inside a nested def reports the innermost
	// frame - the failing line in the defining snippet, not
	// the outer call - and only with SetLogAt on.
	m := newScript(t, &scripts.Options{
		Funcs: []scripts.Func{{
			Name: "boom",
			Args: []string{"state"},
			Body: `def helper():
    return 1 // 0
return helper()
`,
		}},
	})
	st := newState(t, m)
	_, err := st.Call("boom", "state", starlark.NewDict(0))
	if err == nil {
		t.Fatalf("error = nil, want a failure")
	}
	if want := "floored division by zero"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}

	st = newState(t, m)
	st.SetLogAt(true)
	_, err = st.Call("boom", "state", starlark.NewDict(0))
	if err == nil {
		t.Fatalf("error = nil, want a failure")
	}
	want := "rebear:2:14: floored division by zero"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestCompileErrorAtStartup(t *testing.T) {
	// A snippet that does not parse fails construction, naming
	// the file and position.
	_, err := scripts.New(&scripts.Options{
		Funcs: []scripts.Func{{
			Name: "broken",
			Args: []string{"state"},
			Body: `def(:=
`,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "rebear:") {
		t.Errorf("error = %v, want it to name rebear and a position", err)
	}
}

func TestNameInErrors(t *testing.T) {
	// A custom Name is what errors call the file, so the author
	// can tell their pairs apart.
	_, err := scripts.New(&scripts.Options{
		Name: "inbox-pair",
		Funcs: []scripts.Func{{
			Name: "broken",
			Args: []string{"state"},
			Body: `def(:=
`,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "inbox-pair:") {
		t.Errorf("error = %v, want it to name inbox-pair and a position", err)
	}
}

func TestPositionsRemapped(t *testing.T) {
	// Assembled bodies sit after an invisible 4-space indent;
	// reported lines count from the snippet's first line and
	// columns drop the baked indent, so positions point into
	// the snippet text as the author wrote it. Indent adds the
	// snippet's own column offset in its original source.
	_, err := scripts.New(&scripts.Options{
		Name: "pair",
		Funcs: []scripts.Func{{
			Name: "broken",
			Args: []string{"state"},
			Body: `def(:=
`,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "pair:1:5:") {
		t.Errorf("error = %v, want position pair:1:5 (raw is pair:2:9)", err)
	}

	_, err = scripts.New(&scripts.Options{
		Name: "pair",
		Funcs: []scripts.Func{{
			Name: "broken",
			Args: []string{"state"},
			Body: `def(:=
`,
			Indent: 8,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "pair:1:13:") {
		t.Errorf("error = %v, want position pair:1:13 (Indent 8 adds to the column)", err)
	}
}

func TestCallErrors(t *testing.T) {
	testCases := []struct {
		name   string
		cnf    scripts.Options
		call   string
		kwargs []any
		want   string
		// exact spells the wrapper's own errors in full; starlark's
		// call errors keep a backtrace and match by substring.
		exact bool
	}{{
		name:   "unknown function",
		cnf:    scripts.Options{Name: "pair"},
		call:   "nope",
		kwargs: nil,
		want:   `pair: unknown function "nope"`,
		exact:  true,
	}, {
		name: "not a function",
		cnf: scripts.Options{
			Name: "pair",
			Funcs: []scripts.Func{{
				Name: "f",
				Args: []string{"state"},
				Body: "pass\n",
			}},
			Init: []string{"CONST = 1"},
		},
		call:   "CONST",
		kwargs: nil,
		want:   `pair: "CONST" is not a function`,
		exact:  true,
	}, {
		name: "missing argument",
		cnf: scripts.Options{
			Funcs: []scripts.Func{{
				Name: "f",
				Args: []string{"state", "headers"},
				Body: "pass\n",
			}},
		},
		call:   "f",
		kwargs: []any{"state", starlark.NewDict(0)},
		want:   "missing 1 argument (headers)",
	}, {
		name: "unexpected keyword",
		cnf: scripts.Options{
			Funcs: []scripts.Func{{
				Name: "f",
				Args: []string{"state"},
				Body: "pass\n",
			}},
		},
		call:   "f",
		kwargs: []any{"state", starlark.NewDict(0), "extra", starlark.None},
		want:   `unexpected keyword argument "extra"`,
	}}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := scripts.New(&tc.cnf)
			if err != nil {
				t.Fatalf("cannot build script: %v", err)
			}
			st := newState(t, m)
			_, err = st.Call(tc.call, tc.kwargs...)
			if tc.exact {
				if err == nil || err.Error() != tc.want {
					t.Fatalf("error = %v, want %q", err, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestFlowsAreIsolated(t *testing.T) {
	// Flows share the frozen functions but nothing mutable:
	// one flow's state is invisible to the other.
	m := newScript(t, &scripts.Options{
		Funcs: []scripts.Func{{
			Name: "init",
			Args: []string{"state"},
			Body: `state["owner"] = "a"
`,
		}},
	})
	flows := make([]*scripts.State, 2)
	for i := range flows {
		flows[i] = newState(t, m)
	}
	for i, st := range flows {
		state := starlark.NewDict(1)
		if _, err := st.Call("init", "state", state); err != nil {
			t.Fatalf("flow %d failed: %v", i, err)
		}
		if state.Len() != 1 {
			t.Errorf("flow %d state = %v, want one entry", i, state)
		}
	}
}

func TestNilOptions(t *testing.T) {
	// Nil options is the default: an empty file compiles, and a
	// flow starts with no functions.
	m, err := scripts.New(nil)
	if err != nil {
		t.Fatalf("cannot build script: %v", err)
	}
	st := m.Init()
	if _, err := st.Call("anything"); err == nil {
		t.Errorf("error = nil, want unknown function")
	}
}
