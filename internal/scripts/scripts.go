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

// Package scripts wraps starlark for rebear's snippets. The
// snippets of one inlet+outlet pair assemble into one starlark
// file, compiled and run once; the frozen globals are shared by
// every flow. Call arguments are the only mutable data - the
// flow's shared state is one of them, passed by the caller.
package scripts

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// Func is one def assembled into the file.
type Func struct {
	Name string
	Args []string
	Body string

	// At is the snippet's config path, like inlets.local.outgoing-headers;
	// it replaces the file name in this func's positions. Empty falls back
	// to the script's own name.
	At string

	// Indent is the column of Body's lines in their original source, so
	// error positions land on the author's text. Default 0, so the first
	// column of Body is 1.
	Indent int
}

// Options configures the script. A nil pointer is the default.
type Options struct {
	// Name is the file's name in compile and runtime errors; empty means
	// "rebear".
	Name string
	// Print receives the print builtin's output: the thread the
	// call ran on, the call site's remapped position, and the
	// message. Nil discards it.
	Print func(t *starlark.Thread, at, msg string)
	// Funcs are the definitions assembled into the file, in order.
	Funcs []Func
	// Init is host glue written at the end of the file, at column 1.
	Init []string
	// Builtins are host definitions visible to the whole file, shadowing
	// same-named universe builtins. The dict is used as-is and stays owned
	// by the caller.
	Builtins starlark.StringDict
}

// Script is the compiled and initialized snippets: frozen globals shared
// by every flow. Built once at startup.
type Script struct {
	name    string
	globals starlark.StringDict
	print   func(t *starlark.Thread, at, msg string)
	spans   []funcSpan
}

// funcSpan is one func body's line range in the assembled file. Positions
// inside it report as At with the body's first line discounted to 1.
type funcSpan struct {
	at     string
	indent int
	first  int
	last   int
}

// bodyIndent is the indentation assemble gives def bodies.
const bodyIndent = 4

// New assembles, compiles, and runs the file once. Any snippet failure
// lands here, at startup, with positions remapped to the snippet text. The
// frozen globals live on the script.
func New(opts *Options) (*Script, error) {
	if opts == nil {
		opts = &Options{}
	}
	name := opts.Name
	if name == "" {
		name = "rebear"
	}
	src, spans := assemble(opts)
	predeclared := starlark.StringDict{}
	for key, value := range opts.Builtins {
		predeclared[key] = value
	}
	_, program, err := starlark.SourceProgramOptions(&syntax.FileOptions{}, name, src, predeclared.Has)
	if err != nil {
		return nil, errors.New(remapPos(name, spans, err.Error()))
	}
	script := &Script{name: name, globals: nil, print: opts.Print, spans: spans}
	thread := script.newThread()
	globals, err := program.Init(thread, predeclared)
	if err != nil {
		if evalErr, ok := err.(*starlark.EvalError); ok {
			err = fmt.Errorf("%s%w", oneLine(name, spans, evalErr.Backtrace(), evalErr.Msg), wrappedCause{evalErr.Unwrap()})
		}
		return nil, err
	}
	globals.Freeze()
	script.globals = globals
	return script, nil
}

// newThread builds one thread with print routed through position: the
// print call site lands in the author's snippet, its position prefixing
// the message.
func (s *Script) newThread() *starlark.Thread {
	thread := &starlark.Thread{Name: s.name}
	print := s.print
	thread.Print = func(t *starlark.Thread, msg string) {
		if print == nil {
			return
		}
		print(t, s.at(t), msg)
	}
	return thread
}

// at returns the innermost non-builtin frame's position, remapped
// to the snippet's own source - or "" when the stack holds no
// such frame.
func (s *Script) at(t *starlark.Thread) string {
	stack := t.CallStack()
	for i := len(stack) - 1; i >= 0; i-- {
		if p := stack[i].Pos.String(); p != "<builtin>" {
			return position(s.spans, s.name, p)
		}
	}
	return ""
}

// assemble writes one def per Func, then Init, and indents def bodies one
// level. An empty body becomes a pass. It also reports each func body's
// line span, feeding remapPos.
func assemble(opts *Options) (string, []funcSpan) {
	var out strings.Builder
	var spans []funcSpan
	line := 0
	for _, fn := range opts.Funcs {
		fmt.Fprintf(&out, "def %s(%s):\n", fn.Name, strings.Join(fn.Args, ", "))
		line++
		first := line + 1
		body := strings.Trim(fn.Body, "\n")
		if body == "" {
			out.WriteString("    pass\n")
			line++
			spans = append(spans, funcSpan{at: fn.At, indent: fn.Indent, first: first, last: line})
			continue
		}
		for _, text := range strings.Split(body, "\n") {
			if text == "" {
				out.WriteString("\n")
			} else {
				out.WriteString("    ")
				out.WriteString(text)
				out.WriteString("\n")
			}
			line++
		}
		spans = append(spans, funcSpan{at: fn.At, indent: fn.Indent, first: first, last: line})
	}
	for _, text := range opts.Init {
		out.WriteString(text)
		out.WriteString("\n")
		line++
	}
	_ = line
	return out.String(), spans
}

// remapPos rewrites name:l:c positions in msg to the author's snippet
// position, via position.
func remapPos(name string, spans []funcSpan, msg string) string {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `:(\d+):(\d+)\b`)
	return re.ReplaceAllStringFunc(msg, func(m string) string {
		return position(spans, name, m)
	})
}

// position maps one raw name:line:col from the assembled file to the
// snippet's own position: the snippet's At (the file name when unset), the
// line counted from its first line, and the column without the baked
// indent. Positions outside any body keep the file's line and column as
// is.
func position(spans []funcSpan, name, pos string) string {
	parts := strings.Split(pos, ":")
	if len(parts) != 3 {
		return pos
	}
	line, err1 := strconv.Atoi(parts[1])
	col, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil {
		return pos
	}
	for _, sp := range spans {
		if line >= sp.first && line <= sp.last {
			at := sp.at
			if at == "" {
				at = name
			}
			return fmt.Sprintf("%s:%d:%d", at, line-sp.first+1, sp.indent+col-bodyIndent)
		}
	}
	return pos
}

// State is one flow: a thread calling the script's frozen functions.
// Only the call arguments are mutable.
type State struct {
	script *Script
	thread *starlark.Thread
	// logAt prefixes call errors with the call site's position;
	// off by default, so errors carry the message alone.
	logAt bool
}

// SetLogAt turns the position prefix on call errors on or off;
// off by default.
func (s *State) SetLogAt(on bool) {
	s.logAt = on
}

// Init starts one flow. Nothing starlark-side is allocated: the work
// happened once, in New.
func (s *Script) Init() *State {
	return &State{script: s, thread: s.newThread()}
}

// Has reports whether the script defines the named function.
func (s *Script) Has(name string) bool {
	_, ok := s.globals[name]
	return ok
}

// At reports the current call site's position, remapped to the
// snippet's own source. The same walk print uses for its prefix.
func (s *State) At() string {
	return s.script.at(s.thread)
}

// SetLocal stores a value under the key on the flow's thread;
// builtins read it back through their thread argument.
func (s *State) SetLocal(key string, value any) {
	s.thread.SetLocal(key, value)
}

// Call invokes name with the name/value pairs as keyword
// arguments, in the given order. A dict value is mutated in place,
// so the caller reads the result from its own reference.
// Errors render the eval error's message alone; a state recording
// positions prefixes the innermost call site.
func (s *State) Call(name string, pairs ...any) (starlark.Value, error) {
	value, ok := s.script.globals[name]
	if !ok {
		return nil, fmt.Errorf("%s: unknown function %q", s.script.name, name)
	}
	fn, ok := value.(starlark.Callable)
	if !ok {
		return nil, fmt.Errorf("%s: %q is not a function", s.script.name, name)
	}
	if len(pairs)%2 != 0 {
		return nil, fmt.Errorf("argument %v has no value", pairs[len(pairs)-1])
	}
	args := make([]starlark.Tuple, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		arg, ok := pairs[i].(string)
		if !ok {
			return nil, fmt.Errorf("argument name %v is not a string", pairs[i])
		}
		val, ok := pairs[i+1].(starlark.Value)
		if !ok {
			return nil, fmt.Errorf("argument %s value is not a starlark value: %T", arg, pairs[i+1])
		}
		args = append(args, starlark.Tuple{starlark.String(arg), val})
	}
	result, err := starlark.Call(s.thread, fn, nil, args)
	if err != nil {
		var evalErr *starlark.EvalError
		if errors.As(err, &evalErr) {
			// Msg is the message alone; the backtrace would only
			// wrap it in "Error[ in <name>]:" formatting.
			cause := evalErr.Msg
			if s.logAt {
				cause = oneLine(s.script.name, s.script.spans, evalErr.Backtrace(), cause)
			}
			return nil, fmt.Errorf("%s%w", cause, wrappedCause{evalErr.Unwrap()})
		}
		return nil, err
	}
	return result, nil
}

// oneLine renders an eval error as one line: the innermost frame's
// remapped position, then the cause. The header, outer frames, and the
// function names drop - the position says where, the config path says
// whose. A backtrace with no frame at all renders as the cause alone.
func oneLine(name string, spans []funcSpan, backtrace, cause string) string {
	frame := innerFrame(backtrace)
	if frame == "" {
		return cause
	}
	frame = cutFunc(remapPos(name, spans, frame))
	if cause == "" {
		return frame
	}
	return frame + ": " + cause
}

// innerFrame returns a backtrace's innermost frame line. The last line
// is the message, the frames precede it, the header first; a failure
// with no stack of its own leaves none.
func innerFrame(backtrace string) string {
	lines := strings.Split(backtrace, "\n")
	for i := len(lines) - 2; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "Traceback") {
			continue
		}
		return line
	}
	return ""
}

// cutFunc drops a frame's function reference, the ": in name"
// suffix, keeping the position alone.
func cutFunc(frame string) string {
	if i := strings.LastIndex(frame, ": in "); i >= 0 {
		return frame[:i]
	}
	return frame
}

// wrappedCause carries an eval error's cause through the remapped
// frame so errors.As finds it, while adding nothing to the message:
// the rendered line already ends with the cause's own text.
type wrappedCause struct{ cause error }

func (w wrappedCause) Error() string { return "" }
func (w wrappedCause) Unwrap() error { return w.cause }
