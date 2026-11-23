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

// The builtins handed to every snippet: fail and log.
package snippets

import (
	"fmt"
	"strings"

	"github.com/canonical/rebear/internal/plainjson"
	"go.starlark.net/starlark"
)

// FailError is a snippet's deliberate fail() rejection.
type FailError struct {
	// Message is the joined positional arguments of the call.
	Message string
	// Code is the code= argument; 0 when absent.
	Code int
	// Body is the body= argument: a string verbatim, or the
	// JSON encoding of a map.
	Body string
	// BodyJSON reports whether Body came from a map and is
	// JSON, so the HTTP layer can set the content type.
	BodyJSON bool
}

func (e *FailError) Error() string { return e.Message }

// failBuiltin is the fail builtin handed to every snippet, replacing
// the universe's fail. Its message assembles the same way:
// any number of positional values, strings raw and others in
// starlark notation, joined by sep (a space, or the sep=
// argument). Beyond that, code= sets FailError.Code and body=
// sets FailError.Body, a string or a JSON-encoded map.
func failBuiltin(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var sep = " "
	var code starlark.Value
	var body starlark.Value
	if err := starlark.UnpackArgs("fail", nil, kwargs, "sep?", &sep, "code?", &code, "body?", &body); err != nil {
		return nil, err
	}
	msg := new(strings.Builder)
	for i, v := range args {
		if i > 0 {
			msg.WriteString(sep)
		}
		if s, ok := starlark.AsString(v); ok {
			msg.WriteString(s)
		} else {
			msg.WriteString(v.String())
		}
	}
	failErr := &FailError{Message: msg.String()}
	if code != nil && code != starlark.None {
		i, err := starlark.AsInt32(code)
		if err != nil {
			return nil, fmt.Errorf("fail: code must be an int")
		}
		failErr.Code = i
	}
	if d, ok := body.(*starlark.Dict); ok {
		value, err := fromStarlark(d)
		if err != nil {
			return nil, err
		}
		encoded, err := plainjson.Marshal(value)
		if err != nil {
			return nil, err
		}
		failErr.Body = string(encoded)
		failErr.BodyJSON = true
	} else if body != nil {
		s, ok := starlark.AsString(body)
		if !ok {
			return nil, fmt.Errorf("fail: body must be a string or a map")
		}
		failErr.Body = s
	}
	return nil, failErr
}

// atLine cuts the column off a position: the line alone is
// what the log's at field carries.
func atLine(at string) string {
	if i := strings.LastIndex(at, ":"); i >= 0 {
		return at[:i]
	}
	return at
}

// logBuiltin is the log builtin handed to every snippet: it appends
// the given dict to the request's log entry, as one object under
// the record's log list. The call's position joins the keys as at
// only while the flow records positions (SetLogAt).
func logBuiltin(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var dict *starlark.Dict
	if err := starlark.UnpackPositionalArgs("log", args, kwargs, 1, &dict); err != nil {
		return nil, err
	}
	flow, _ := thread.Local("flow").(*Flow)
	if flow == nil || flow.entry == nil {
		return nil, fmt.Errorf("log: no flow to log into")
	}
	values, err := fromStarlark(dict)
	if err != nil {
		return nil, fmt.Errorf("log: %v", err)
	}
	object, ok := values.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("log: argument must be a dict")
	}
	if flow.logAt {
		object["at"] = atLine(flow.scriptState.At())
	}
	flow.entry.Log(object)
	return starlark.None, nil
}

// fromStarlark converts a starlark value into a Go value that
// plainjson.Marshal accepts: ints, floats, bools, strings, lists,
// and dicts with string keys.
func fromStarlark(v starlark.Value) (any, error) {
	switch value := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Int:
		if i, ok := value.Int64(); ok {
			return i, nil
		}
		return nil, fmt.Errorf("cannot convert %s", value.String())
	case starlark.Float:
		return float64(value), nil
	case starlark.Bool:
		return bool(value), nil
	case starlark.String:
		return string(value), nil
	case *starlark.List:
		out := make([]any, 0, value.Len())
		for i := 0; i < value.Len(); i++ {
			item, err := fromStarlark(value.Index(i))
			if err != nil {
				return nil, err
			}
			out = append(out, item)
		}
		return out, nil
	case *starlark.Dict:
		out := make(map[string]any, value.Len())
		for _, item := range value.Items() {
			key, ok := starlark.AsString(item[0])
			if !ok {
				return nil, fmt.Errorf("cannot convert dict key %s", item[0].String())
			}
			converted, err := fromStarlark(item[1])
			if err != nil {
				return nil, err
			}
			out[key] = converted
		}
		return out, nil
	}
	return nil, fmt.Errorf("cannot convert %s", v.String())
}

// builtins are handed to every compiled pair.
func builtins() starlark.StringDict {
	return starlark.StringDict{
		"fail": starlark.NewBuiltin("fail", failBuiltin),
		"log":  starlark.NewBuiltin("log", logBuiltin),
	}
}
