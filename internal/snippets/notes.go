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

	"github.com/canonical/rebear/internal/notes"
	"github.com/canonical/rebear/internal/scripts"
	"go.starlark.net/starlark"
)

// notesValue is the notes argument of a closing-flow snippet: the
// global notes map, wrapped so the first use acquires the store -
// no other snippet can change or observe the notes until this one
// ends - and the release at the end encodes the map back into the
// canonical state. The map operations delegate to the genuine
// dict: the full native api, one reference shared by everything.
type notesValue struct {
	store *notes.Store
	flow  *Flow
	// acquired marks the store taken: a snippet never touching
	// the notes acquires nothing.
	acquired bool
	// data is the decoded map, one per snippet.
	data *starlark.Dict
	// original is the JSON string Acquire answered: the state
	// to fall back to when the release cannot encode the map.
	original string
}

func (v *notesValue) String() string       { return "notes" }
func (v *notesValue) Type() string         { return "notes" }
func (v *notesValue) Freeze()              {}
func (v *notesValue) Truth() starlark.Bool { return starlark.True }
func (v *notesValue) Hash() (uint32, error) {
	return 0, fmt.Errorf("unhashable type: notes")
}

// dict acquires the store on first use and answers the decoded
// map. A state that does not decode logs on the flow's entry and
// answers an empty map: the snippet did nothing wrong, but it is
// the only witness. No store is a map of the value's own, kept
// for the snippet's span and thrown away with it.
func (v *notesValue) dict() *starlark.Dict {
	if v.data == nil {
		if v.store == nil {
			v.data = starlark.NewDict(0)
			return v.data
		}
		v.original = v.store.Acquire()
		v.acquired = true
		value, err := scripts.DecodeJSON([]byte(v.original))
		if err != nil {
			v.data = starlark.NewDict(0)
			if v.flow.entry != nil {
				v.flow.entry.Log(map[string]any{"error": "notes: cannot decode: " + err.Error()})
			}
			return v.data
		}
		dict, ok := value.(*starlark.Dict)
		if !ok {
			v.data = starlark.NewDict(0)
			if v.flow.entry != nil {
				v.flow.entry.Log(map[string]any{"error": "notes: state is not a json object"})
			}
			return v.data
		}
		v.data = dict
	}
	return v.data
}

// release ends the snippet's hold: the map encodes back into the
// canonical state - a value the state cannot hold logged on the
// flow's entry, the snippet's changes lost, the original state
// kept - and the store opens. A snippet never touching the notes
// releases nothing.
func (v *notesValue) release() {
	if !v.acquired {
		return
	}
	state := v.original
	if v.data != nil {
		if encoded, err := scripts.EncodeJSON(v.data); err == nil {
			state = string(encoded)
		} else if v.flow.entry != nil {
			v.flow.entry.Log(map[string]any{"error": "notes: cannot encode: " + err.Error()})
		}
	}
	v.acquired = false
	v.store.Release(state)
}

// Get answers the map lookup, the in operator included.
func (v *notesValue) Get(k starlark.Value) (starlark.Value, bool, error) {
	return v.dict().Get(k)
}

// SetKey answers the index assignment.
func (v *notesValue) SetKey(k, val starlark.Value) error {
	return v.dict().SetKey(k, val)
}

// Iterate answers the loop over the keys.
func (v *notesValue) Iterate() starlark.Iterator {
	return v.dict().Iterate()
}

// Len answers the len call.
func (v *notesValue) Len() int {
	return v.dict().Len()
}

// Attr answers the dict's own methods - get, pop, update, and
// the rest - bound to the genuine map.
func (v *notesValue) Attr(name string) (starlark.Value, error) {
	return v.dict().Attr(name)
}

// AttrNames lists the dict's own method names.
func (v *notesValue) AttrNames() []string {
	return v.dict().AttrNames()
}
