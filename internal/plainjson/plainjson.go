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

// Package plainjson is the one json marshal form rebear writes:
// a string escapes only what json itself requires, so a Go
// html escape - the \u0026 for & - never lands in a written
// byte. The log stream, the snapshot dumps, and the bodies a
// snippet rewrote all marshal through this form.
package plainjson

import (
	"bytes"
	"encoding/json"
)

// Marshal encodes like json.Marshal with the html escaping off:
// an &, <, or > rides raw, and every json-mandated escape stays
// - a newline included, so a written line stays one line. The
// newline Encode appends is dropped, so the bytes hold the
// value alone.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}
