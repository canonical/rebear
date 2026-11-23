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
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

func TestRoundTrip(t *testing.T) {
	// A decoded value re-encodes to the same text: key order
	// kept, numbers kept as written.
	// A decoded value re-encodes to the same text: key order
	// kept, numbers kept as written, strings kept plain - an
	// & round trips raw, no html escape landing.
	testCases := []string{
		`{}`,
		`{"a":1}`,
		`{"z":1,"a":{"z":[true,null,"x"],"a":2.5},"n":-3}`,
		`{"q":"poll=1&since=fuFmZm33sr"}`,
		`{"a&b":1}`,
		`[1,2.5,"three",true,null,{"k":"v"}]`,
		`"text"`,
		`null`,
		`true`,
		`123456789012345678901234567890`,
	}
	for _, tc := range testCases {
		value, err := DecodeJSON([]byte(tc))
		if err != nil {
			t.Fatalf("DecodeJSON(%s): unexpected error: %v", tc, err)
		}
		encoded, err := EncodeJSON(value)
		if err != nil {
			t.Fatalf("EncodeJSON(%s): unexpected error: %v", tc, err)
		}
		if string(encoded) != tc {
			t.Errorf("round trip of %s = %s, want it unchanged", tc, encoded)
		}
	}
}

func TestDecodeErrors(t *testing.T) {
	// Only exactly one JSON value decodes.
	testCases := []string{
		``,
		`{`,
		`nul`,
		`{} {}`,
		`{"a":} `,
		`[1,]`,
	}
	for _, tc := range testCases {
		if _, err := DecodeJSON([]byte(tc)); err == nil {
			t.Errorf("DecodeJSON(%q): error = nil, want a failure", tc)
		}
	}
}

func TestEncodeErrors(t *testing.T) {
	// A value the text cannot hold fails instead of encoding.
	dict := starlark.NewDict(1)
	if err := dict.SetKey(starlark.MakeInt(1), starlark.String("v")); err != nil {
		t.Fatalf("cannot build dict: %v", err)
	}
	testCases := []struct {
		value starlark.Value
		want  string
	}{
		{starlark.NewBuiltin("f", nil), "cannot encode"},
		{dict, "not a string"},
	}
	for _, tc := range testCases {
		encoded, err := EncodeJSON(tc.value)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("EncodeJSON(%s): error = %v, want the %q failure", tc.value.String(), err, tc.want)
		}
		if encoded != nil {
			t.Errorf("EncodeJSON(%s): bytes = %s, want none", tc.value.String(), encoded)
		}
	}
}
