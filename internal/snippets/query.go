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
	"sort"

	"go.starlark.net/starlark"
)

// queryValue is the outgoing query as a starlark value: a dict
// over the first item of each parameter, with the getn family for
// the whole values list. Reading never rewrites the wire form -
// only a mutation marks the query updated, handing the encoding
// over to url.Values.
type queryValue struct {
	flow *Flow
}

func (v *queryValue) String() string       { return "query" }
func (v *queryValue) Type() string         { return "query" }
func (v *queryValue) Freeze()              {}
func (v *queryValue) Truth() starlark.Bool { return starlark.True }
func (v *queryValue) Hash() (uint32, error) {
	return 0, fmt.Errorf("unhashable type: query")
}

// AttrNames lists the query's methods.
func (v *queryValue) AttrNames() []string {
	return []string{"get", "getn", "keys", "pop", "popn", "popitem", "popitemn", "setdefault", "update", "values"}
}

// Attr answers the query's methods, the dict interface over the
// first items plus the n variants over the whole values lists.
func (v *queryValue) Attr(name string) (starlark.Value, error) {
	switch name {
	case "get":
		return starlark.NewBuiltin("get", v.get), nil
	case "getn":
		return starlark.NewBuiltin("getn", v.getn), nil
	case "keys":
		return starlark.NewBuiltin("keys", v.keys), nil
	case "pop":
		return starlark.NewBuiltin("pop", v.pop), nil
	case "popn":
		return starlark.NewBuiltin("popn", v.popn), nil
	case "popitem":
		return starlark.NewBuiltin("popitem", v.popitem), nil
	case "popitemn":
		return starlark.NewBuiltin("popitemn", v.popitemn), nil
	case "setdefault":
		return starlark.NewBuiltin("setdefault", v.setdefault), nil
	case "update":
		return starlark.NewBuiltin("update", v.update), nil
	case "values":
		return starlark.NewBuiltin("values", v.values), nil
	}
	return nil, nil
}

// Get answers the index read and the in operator: the first
// value of the parameter.
func (v *queryValue) Get(k starlark.Value) (starlark.Value, bool, error) {
	key, ok := starlark.AsString(k)
	if !ok {
		return nil, false, fmt.Errorf("query key must be string, got %s", k.Type())
	}
	if values := v.flow.OutQuery()[key]; len(values) > 0 {
		return starlark.String(values[0]), true, nil
	}
	return nil, false, nil
}

// SetKey answers the index assignment: a string sets the single
// value, a list or tuple of strings the whole values list.
func (v *queryValue) SetKey(k, val starlark.Value) error {
	if err := v.mutable(); err != nil {
		return err
	}
	key, ok := starlark.AsString(k)
	if !ok {
		return fmt.Errorf("query key must be string, got %s", k.Type())
	}
	return setQueryValue(v.flow.OutQuery(), key, val, v.flow)
}

// Iterate answers the loop over the keys, sorted as Encode
// writes them.
func (v *queryValue) Iterate() starlark.Iterator {
	return starlark.NewList(v.keysList()).Iterate()
}

// Len answers the len call.
func (v *queryValue) Len() int {
	return len(v.flow.OutQuery())
}

// mutable checks if mutations are allowed.
func (v *queryValue) mutable() error {
	if v.flow.outHeaders == nil {
		return fmt.Errorf("cannot mutate outgoing.query: no request")
	}
	if v.flow.stage != outgoingStage {
		return fmt.Errorf("cannot mutate outgoing.query: request already sent")
	}
	return nil
}

// keysList answers the keys sorted: the order the encoding
// writes them, so what the snippet sees is what gets sent.
func (v *queryValue) keysList() []starlark.Value {
	query := v.flow.OutQuery()
	keys := make([]starlark.Value, 0, len(query))
	for key := range query {
		keys = append(keys, starlark.String(key))
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].(starlark.String) < keys[j].(starlark.String)
	})
	return keys
}

// get answers the first value, or the default on a miss.
func (v *queryValue) get(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var key string
	var dflt starlark.Value = starlark.None
	if err := starlark.UnpackPositionalArgs("get", args, kwargs, 1, &key, &dflt); err != nil {
		return nil, err
	}
	if values := v.flow.OutQuery()[key]; len(values) > 0 {
		return starlark.String(values[0]), nil
	}
	return dflt, nil
}

// getn answers the whole values list, empty on a miss.
func (v *queryValue) getn(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var key string
	if err := starlark.UnpackPositionalArgs("getn", args, kwargs, 1, &key); err != nil {
		return nil, err
	}
	return valuesList(v.flow.OutQuery()[key]), nil
}

// keys answers the keys, sorted.
func (v *queryValue) keys(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackPositionalArgs("keys", args, kwargs, 0); err != nil {
		return nil, err
	}
	return starlark.NewList(v.keysList()), nil
}

// values answers the first value of each key, sorted to match
// keys.
func (v *queryValue) values(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackPositionalArgs("values", args, kwargs, 0); err != nil {
		return nil, err
	}
	query := v.flow.OutQuery()
	values := make([]starlark.Value, 0, len(query))
	for _, key := range v.keysList() {
		values = append(values, starlark.String(query[string(key.(starlark.String))][0]))
	}
	return starlark.NewList(values), nil
}

// pop removes the key and answers its first value - or the
// default on a miss, and a failure without one.
func (v *queryValue) pop(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	values, found, err := v.popValues("pop", args, kwargs)
	if err != nil {
		return nil, err
	}
	if !found {
		if len(args) == 2 {
			return args[1], nil
		}
		return nil, fmt.Errorf("pop: missing key")
	}
	return starlark.String(values[0]), nil
}

// popn removes the key and answers its whole values list - or
// the default on a miss, and a failure without one.
func (v *queryValue) popn(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	values, found, err := v.popValues("popn", args, kwargs)
	if err != nil {
		return nil, err
	}
	if !found {
		if len(args) == 2 {
			return args[1], nil
		}
		return nil, fmt.Errorf("popn: missing key")
	}
	return valuesList(values), nil
}

// popValues lands the shared pop body: the values list of the
// removed key, found=false on a miss.
func (v *queryValue) popValues(name string, args starlark.Tuple, kwargs []starlark.Tuple) ([]string, bool, error) {
	if err := v.mutable(); err != nil {
		return nil, false, err
	}
	if len(kwargs) != 0 {
		return nil, false, fmt.Errorf("%s: unexpected keyword arguments", name)
	}
	if len(args) < 1 || len(args) > 2 {
		return nil, false, fmt.Errorf("%s: got %d arguments, want 1 or 2", name, len(args))
	}
	key, ok := starlark.AsString(args[0])
	if !ok {
		return nil, false, fmt.Errorf("%s: key must be string, got %s", name, args[0].Type())
	}
	query := v.flow.OutQuery()
	values, found := query[key]
	if found {
		delete(query, key)
		v.flow.updated = true
	}
	return values, found, nil
}

// popitem removes the first sorted key and answers the pair of
// it and its first value.
func (v *queryValue) popitem(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	key, values, err := v.popItemValues("popitem")
	if err != nil {
		return nil, err
	}
	return starlark.Tuple{starlark.String(key), starlark.String(values[0])}, nil
}

// popitemn removes the first sorted key and answers the pair of
// it and its whole values list.
func (v *queryValue) popitemn(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	key, values, err := v.popItemValues("popitemn")
	if err != nil {
		return nil, err
	}
	return starlark.Tuple{starlark.String(key), valuesList(values)}, nil
}

// popItemValues lands the shared popitem body: the first sorted
// key and its values list, removed.
func (v *queryValue) popItemValues(name string) (string, []string, error) {
	if err := v.mutable(); err != nil {
		return "", nil, err
	}
	keys := v.keysList()
	if len(keys) == 0 {
		return "", nil, fmt.Errorf("%s: empty query", name)
	}
	key := string(keys[0].(starlark.String))
	query := v.flow.OutQuery()
	values := query[key]
	delete(query, key)
	v.flow.updated = true
	return key, values, nil
}

// setdefault answers the first value of a present key, and sets
// and answers the default on a miss.
func (v *queryValue) setdefault(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var key string
	var dflt string
	if err := starlark.UnpackPositionalArgs("setdefault", args, kwargs, 1, &key, &dflt); err != nil {
		return nil, err
	}
	if values := v.flow.OutQuery()[key]; len(values) > 0 {
		return starlark.String(values[0]), nil
	}
	if err := v.mutable(); err != nil {
		return nil, err
	}
	v.flow.OutQuery()[key] = []string{dflt}
	v.flow.updated = true
	return starlark.String(dflt), nil
}

// update lands many assignments at once: a dict or a list of
// pairs, plus the keyword arguments.
func (v *queryValue) update(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := v.mutable(); err != nil {
		return nil, err
	}
	if len(args) > 1 {
		return nil, fmt.Errorf("update: got %d arguments, want at most 1", len(args))
	}
	query := v.flow.OutQuery()
	if len(args) == 1 {
		switch src := args[0].(type) {
		case *starlark.Dict:
			for _, item := range src.Items() {
				key, ok := starlark.AsString(item[0])
				if !ok {
					return nil, fmt.Errorf("update: key must be string, got %s", item[0].Type())
				}
				if err := setQueryValue(query, key, item[1], v.flow); err != nil {
					return nil, err
				}
			}
		case starlark.Iterable:
			iter := src.Iterate()
			defer iter.Done()
			var item starlark.Value
			for iter.Next(&item) {
				pair, ok := item.(starlark.Indexable)
				if !ok || pair.Len() != 2 {
					return nil, fmt.Errorf("update: want a list of pairs, got %s", item.Type())
				}
				key, ok := starlark.AsString(pair.Index(0))
				if !ok {
					return nil, fmt.Errorf("update: key must be string, got %s", pair.Index(0).Type())
				}
				if err := setQueryValue(query, key, pair.Index(1), v.flow); err != nil {
					return nil, err
				}
			}
		default:
			return nil, fmt.Errorf("update: got %s, want dict or list of pairs", args[0].Type())
		}
	}
	for _, kw := range kwargs {
		key, ok := starlark.AsString(kw[0])
		if !ok {
			return nil, fmt.Errorf("update: key must be string, got %s", kw[0].Type())
		}
		if err := setQueryValue(query, key, kw[1], v.flow); err != nil {
			return nil, err
		}
	}
	v.flow.updated = true
	return starlark.None, nil
}

// setQueryValue lands one assignment: a string becomes the
// single value, a list or tuple of strings the whole values list.
func setQueryValue(query map[string][]string, key string, val starlark.Value, flow *Flow) error {
	switch val := val.(type) {
	case starlark.String:
		query[key] = []string{string(val)}
	case starlark.Indexable:
		values := make([]string, 0, val.Len())
		for i := 0; i < val.Len(); i++ {
			s, ok := starlark.AsString(val.Index(i))
			if !ok {
				return fmt.Errorf("query values must be strings, got %s", val.Index(i).Type())
			}
			values = append(values, s)
		}
		query[key] = values
	default:
		return fmt.Errorf("query value must be string or list of strings, got %s", val.Type())
	}
	flow.updated = true
	return nil
}

// valuesList copies a values list into its starlark form, an
// empty list answering for none.
func valuesList(values []string) starlark.Value {
	list := make([]starlark.Value, 0, len(values))
	for _, value := range values {
		list = append(list, starlark.String(value))
	}
	return starlark.NewList(list)
}
