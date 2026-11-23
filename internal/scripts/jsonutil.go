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

package scripts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"

	"github.com/canonical/rebear/internal/plainjson"
	"go.starlark.net/starlark"
)

// DecodeJSON parses JSON text into one starlark value, failing when
// the bytes are not exactly one JSON value.
func DecodeJSON(body []byte) (starlark.Value, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	value, err := readJSON(dec)
	if err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data after json value")
	}
	return value, nil
}

// readJSON reads one JSON value: an object becomes a dict (key order kept,
// so an untouched body re-encodes unchanged), an array a list, and numbers
// become ints when they were written without a fraction or exponent.
func readJSON(dec *json.Decoder) (starlark.Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch value := tok.(type) {
	case json.Delim:
		switch value {
		case '{':
			dict := starlark.NewDict(0)
			for dec.More() {
				name, err := dec.Token()
				if err != nil {
					return nil, err
				}
				item, err := readJSON(dec)
				if err != nil {
					return nil, err
				}
				if err := dict.SetKey(starlark.String(name.(string)), item); err != nil {
					return nil, err
				}
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return dict, nil
		case '[':
			list := starlark.NewList(nil)
			for dec.More() {
				item, err := readJSON(dec)
				if err != nil {
					return nil, err
				}
				if err := list.Append(item); err != nil {
					return nil, err
				}
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return list, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %s", value)
	case json.Number:
		return numberValue(string(value)), nil
	case string:
		return starlark.String(value), nil
	case bool:
		return starlark.Bool(value), nil
	case nil:
		return starlark.None, nil
	}
	return nil, fmt.Errorf("unexpected token %v", tok)
}

// numberValue converts a JSON number to an int when it was written as one,
// or to a float otherwise.
func numberValue(text string) starlark.Value {
	if i, err := strconv.ParseInt(text, 10, 64); err == nil {
		return starlark.MakeInt64(i)
	}
	if i, ok := new(big.Int).SetString(text, 10); ok {
		return starlark.MakeBigInt(i)
	}
	f, _ := strconv.ParseFloat(text, 64)
	return starlark.Float(f)
}

// encodeJSON writes a starlark value back as JSON text, keeping dict
// key order and the plain escapes: no html escaping, so a rewritten
// body keeps the bytes the wire holds. Any value the text cannot hold
// - a function, a non-string key - fails instead of encoding.
func EncodeJSON(v starlark.Value) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeJSON(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodeJSON writes a starlark value back as JSON text, keeping dict key
// order. Any value the text cannot hold - a function, a non-string key -
// fails instead of encoding.
func encodeJSON(buf *bytes.Buffer, v starlark.Value) error {
	switch value := v.(type) {
	case starlark.NoneType:
		buf.WriteString("null")
	case starlark.Bool:
		if value {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case starlark.Int:
		buf.WriteString(value.String())
	case starlark.Float:
		encoded, err := plainjson.Marshal(float64(value))
		if err != nil {
			return err
		}
		buf.Write(encoded)
	case starlark.String:
		encoded, err := plainjson.Marshal(string(value))
		if err != nil {
			return err
		}
		buf.Write(encoded)
	case *starlark.List:
		buf.WriteString("[")
		for i := 0; i < value.Len(); i++ {
			if i > 0 {
				buf.WriteString(",")
			}
			if err := encodeJSON(buf, value.Index(i)); err != nil {
				return err
			}
		}
		buf.WriteString("]")
	case *starlark.Dict:
		buf.WriteString("{")
		for i, item := range value.Items() {
			if i > 0 {
				buf.WriteString(",")
			}
			name, ok := starlark.AsString(item[0])
			if !ok {
				return fmt.Errorf("dict key %s is not a string", item[0].String())
			}
			encoded, err := plainjson.Marshal(name)
			if err != nil {
				return err
			}
			buf.Write(encoded)
			buf.WriteString(":")
			if err := encodeJSON(buf, item[1]); err != nil {
				return err
			}
		}
		buf.WriteString("}")
	default:
		return fmt.Errorf("cannot encode %s as json", v.String())
	}
	return nil
}
