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

package plainjson

import (
	"strings"
	"testing"
)

func TestMarshal(t *testing.T) {
	// The html escaping is off: &, <, and > ride raw, so a
	// written byte holds what the wire holds. Everything json
	// mandates escapes still, an encoded wire form included.
	testCases := []struct {
		value any
		want  string
	}{
		{"poll=1&since=x", `"poll=1&since=x"`},
		{"a=b%26c", `"a=b%26c"`},
		{"x < y > z", `"x < y > z"`},
		{`quote " and \ slash`, `"quote \" and \\ slash"`},
		{"new\nline", `"new\nline"`},
		{"tab\tcarriage\rreturn", `"tab\tcarriage\rreturn"`},
		{map[string]any{"k": "<v&>"}, `{"k":"<v&>"}`},
		{[]any{1, "&", true}, `[1,"&",true]`},
	}
	for _, tc := range testCases {
		got, err := Marshal(tc.value)
		if err != nil {
			t.Fatalf("Marshal(%v): unexpected error: %v", tc.value, err)
		}
		if string(got) != tc.want {
			t.Errorf("Marshal(%v) = %s, want %s", tc.value, got, tc.want)
		}
	}
}

func TestMarshalUnchangedBytes(t *testing.T) {
	// The output holds the value alone: no newline around it,
	// so a caller writes its own separator. Marshal fails on a
	// value json cannot hold, the same way json.Marshal does.
	got, err := Marshal("x")
	if err != nil {
		t.Fatalf("Marshal: unexpected error: %v", err)
	}
	if strings.Contains(string(got), "\n") {
		t.Errorf("Marshal = %q, want no newline", got)
	}
	if _, err := Marshal(make(chan int)); err == nil {
		t.Errorf("Marshal(chan): error = nil, want a failure")
	}
}
