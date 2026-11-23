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
	"errors"
	"fmt"
	"strconv"

	"github.com/canonical/rebear/internal/scripts"
	"github.com/canonical/rebear/internal/setup"
	"go.starlark.net/starlark"
)

// ErrValueType reports a body with no dict form: not JSON, or JSON
// whose top level is not an object. Reported, never stored: the body
// passes through untouched.
var ErrValueType = errors.New("invalid value type")

// JSONLimit is the byte limit on a body held for whole-body JSON
// filtering. A body over it fails the flow instead of exhausting
// memory.
const JSONLimit = 16 * (1 << 20) // 16 MiB

// Kind is the value kind a JSON body holds, recorded from its first
// rune at birth.
type Kind byte

const (
	NoKind Kind = iota
	DictKind
	ListKind
	StringKind
	NumberKind
	BoolKind
	NullKind
)

// JSONValue is one request or response body held through a flow's
// Body filter stages: the raw bytes until the first filter asks for
// the value, then a single starlark value that every filter works
// on in place - a dict or a list mutates, the scalar kinds only
// observe. Each form is produced at most once, and only when asked
// for: a body crossing any number of filtering stages is decoded
// once and encoded once.
type JSONValue struct {
	// raw is the body's byte form; nil once the value took over,
	// or after a Bytes() re-encode left it paired with the value.
	raw []byte
	// value is the body's decoded form, once a filter decoded it.
	value starlark.Value
	// kind is the kind of the first rune of the raw body,
	// recorded at birth. NoKind short-circuits the decode.
	kind Kind
	// err is the terminal failure, if any: a decode over the
	// limit, or an unencodable value. Once set, the value stops
	// working.
	err error
}

// NewJSONValue holds one body's bytes, noting the kind its first
// rune promises.
func NewJSONValue(raw []byte) *JSONValue {
	v := &JSONValue{raw: raw}
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '{':
			v.kind = DictKind
		case '[':
			v.kind = ListKind
		case '"':
			v.kind = StringKind
		case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			v.kind = NumberKind
		case 't', 'f':
			v.kind = BoolKind
		case 'n':
			v.kind = NullKind
		}
		break
	}
	return v
}

// Kind returns the value kind of the first rune. It doesn't
// guarantee the value is valid.
func (v *JSONValue) Kind() Kind {
	return v.kind
}

// Value returns the body's decoded form, decoding the raw bytes on
// the first call only. A body with no decoded form - not JSON, or
// JSON that does not parse to exactly one value - answers
// ErrValueType, without storing it: the body still passes through
// untouched. A terminal failure is stored and answered from then on.
func (v *JSONValue) Value() (starlark.Value, error) {
	if v.err != nil {
		return nil, v.err
	}
	if v.value != nil {
		v.raw = nil
		return v.value, nil
	}
	if v.kind == NoKind {
		return nil, ErrValueType
	}
	if len(v.raw) > JSONLimit {
		v.err = fmt.Errorf("json body of %d bytes exceeds the %d byte limit", len(v.raw), JSONLimit)
		return nil, v.err
	}
	value, err := scripts.DecodeJSON(v.raw)
	if err != nil || value == nil {
		return nil, ErrValueType
	}
	v.value = value
	v.raw = nil
	return value, nil
}

// Dict returns the body as a dict, failing with ErrValueType when it
// is any other kind. See Value for the decoding rules.
func (v *JSONValue) Dict() (*starlark.Dict, error) {
	value, err := v.Value()
	if err != nil {
		return nil, err
	}
	dict, ok := value.(*starlark.Dict)
	if !ok {
		return nil, ErrValueType
	}
	return dict, nil
}

// List returns the body as a list, failing with ErrValueType when it
// is any other kind. See Value for the decoding rules.
func (v *JSONValue) List() (*starlark.List, error) {
	value, err := v.Value()
	if err != nil {
		return nil, err
	}
	list, ok := value.(*starlark.List)
	if !ok {
		return nil, ErrValueType
	}
	return list, nil
}

// String returns the body as a string, failing with ErrValueType
// when it is any other kind. See Value for the decoding rules.
func (v *JSONValue) String() (string, error) {
	value, err := v.Value()
	if err != nil {
		return "", err
	}
	str, ok := starlark.AsString(value)
	if !ok {
		return "", ErrValueType
	}
	return str, nil
}

// Number returns the body as a number, failing with ErrValueType
// when it is any other kind. See Value for the decoding rules.
func (v *JSONValue) Number() (starlark.Value, error) {
	value, err := v.Value()
	if err != nil {
		return nil, err
	}
	switch value.(type) {
	case starlark.Int, starlark.Float:
		return value, nil
	}
	return nil, ErrValueType
}

// Bool returns the body as a bool, failing with ErrValueType when it
// is any other kind. See Value for the decoding rules.
func (v *JSONValue) Bool() (bool, error) {
	value, err := v.Value()
	if err != nil {
		return false, err
	}
	b, ok := value.(starlark.Bool)
	if !ok {
		return false, ErrValueType
	}
	return bool(b), nil
}

// Null reports the body being null, failing with ErrValueType when
// it is any other kind. See Value for the decoding rules.
func (v *JSONValue) Null() error {
	value, err := v.Value()
	if err != nil {
		return err
	}
	if _, ok := value.(starlark.NoneType); !ok {
		return ErrValueType
	}
	return nil
}

// Bytes returns the body's byte form, encoding the value on the first
// call only. The value stays: encoding does not change it, so a later
// Value still answers from it.
func (v *JSONValue) Bytes() ([]byte, error) {
	if v.err != nil {
		return nil, v.err
	}
	if v.raw != nil {
		return v.raw, nil
	}
	raw, err := scripts.EncodeJSON(v.value)
	if err != nil {
		v.err = err
		return nil, err
	}
	v.raw = raw
	return v.raw, nil
}

// Set replaces the value, clearing the byte form so the next Bytes
// re-encodes. A terminal failure stays terminal.
func (v *JSONValue) Set(value starlark.Value) {
	if v.err != nil {
		return
	}
	v.value = value
	v.raw = nil
}

// SetRaw replaces the raw bytes, clearing the decoded form and
// re-sensing the kind. A terminal failure stays terminal.
func (v *JSONValue) SetRaw(raw []byte) {
	if v.err != nil {
		return
	}
	*v = *NewJSONValue(raw)
}

// Err reports the terminal failure, if any.
func (v *JSONValue) Err() error {
	return v.err
}

// StreamEvent is one SSE event's fields beside its data: carried by
// the bodyValue of a streamed event, shared by every snippet in the
// chain over that event. Retry stays a string here - the decimal
// milliseconds off the wire - decoded to an int at the value.
// Assigning an empty string drops a field.
type StreamEvent struct {
	Event string
	ID    string
	Retry string
}

// bodyValue is one body or event handed to a filter as its argument:
// the JSONValue wrapped, its two forms exposed as attributes. The
// data field is the raw bytes; the json field the decoded form,
// produced on first read, None when the bytes are not JSON. Either
// form assigns to replace the payload. The event pointer keys the
// surface: nil is a body, with data and json alone; set is an
// event, gaining name, id, and retry - name reading "message"
// when the event carries no event line, without writing it back,
// so an untouched event crosses as-is.
type bodyValue struct {
	value  *JSONValue
	event  *StreamEvent
	frozen bool
}

func (v *bodyValue) String() string { return v.kindName() }
func (v *bodyValue) Type() string   { return v.kindName() }

func (v *bodyValue) kindName() string {
	if v.event != nil {
		return "event"
	}
	return "body"
}

func (v *bodyValue) Freeze()               { v.frozen = true }
func (v *bodyValue) Truth() starlark.Bool  { return starlark.True }
func (v *bodyValue) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable type: %s", v.kindName()) }

func (v *bodyValue) AttrNames() []string {
	if v.event == nil {
		return []string{"data", "json"}
	}
	return []string{"data", "json", "name", "id", "retry"}
}

func (v *bodyValue) Attr(name string) (starlark.Value, error) {
	switch name {
	case "data":
		raw, err := v.value.Bytes()
		if err != nil {
			return nil, err
		}
		return starlark.String(raw), nil
	case "json":
		decoded, err := v.value.Value()
		if err != nil {
			if errors.Is(err, ErrValueType) {
				return starlark.None, nil
			}
			return nil, err
		}
		return decoded, nil
	case "name", "id", "retry":
		if v.event == nil {
			return nil, nil
		}
		switch name {
		case "name":
			if v.event.Event == "" {
				// The spec's default, read-only: an untouched
				// event keeps its absent line.
				return starlark.String("message"), nil
			}
			return starlark.String(v.event.Event), nil
		case "id":
			return starlark.String(v.event.ID), nil
		}
		if v.event.Retry == "" {
			return starlark.None, nil
		}
		ms, err := strconv.ParseInt(v.event.Retry, 10, 64)
		if err != nil {
			return starlark.None, nil
		}
		return starlark.MakeInt64(ms), nil
	}
	return nil, nil
}

// SetField lands the assignment forms: json = v replaces the value,
// data = s the bytes, and on an event the fields assign - name and
// id as strings, retry as an int, None or an empty string dropping
// the field. Assignment is the replacement workflow - a snippet's
// return value is ignored. Assigning None to json writes a JSON
// null, mirroring the decoder: a null payload reads back as None.
func (v *bodyValue) SetField(name string, val starlark.Value) error {
	if v.frozen {
		return fmt.Errorf("cannot set %s.%s: %s is frozen", v.kindName(), name, v.kindName())
	}
	switch name {
	case "json":
		v.value.Set(val)
		return nil
	case "data":
		raw, ok := starlark.AsString(val)
		if !ok {
			return fmt.Errorf("cannot set %s.data: got %s, want string", v.kindName(), val.Type())
		}
		v.value.SetRaw([]byte(raw))
		return nil
	case "name", "id", "retry":
		if v.event == nil {
			return fmt.Errorf("%s has no .%s field or method", v.kindName(), name)
		}
		if name == "retry" {
			if val == starlark.None {
				v.event.Retry = ""
				return nil
			}
			ms, ok := val.(starlark.Int)
			if !ok {
				return fmt.Errorf("cannot set event.retry: got %s, want int", val.Type())
			}
			v.event.Retry = ms.String()
			return nil
		}
		str, ok := starlark.AsString(val)
		if !ok {
			return fmt.Errorf("cannot set event.%s: got %s, want string", name, val.Type())
		}
		if name == "name" {
			v.event.Event = str
		} else {
			v.event.ID = str
		}
		return nil
	}
	return fmt.Errorf("%s has no .%s field or method", v.kindName(), name)
}

// filterJSON runs one sniplet's body or event filter on the shared
// value: the snippet takes it as its named argument - body or event -
// and works on it - mutating json in place, or assigning json or
// data to replace it. A payload with no decoded form reads json as
// None and passes through unless the snippet assigns. An event
// carries its fields through the event pointer. The memory limit
// is enforced where the proxy buffers; the decode here is the
// backstop.
func (p *Program) filterJSON(flow *Flow, sniplet setup.Sniplet, field, arg string, value *JSONValue, event *StreamEvent) error {
	if p == nil || flow == nil || value == nil || !p.has(sniplet, field) {
		return nil
	}
	_, err := p.call(flow, sniplet, field, arg, &bodyValue{value: value, event: event})
	return err
}
