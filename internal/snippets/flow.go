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

package snippets

import (
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"slices"

	"github.com/canonical/rebear/internal/logging"
	"github.com/canonical/rebear/internal/scripts"
	"github.com/canonical/rebear/internal/setup"
	"github.com/canonical/rebear/internal/snapshots"
	"go.starlark.net/starlark"
)

// stage is one flow's position in its lifecycle: opening at
// birth, outgoing once the inlet's opening-flow ran, incoming
// once the request left, closing once the response reached the
// client, closed once the closings ran. The opening snippet
// runs inside outgoing, the closing ones inside closed, and
// each step freezes what it ends: the stages are the locking.
type stage int

const (
	openingStage stage = iota
	outgoingStage
	incomingStage
	closingStage
	closedStage
)

// Flow is one request's snippet flow: the script and flow state,
// the state dict the snippets share, the routing state, and the request's
// log entry.
type Flow struct {
	// inlet is the inlet that started the flow.
	inlet *setup.Inlet

	scriptState *scripts.State
	flowState   *starlark.Dict

	// entry is the request's log entry: log() calls land on it.
	entry *logging.Entry
	// logAt records the call positions in the entry's log objects
	// and error messages; off by default.
	logAt bool

	// outlet is set_outlet's record, or the settled outlet once its opening
	// ran. Nil until one of those happens.
	outlet *setup.Outlet
	// outletLocked marks that the choice has closed: further changes fail,
	// to the outlet and to the midlet chain alike.
	outletLocked bool
	// midletsPending are the midlets queued to run: the config
	// lists of the inlet and of each settled midlet, then the
	// add_midlet records, in arrival order.
	midletsPending []*setup.Midlet
	// midletsDone are the midlets already reached on this flow:
	// each midlet runs once, later listings are skipped, and an
	// add_midlet naming one of them is a no-op.
	midletsDone []string
	// opened are the sniplets whose opening-flow ran without
	// failure, in opening order. ClosingFlow walks it in reverse
	// once the whole flow has finished.
	opened []setup.Sniplet

	// stage is the flow's lifecycle step; it only advances.
	stage stage

	// outMethod, outPath, and outHeaders model the request. The
	// dict is the one the headers stages and outgoing share: a
	// mutation is visible everywhere at once. pathTouched marks a
	// snippet-assigned path - the outlet prefixing leaves it. A
	// nil dict marks a request not known yet.
	outMethod   string
	outPath     string
	pathTouched bool
	outHeaders  *starlark.Dict

	// rawQuery is the client's query, verbatim; outQuery is its
	// parsed form, built on first use. updated marks a mutation:
	// until then the verbatim form keeps riding the wire, so an
	// awkward construction survives the proxy untouched.
	rawQuery string
	outQuery url.Values
	updated  bool

	// inCode and inHeaders model the response, seeded when it
	// arrives. A nil dict marks a response not known yet.
	inCode    int
	inHeaders *starlark.Dict

	// outSnapshots and inSnapshots hold the snapshot names
	// requested on each half, deduplicated, in first-request order.
	outSnapshots []string
	inSnapshots  []string
}

// SetLogAt turns the recording of call positions in the entry's log
// objects and error messages on or off; off by default. The script
// state is stamped too, so the two cannot diverge.
func (s *Flow) SetLogAt(on bool) {
	s.logAt = on
	s.scriptState.SetLogAt(on)
}

// SetOutlet records the outlet the request will leave through. Once
// locked, the change fails.
func (s *Flow) SetOutlet(outlet *setup.Outlet) error {
	if s.outletLocked {
		return fmt.Errorf("set_outlet: too late to redefine the outlet")
	}
	s.outlet = outlet
	return nil
}

// LockOutlet closes the outlet choice.
func (s *Flow) LockOutlet() {
	s.outletLocked = true
}

// AddMidlet appends a midlet to the pending queue. Once the
// outlet choice has closed the change fails; a midlet already
// reached is not added - it is in the past.
func (s *Flow) AddMidlet(midlet *setup.Midlet) error {
	if s.outletLocked {
		return fmt.Errorf("add_midlet: too late to add a midlet")
	}
	if slices.Contains(s.midletsDone, midlet.Name) {
		return nil
	}
	s.midletsPending = append(s.midletsPending, midlet)
	return nil
}

// PopNextMidlet pops the next midlet to run, skipping any already
// settled: the first listing of a midlet is its only run. Nil
// ends the chain.
func (s *Flow) PopNextMidlet() *setup.Midlet {
	if s == nil {
		return nil
	}
	for len(s.midletsPending) > 0 {
		next := s.midletsPending[0]
		s.midletsPending = s.midletsPending[1:]
		if !slices.Contains(s.midletsDone, next.Name) {
			return next
		}
	}
	return nil
}

// Outlet reports the outlet the request will leave through, or nil while
// the record is empty.
func (s *Flow) Outlet() *setup.Outlet {
	if s == nil {
		return nil
	}
	return s.outlet
}

// SetOut seeds the outgoing request: the client's method, the
// mount-cut path, the raw query, and the request headers.
// Multi-valued headers collapse to their first value, the
// starlark form.
func (s *Flow) SetOut(method, path, rawQuery string, headers http.Header) {
	if s == nil {
		return
	}
	s.outMethod = method
	s.outPath = path
	s.rawQuery = rawQuery
	s.outQuery = nil
	s.updated = false
	s.outHeaders = headerDict(headers)
}

// SetOutPath replaces the outgoing path - the trailing, with no
// outlet prefix on it: the prefix joins at the send, from the
// settled outlet. The touched mark lands only on a real change,
// gating the escaped-passthrough.
func (s *Flow) SetOutPath(path string) {
	if s == nil || path == s.outPath {
		return
	}
	s.outPath = path
	s.pathTouched = true
}

// PathTouched reports whether the outgoing path was assigned,
// rather than only seeded.
func (s *Flow) PathTouched() bool {
	return s != nil && s.pathTouched
}

// OutMethod reports the method the request leaves with.
func (s *Flow) OutMethod() string {
	if s == nil {
		return ""
	}
	return s.outMethod
}

// OutPath reports the path as it is currently best known.
func (s *Flow) OutPath() string {
	if s == nil {
		return ""
	}
	return s.outPath
}

// OutQuery answers the query's parsed form, parsing the raw one
// on first use only. Reading never rewrites the wire form.
func (s *Flow) OutQuery() url.Values {
	if s == nil {
		return nil
	}
	if s.outQuery == nil {
		// A bad escape parses to what it could: the query is
		// the client's, and reading must not fail the flow.
		s.outQuery, _ = url.ParseQuery(s.rawQuery)
	}
	return s.outQuery
}

// QueryUpdated reports whether the query was mutated, rather
// than only read. Only then does the outgoing URL re-encode it.
func (s *Flow) QueryUpdated() bool {
	return s != nil && s.updated
}

// OutHeaders renders the shared outgoing dict back to header
// form, for the delivery.
func (s *Flow) OutHeaders() http.Header {
	if s == nil || s.outHeaders == nil {
		return http.Header{}
	}
	return dictHeaders(s.outHeaders)
}

// LockOut ends the outgoing half: the request leaves, the
// incoming half begins, and from here the outgoing fields
// refuse assignment. Only advances.
func (s *Flow) LockOut() {
	if s == nil || s.stage != outgoingStage {
		return
	}
	s.stage = incomingStage
	if s.outHeaders != nil {
		s.outHeaders.Freeze()
	}
}

// SetIn seeds the incoming response: the outlet's status and
// headers, recordable once the request has left.
func (s *Flow) SetIn(code int, headers http.Header) error {
	if s == nil {
		return nil
	}
	if s.stage != incomingStage {
		return fmt.Errorf("cannot record the response before the request leaves")
	}
	s.inCode = code
	s.inHeaders = headerDict(headers)
	return nil
}

// InCode reports the status the response carries now.
func (s *Flow) InCode() int {
	if s == nil {
		return 0
	}
	return s.inCode
}

// InHeaders renders the shared incoming dict back to header
// form, for the delivery.
func (s *Flow) InHeaders() http.Header {
	if s == nil || s.inHeaders == nil {
		return http.Header{}
	}
	return dictHeaders(s.inHeaders)
}

// LockIn ends the incoming half by beginning the closing: the
// response reached the client, the incoming dict freezes, and
// from here the incoming fields refuse assignment. Only
// advances.
func (s *Flow) LockIn() {
	if s == nil || s.stage != incomingStage {
		return
	}
	s.stage = closingStage
	if s.inHeaders != nil {
		s.inHeaders.Freeze()
	}
}

// flowValue is the starlark flow argument: the shared state dict,
// and set_outlet for rerouting. The glue composes the sides: the program
// resolves the name, the flow records the choice.
type flowValue struct {
	program *Program
	flow    *Flow
}

// SnapshotOut records a snapshot name for the outgoing payload,
// first request winning: the name joins once.
func (s *Flow) SnapshotOut(name string) {
	if s == nil || slices.Contains(s.outSnapshots, name) {
		return
	}
	s.outSnapshots = append(s.outSnapshots, name)
}

// SnapshotIn records a snapshot name for the incoming payload,
// first request winning.
func (s *Flow) SnapshotIn(name string) {
	if s == nil || slices.Contains(s.inSnapshots, name) {
		return
	}
	s.inSnapshots = append(s.inSnapshots, name)
}

// OutSnapshots lists the names requested on the outgoing half.
func (s *Flow) OutSnapshots() []string {
	if s == nil {
		return nil
	}
	return s.outSnapshots
}

// InSnapshots lists the names requested on the incoming half.
func (s *Flow) InSnapshots() []string {
	if s == nil {
		return nil
	}
	return s.inSnapshots
}

// SnapshotNames lists every distinct snapshot name across both halves,
// first-seen order - the log file writes one per name.
func (s *Flow) SnapshotNames() []string {
	if s == nil {
		return nil
	}
	names := append([]string{}, s.outSnapshots...)
	for _, name := range s.inSnapshots {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

func (v *flowValue) String() string       { return "flow" }
func (v *flowValue) Type() string         { return "flow" }
func (v *flowValue) Freeze()              {}
func (v *flowValue) Truth() starlark.Bool { return starlark.True }
func (v *flowValue) Hash() (uint32, error) {
	return 0, fmt.Errorf("unhashable type: flow")
}

// Attr returns the flow's fields.
func (v *flowValue) Attr(name string) (starlark.Value, error) {
	switch name {
	case "state":
		return v.flow.flowState, nil
	case "set_outlet":
		return starlark.NewBuiltin("set_outlet", func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var name string
			if err := starlark.UnpackPositionalArgs("set_outlet", args, kwargs, 1, &name); err != nil {
				return nil, err
			}
			outlet, err := v.program.Outlet(name)
			if err != nil {
				return nil, fmt.Errorf("set_outlet: %w", err)
			}
			return starlark.None, v.flow.SetOutlet(outlet)
		}), nil
	case "add_midlet":
		return starlark.NewBuiltin("add_midlet", func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var name string
			if err := starlark.UnpackPositionalArgs("add_midlet", args, kwargs, 1, &name); err != nil {
				return nil, err
			}
			midlet, err := v.program.Midlet(name)
			if err != nil {
				return nil, fmt.Errorf("add_midlet: %w", err)
			}
			return starlark.None, v.flow.AddMidlet(midlet)
		}), nil
	}
	return nil, nil
}

// AttrNames lists the flow's fields.
func (v *flowValue) AttrNames() []string {
	return []string{"add_midlet", "set_outlet", "state"}
}

// outgoingValue is the flow's outgoing half, handed to every
// snippet as its outgoing argument: the request as it stands.
// Its fields assign until the request leaves; the headers dict
// is the live one the headers stages share.
type outgoingValue struct {
	flow *Flow
}

func (v *outgoingValue) String() string       { return "outgoing" }
func (v *outgoingValue) Type() string         { return "outgoing" }
func (v *outgoingValue) Freeze()              {}
func (v *outgoingValue) Truth() starlark.Bool { return starlark.True }
func (v *outgoingValue) Hash() (uint32, error) {
	return 0, fmt.Errorf("unhashable type: outgoing")
}

func (v *outgoingValue) AttrNames() []string {
	return []string{"headers", "method", "path", "query", "snapshot", "type"}
}

// snapshot records the name for a payload dump, teed when the
// request leaves. Legal from any snippet until then: the payload
// need not be known yet, only not yet gone.
func (v *outgoingValue) snapshot(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name string
	if err := starlark.UnpackPositionalArgs("snapshot", args, kwargs, 1, &name); err != nil {
		return nil, err
	}
	if err := snapshots.CheckName(name); err != nil {
		return nil, err
	}
	f := v.flow
	if f.stage != outgoingStage {
		return nil, fmt.Errorf("cannot snapshot outgoing: request already sent")
	}
	f.SnapshotOut(name)
	return starlark.None, nil
}

// Attr reads the request's current facts, None for every field
// before the seeding. The type is the cleaned Content-Type: the
// media type alone, parameters dropped, "" when unset or unparseable.
func (v *outgoingValue) Attr(name string) (starlark.Value, error) {
	f := v.flow
	switch name {
	case "method":
		if f.outHeaders == nil {
			return starlark.None, nil
		}
		return starlark.String(f.outMethod), nil
	case "path":
		if f.outHeaders == nil {
			return starlark.None, nil
		}
		return starlark.String(f.outPath), nil
	case "type":
		if f.outHeaders == nil {
			return starlark.None, nil
		}
		return starlark.String(cleanMediaType(dictContentType(f.outHeaders))), nil
	case "headers":
		if f.outHeaders == nil {
			return starlark.None, nil
		}
		return f.outHeaders, nil
	case "query":
		if f.outHeaders == nil {
			return starlark.None, nil
		}
		return &queryValue{flow: f}, nil
	case "snapshot":
		return starlark.NewBuiltin("snapshot", v.snapshot), nil
	}
	return nil, nil
}

// SetField lands the assignment forms. Everything assigns until
// the request leaves - then the message is gone, and every field
// refuses. The headers dict never replaces: mutate it in place.
func (v *outgoingValue) SetField(name string, val starlark.Value) error {
	f := v.flow
	if f.stage != outgoingStage {
		return fmt.Errorf("cannot set outgoing.%s: request already sent", name)
	}
	if f.outHeaders == nil {
		return fmt.Errorf("cannot set outgoing.%s: no request", name)
	}
	switch name {
	case "method":
		method, ok := starlark.AsString(val)
		if !ok {
			return fmt.Errorf("cannot set outgoing.method: got %s, want string", val.Type())
		}
		f.outMethod = method
		return nil
	case "path":
		path, ok := starlark.AsString(val)
		if !ok {
			return fmt.Errorf("cannot set outgoing.path: got %s, want string", val.Type())
		}
		f.SetOutPath(path)
		return nil
	case "type":
		contentType, ok := starlark.AsString(val)
		if !ok {
			return fmt.Errorf("cannot set outgoing.type: got %s, want string", val.Type())
		}
		return f.outHeaders.SetKey(starlark.String("Content-Type"), starlark.String(contentType))
	case "headers":
		return fmt.Errorf("cannot set outgoing.headers: mutate the dict in place")
	}
	return fmt.Errorf("outgoing has no .%s field or method", name)
}

// incomingValue is the flow's incoming half, handed to every
// snippet as its incoming argument: the response as it currently
// stands, from its arrival to the client write-back.
type incomingValue struct {
	flow *Flow
}

func (v *incomingValue) String() string       { return "incoming" }
func (v *incomingValue) Type() string         { return "incoming" }
func (v *incomingValue) Freeze()              {}
func (v *incomingValue) Truth() starlark.Bool { return starlark.True }
func (v *incomingValue) Hash() (uint32, error) {
	return 0, fmt.Errorf("unhashable type: incoming")
}

func (v *incomingValue) AttrNames() []string {
	return []string{"code", "headers", "snapshot", "type"}
}

// snapshot records the name for a payload dump, teed when the
// response is written back. Legal from any snippet until then -
// before the response arrives included.
func (v *incomingValue) snapshot(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name string
	if err := starlark.UnpackPositionalArgs("snapshot", args, kwargs, 1, &name); err != nil {
		return nil, err
	}
	if err := snapshots.CheckName(name); err != nil {
		return nil, err
	}
	f := v.flow
	if f.stage >= closingStage {
		return nil, fmt.Errorf("cannot snapshot incoming: response already sent")
	}
	f.SnapshotIn(name)
	return starlark.None, nil
}

// Attr reads the response's current facts, None for every field
// before the response arrives.
func (v *incomingValue) Attr(name string) (starlark.Value, error) {
	f := v.flow
	switch name {
	case "code":
		if f.inHeaders == nil {
			return starlark.None, nil
		}
		return starlark.MakeInt(f.inCode), nil
	case "type":
		if f.inHeaders == nil {
			return starlark.None, nil
		}
		return starlark.String(cleanMediaType(dictContentType(f.inHeaders))), nil
	case "headers":
		if f.inHeaders == nil {
			return starlark.None, nil
		}
		return f.inHeaders, nil
	case "snapshot":
		return starlark.NewBuiltin("snapshot", v.snapshot), nil
	}
	return nil, nil
}

// SetField lands the assignment forms, the outgoing half's
// mirror: the code and the content type assign from the arrival
// to the write-back, the headers dict never replaces.
func (v *incomingValue) SetField(name string, val starlark.Value) error {
	f := v.flow
	if f.stage >= closingStage {
		return fmt.Errorf("cannot set incoming.%s: response already sent", name)
	}
	if f.inHeaders == nil {
		return fmt.Errorf("cannot set incoming.%s: no response", name)
	}
	switch name {
	case "code":
		code, err := starlark.AsInt32(val)
		if err != nil {
			return fmt.Errorf("cannot set incoming.code: got %s, want int", val.Type())
		}
		f.inCode = code
		return nil
	case "type":
		contentType, ok := starlark.AsString(val)
		if !ok {
			return fmt.Errorf("cannot set incoming.type: got %s, want string", val.Type())
		}
		return f.inHeaders.SetKey(starlark.String("Content-Type"), starlark.String(contentType))
	case "headers":
		return fmt.Errorf("cannot set incoming.headers: mutate the dict in place")
	}
	return fmt.Errorf("incoming has no .%s field or method", name)
}

// cleanMediaType parses a Content-Type value down to its media
// type - lowercase, parameters dropped - or "" when unset or
// unparseable.
func cleanMediaType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return mediaType
}

// dictContentType reads a header dict's Content-Type, "" when
// absent.
func dictContentType(headers *starlark.Dict) string {
	value, found, err := headers.Get(starlark.String("Content-Type"))
	if !found || err != nil {
		return ""
	}
	contentType, ok := starlark.AsString(value)
	if !ok {
		return ""
	}
	return contentType
}

// headerDict converts headers into their starlark form: one
// value per name, the first - the form the headers stages and
// flow.i/o share.
func headerDict(headers http.Header) *starlark.Dict {
	dict := starlark.NewDict(len(headers))
	for name, values := range headers {
		if len(values) == 0 {
			continue
		}
		_ = dict.SetKey(starlark.String(name), starlark.String(values[0]))
	}
	return dict
}

// dictHeaders renders a header dict back to header form, the
// delivery shape.
func dictHeaders(dict *starlark.Dict) http.Header {
	headers := make(http.Header, dict.Len())
	for _, key := range dict.Keys() {
		name, ok := starlark.AsString(key)
		if !ok {
			continue
		}
		value, _, err := dict.Get(key)
		if err != nil {
			continue
		}
		content, ok := starlark.AsString(value)
		if !ok {
			continue
		}
		headers[name] = []string{content}
	}
	return headers
}
