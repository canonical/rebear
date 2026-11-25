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

// Package snippets wraps the scripts package for the proxy:
// one program holds every inlet's and outlet's snippets, so a
// request can reroute between outlets while it is filtered.
// A config with no snippets compiles to a nil Program, whose
// methods are all no-ops.
package snippets

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/canonical/rebear/internal/logging"
	"github.com/canonical/rebear/internal/notes"
	"github.com/canonical/rebear/internal/scripts"
	"github.com/canonical/rebear/internal/setup"
	"go.starlark.net/starlark"
)

// snippetFunc is one sniplet's one snippet: the assembled fnc, name and
// args included. A type of its own - not the bare Func - so the streaming
// phase can grow it: one incoming- key will then hold more than one func.
type snippetFunc struct {
	fnc scripts.Func
}

// Snippet argument lists: every function takes the flow and
// the two message values - incoming reads None before the
// response arrives, outgoing is seeded before the opening runs
// - then the stage's own argument, when it has one.
var (
	flowArgs    = []string{"flow", "outgoing", "incoming"}
	closingArgs = []string{"flow", "outgoing", "incoming", "notes"}
	headerArgs  = []string{"flow", "outgoing", "incoming", "headers"}
	bodyArgs    = []string{"flow", "outgoing", "incoming", "body"}
	eventArgs   = []string{"flow", "outgoing", "incoming", "event"}
)

// Program is the compiled snippets of the whole setup.
type Program struct {
	script *scripts.Script
	// funcs are the assembled defs, handed to scripts.New.
	funcs []scripts.Func
	// snippets maps each sniplet's fields to their assembled funcs: the
	// proxy's hooks look their target up here, never rebuilding names.
	snippets map[setup.Sniplet]map[string]snippetFunc
	// outlets resolves outlet names for set_outlet. Snippetless outlets
	// sit here, not in the snippet table.
	outlets map[string]*setup.Outlet
	// midlets resolves midlet names for add_midlet. Snippetless
	// midlets sit here too.
	midlets map[string]*setup.Midlet
	// logAt records snippet call positions in the requests' log
	// objects and error messages; stamped onto every flow
	// at birth.
	logAt bool
	// notes is the global notes store the closing-flow snippets
	// see as their notes argument. Nil is a store of its own,
	// thrown away with each flow: only the proxy wires the real
	// one, and only the closings reach it.
	notes *notes.Store
}

// SetLogAt turns the recording of snippet call positions on or off
// for the flows this program starts; off by default.
func (p *Program) SetLogAt(on bool) {
	if p != nil {
		p.logAt = on
	}
}

// SetNotes hands the program the global notes store, shared by
// every flow and surviving reloads.
func (p *Program) SetNotes(store *notes.Store) {
	if p != nil {
		p.notes = store
	}
}

// Compile assembles every inlet's and outlet's snippets into one
// program. The opened secrets, when any, join the build as one
// frozen predeclared dict: a snippet reads `secrets["name"]`;
// with none, a snippet naming it fails to compile. A config with
// no snippets returns a nil Program, whose methods are no-ops.
// A bad inlet or outlet name fails here, where names become
// function names. A snippet that fails to compile fails here
// too, with its config path and the position in its own text.
// Print output goes to the package logger.
func Compile(cfg *setup.Setup, secrets map[string]string) (*Program, error) {
	for _, in := range cfg.Inlets {
		if err := setup.CheckName(in.Name); err != nil {
			return nil, fmt.Errorf("inlet %v", err)
		}
	}
	for _, midlet := range cfg.Midlets {
		if err := setup.CheckName(midlet.Name); err != nil {
			return nil, fmt.Errorf("midlet %v", err)
		}
	}
	for _, out := range cfg.Outlets {
		if err := setup.CheckName(out.Name); err != nil {
			return nil, fmt.Errorf("outlet %v", err)
		}
	}
	p := &Program{snippets: map[setup.Sniplet]map[string]snippetFunc{}}
	addField := func(s setup.Sniplet, field, snippet string, args []string) {
		if snippet == "" {
			return
		}
		fnc := scripts.Func{
			Name: funcName(s, field),
			Args: args,
			Body: snippet,
			At:   s.Kindlet() + "s." + s.Namelet() + "." + field,
		}
		p.funcs = append(p.funcs, fnc)
		if p.snippets[s] == nil {
			p.snippets[s] = map[string]snippetFunc{}
		}
		p.snippets[s][field] = snippetFunc{fnc: fnc}
	}
	for _, inlet := range cfg.Inlets {
		addField(inlet, "opening-flow", inlet.OpeningFlow, flowArgs)
		addField(inlet, "closing-flow", inlet.ClosingFlow, closingArgs)
		addField(inlet, "outgoing-headers", inlet.OutgoingHeaders, headerArgs)
		addField(inlet, "incoming-headers", inlet.IncomingHeaders, headerArgs)
		addField(inlet, "incoming-event", inlet.IncomingEvent, eventArgs)
		snippet, err := bodySnippet(inlet, "outgoing-body", inlet.OutgoingBody)
		if err != nil {
			return nil, err
		}
		addField(inlet, "outgoing-body", snippet, bodyArgs)
		snippet, err = bodySnippet(inlet, "incoming-body", inlet.IncomingBody)
		if err != nil {
			return nil, err
		}
		addField(inlet, "incoming-body", snippet, bodyArgs)
	}
	for _, midlet := range cfg.Midlets {
		addField(midlet, "opening-flow", midlet.OpeningFlow, flowArgs)
		addField(midlet, "closing-flow", midlet.ClosingFlow, closingArgs)
		addField(midlet, "outgoing-headers", midlet.OutgoingHeaders, headerArgs)
		addField(midlet, "incoming-headers", midlet.IncomingHeaders, headerArgs)
		addField(midlet, "incoming-event", midlet.IncomingEvent, eventArgs)
		snippet, err := bodySnippet(midlet, "outgoing-body", midlet.OutgoingBody)
		if err != nil {
			return nil, err
		}
		addField(midlet, "outgoing-body", snippet, bodyArgs)
		snippet, err = bodySnippet(midlet, "incoming-body", midlet.IncomingBody)
		if err != nil {
			return nil, err
		}
		addField(midlet, "incoming-body", snippet, bodyArgs)
	}
	for _, outlet := range cfg.Outlets {
		addField(outlet, "opening-flow", outlet.OpeningFlow, flowArgs)
		addField(outlet, "closing-flow", outlet.ClosingFlow, closingArgs)
		addField(outlet, "outgoing-headers", outlet.OutgoingHeaders, headerArgs)
		addField(outlet, "incoming-headers", outlet.IncomingHeaders, headerArgs)
		addField(outlet, "incoming-event", outlet.IncomingEvent, eventArgs)
		snippet, err := bodySnippet(outlet, "outgoing-body", outlet.OutgoingBody)
		if err != nil {
			return nil, err
		}
		addField(outlet, "outgoing-body", snippet, bodyArgs)
		snippet, err = bodySnippet(outlet, "incoming-body", outlet.IncomingBody)
		if err != nil {
			return nil, err
		}
		addField(outlet, "incoming-body", snippet, bodyArgs)
	}
	if len(p.funcs) == 0 {
		return nil, nil
	}
	predeclared := builtins()
	if len(secrets) > 0 {
		predeclared["secrets"] = secretsDict(secrets)
	}
	script, err := scripts.New(&scripts.Options{
		// Print output is the author's text: never a format string.
		// It lands on the request's log entry as one object, the
		// same shape log() produces. Without a flow behind the
		// call there is no entry to ride, and the message becomes
		// a bare log line instead.
		Print: func(t *starlark.Thread, at, msg string) {
			flow, _ := t.Local("flow").(*Flow)
			if flow == nil || flow.entry == nil {
				logging.Log(msg)
				return
			}
			object := map[string]any{"print": msg}
			if at != "" {
				object["at"] = atLine(at)
			}
			flow.entry.Log(object)
		},
		Funcs:    p.funcs,
		Builtins: predeclared,
	})
	if err != nil {
		return nil, err
	}
	p.script = script
	p.outlets = make(map[string]*setup.Outlet, len(cfg.Outlets))
	for _, out := range cfg.Outlets {
		p.outlets[out.Name] = out
	}
	p.midlets = make(map[string]*setup.Midlet, len(cfg.Midlets))
	for _, midlet := range cfg.Midlets {
		p.midlets[midlet.Name] = midlet
	}
	return p, nil
}

// funcName is the assembled def's name: the sniplet's name with - as _,
// then its kind, then the field with - as _. Compile-time only; the
// runtime reads the name from the snippet table.
func funcName(s setup.Sniplet, field string) string {
	return strings.ReplaceAll(s.Namelet(), "-", "_") + "_" + s.Kindlet() + "_" + strings.ReplaceAll(field, "-", "_")
}

// BodySnippet pulls the whole-body snippet out of a body filter, failing
// on the key-binding form: streaming is a later phase, and holding it
// silently would misroute bodies.
func bodySnippet(s setup.Sniplet, filter string, jf *setup.BodyFilter) (string, error) {
	if jf == nil {
		return "", nil
	}
	if len(jf.Keys) > 0 {
		return "", fmt.Errorf("%ss.%s.%s: key bindings are not implemented yet", s.Kindlet(), s.Namelet(), filter)
	}
	return jf.Body, nil
}

// secretsDict spells the opened secrets as one frozen dict, the
// predeclared `secrets` every snippet reads. Handoff carries the
// freeze the later globals freeze gives, so no snippet hands a
// secret to another flow by mutation. Names are inserted sorted,
// keeping the assembly deterministic.
func secretsDict(secrets map[string]string) starlark.Value {
	dict := starlark.NewDict(len(secrets))
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		_ = dict.SetKey(starlark.String(name), starlark.String(secrets[name]))
	}
	dict.Freeze()
	return dict
}

// NewFlow creates one request's flow on the inlet, nothing
// run. The request facts seed onto it right after, so the
// opening already sees them. A nil Program gives a bare flow
// that models the request and never runs a snippet.
func (p *Program) NewFlow(inlet *setup.Inlet, entry *logging.Entry) *Flow {
	if p == nil {
		return &Flow{inlet: inlet, entry: entry}
	}
	flow := &Flow{
		inlet:       inlet,
		scriptState: p.script.Init(),
		flowState:   starlark.NewDict(0),
		entry:       entry,
	}
	flow.SetLogAt(p.logAt)
	return flow
}

// OpeningFlow settles one sniplet on the flow and runs its
// opening-flow snippet. A nil flow starts one, on an inlet only;
// the entry is the request's log entry. A sniplet joins the flow's
// opened roster once its opening ran without failure. A nil
// Program runs nothing, but the inlet's opening still starts the
// outgoing half - the lifecycle moves with the stage calls, not
// with the snippets.
func (p *Program) OpeningFlow(flow *Flow, s setup.Sniplet, entry *logging.Entry) (*Flow, error) {
	if flow == nil {
		inlet, ok := s.(*setup.Inlet)
		if !ok {
			return nil, fmt.Errorf("cannot start a flow on %q", s.Kindlet())
		}
		flow = p.NewFlow(inlet, entry)
	}
	if _, ok := s.(*setup.Inlet); ok {
		if flow.stage != openingStage {
			return flow, fmt.Errorf("flow already started on inlet %q", flow.inlet.Name)
		}
		flow.stage = outgoingStage
	}
	if midlet, ok := s.(*setup.Midlet); ok {
		if slices.Contains(flow.midletsDone, midlet.Name) {
			return flow, fmt.Errorf("cannot use midlet %q twice", midlet.Name)
		}
		flow.midletsDone = append(flow.midletsDone, midlet.Name)
	}
	if p == nil {
		return flow, nil
	}
	switch v := s.(type) {
	case *setup.Midlet:
		if v.Outlet != "" {
			outlet, err := p.Outlet(v.Outlet)
			if err != nil {
				return flow, err
			}
			if err := flow.SetOutlet(outlet); err != nil {
				return flow, err
			}
		}
	case *setup.Outlet:
		if err := flow.SetOutlet(v); err != nil {
			return flow, err
		}
		flow.LockOutlet()
	}
	if _, err := p.call(flow, s, "opening-flow"); err != nil {
		return flow, err
	}
	flow.opened = append(flow.opened, s)
	return flow, nil
}

// ClosingFlow runs the closing-flow snippet of every sniplet the
// flow opened, in reverse opening order. It runs once the flow
// has finished, so a closing failure only lands on the request's
// log entry and the remaining closings still run. A second call
// is a no-op: the flow is closed. A nil Program closes the flow
// all the same - the lifecycle moves with the stage calls, not
// with the snippets.
func (p *Program) ClosingFlow(flow *Flow) {
	if flow == nil || flow.stage != closingStage {
		return
	}
	flow.stage = closedStage
	if p == nil {
		return
	}
	for i := len(flow.opened) - 1; i >= 0; i-- {
		// One notes value per closing: the lock, when the snippet
		// takes it, opens at the snippet's end - the fold-back and
		// its error landing on this flow's log entry.
		notes := &notesValue{store: p.notes, flow: flow}
		_, err := p.call(flow, flow.opened[i], "closing-flow", "notes", notes)
		notes.release()
		if err != nil && flow.entry != nil {
			flow.entry.Log(map[string]any{"error": err.Error()})
		}
	}
}

// OutgoingHeaders runs the sniplet's request header filter on
// the flow's live request headers. A nil Program or Flow, or a
// sniplet without the snippet, leaves them untouched.
func (p *Program) OutgoingHeaders(flow *Flow, sniplet setup.Sniplet) error {
	if flow == nil {
		return nil
	}
	return p.headerFilter(flow, sniplet, "outgoing-headers", flow.outHeaders)
}

// IncomingHeaders runs the sniplet's response header filter on
// the flow's live response headers.
func (p *Program) IncomingHeaders(flow *Flow, sniplet setup.Sniplet) error {
	if flow == nil {
		return nil
	}
	return p.headerFilter(flow, sniplet, "incoming-headers", flow.inHeaders)
}

// OutgoingBody runs the sniplet's request body filter on the shared
// value. A nil Program or Flow, or a sniplet without the snippet,
// leaves the body untouched.
func (p *Program) OutgoingBody(flow *Flow, sniplet setup.Sniplet, body *JSONValue) error {
	return p.filterJSON(flow, sniplet, "outgoing-body", "body", body, nil)
}

// IncomingBody runs the sniplet's response body filter on the shared
// value.
func (p *Program) IncomingBody(flow *Flow, sniplet setup.Sniplet, body *JSONValue) error {
	return p.filterJSON(flow, sniplet, "incoming-body", "body", body, nil)
}

// IncomingEvent runs the sniplet's response event filter on one
// streamed event's value, the event's fields riding the value.
func (p *Program) IncomingEvent(flow *Flow, sniplet setup.Sniplet, body *JSONValue, event *StreamEvent) error {
	return p.filterJSON(flow, sniplet, "incoming-event", "event", body, event)
}

// FiltersOutgoingBody reports whether the sniplet filters outgoing
// bodies.
func (p *Program) FiltersOutgoingBody(sniplet setup.Sniplet) bool {
	return p != nil && p.has(sniplet, "outgoing-body")
}

// FiltersIncomingBody reports whether the sniplet filters incoming
// bodies.
func (p *Program) FiltersIncomingBody(sniplet setup.Sniplet) bool {
	return p != nil && p.has(sniplet, "incoming-body")
}

// FiltersIncomingEvent reports whether the sniplet filters incoming
// events.
func (p *Program) FiltersIncomingEvent(sniplet setup.Sniplet) bool {
	return p != nil && p.has(sniplet, "incoming-event")
}

// has reports whether the sniplet carries the field. A nil program has no
// snippets at all.
func (p *Program) has(sniplet setup.Sniplet, field string) bool {
	if p == nil {
		return false
	}
	_, ok := p.snippets[sniplet][field]
	return ok
}

// call runs the sniplet's field on the flow, when present, with the
// flow value plus the name/value pairs as keyword arguments. The
// flow sits on the thread for the call's duration: builtins read
// it back through their thread argument. The return value is the
// caller's to use or ignore.
func (p *Program) call(flow *Flow, sniplet setup.Sniplet, field string, pairs ...any) (starlark.Value, error) {
	fnc, ok := p.snippets[sniplet][field]
	if !ok {
		return nil, nil
	}
	flow.scriptState.SetLocal("flow", flow)
	pairs = append(pairs,
		"flow", &flowValue{program: p, flow: flow},
		"outgoing", &outgoingValue{flow: flow},
		"incoming", &incomingValue{flow: flow},
	)
	return flow.scriptState.Call(fnc.fnc.Name, pairs...)
}

// Outlet resolves an outlet name. Snippetless outlets sit in this
// registry, not in the snippet table.
func (p *Program) Outlet(name string) (*setup.Outlet, error) {
	if p == nil {
		return nil, fmt.Errorf("unknown outlet %q", name)
	}
	outlet, ok := p.outlets[name]
	if !ok {
		return nil, fmt.Errorf("unknown outlet %q", name)
	}
	return outlet, nil
}

// Midlet resolves a midlet name. Snippetless midlets sit in this
// registry, not in the snippet table.
func (p *Program) Midlet(name string) (*setup.Midlet, error) {
	if p == nil {
		return nil, fmt.Errorf("unknown midlet %q", name)
	}
	midlet, ok := p.midlets[name]
	if !ok {
		return nil, fmt.Errorf("unknown midlet %q", name)
	}
	return midlet, nil
}

// headerFilter runs the sniplet's header snippet on the flow's
// live header dict - the same dict flow.i/o.headers exposes -
// so a mutation is visible everywhere at once.
func (p *Program) headerFilter(flow *Flow, sniplet setup.Sniplet, field string, headers *starlark.Dict) error {
	if headers == nil || !p.has(sniplet, field) {
		return nil
	}
	_, err := p.call(flow, sniplet, field, "headers", headers)
	return err
}
